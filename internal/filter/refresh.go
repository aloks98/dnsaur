package filter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/aloks98/dnsaur/internal/store"
)

// maxListBytes caps one downloaded list. The body goes straight into
// data_dir and is then turned into a trie, so an endless (or merely absurd)
// response would fill the disk and then the heap. 64 MiB is roughly six
// times the largest list anyone actually publishes.
const maxListBytes = 64 << 20

type Refresher struct {
	fs      store.FilterStore
	cs      store.ClientStore
	eng     *Engine
	dataDir string
	hc      *http.Client
	now     func() time.Time

	// mu serializes RefreshAll and Recompile runs. Without it, the
	// initial-load goroutine, the periodic ticker, the settings-change
	// watcher (see internal/app) and every API write can invoke them
	// concurrently, letting their list-cache .tmp writes and
	// compiled-ruleset swaps interleave.
	mu sync.Mutex

	// nextRefresh is unix ms of the periodic download's next tick, 0 when
	// no cadence is running. Atomic rather than under mu: mu is held for
	// the whole of a refresh round, and a dashboard asking when the next
	// one is due must not wait out a download to be told.
	nextRefresh atomic.Int64
}

// RefresherOption adjusts a Refresher at construction.
type RefresherOption func(*Refresher)

// AllowLoopbackTargets lets this refresher fetch lists from loopback, which
// it otherwise refuses along with every other non-public address. It exists
// for tests that serve a list from httptest; nothing in cmd/ passes it, and
// it widens nothing else — a redirect from that loopback server to a private
// or link-local address is still refused.
func AllowLoopbackTargets() RefresherOption {
	return func(r *Refresher) { r.hc.Transport = listTransport(true) }
}

func NewRefresher(fs store.FilterStore, cs store.ClientStore, eng *Engine, dataDir string, opts ...RefresherOption) *Refresher {
	r := &Refresher{fs: fs, cs: cs, eng: eng, dataDir: dataDir,
		hc: &http.Client{Timeout: 60 * time.Second, Transport: listTransport(false)}, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// listTransport refuses to open a connection to an address that is not on
// the public internet. A list URL is admin-supplied but fetched by the
// server itself, from inside the LAN, so without this a subscription aimed
// at 169.254.169.254, at a neighbour's admin port, or at dnsaur's own API
// makes the fetcher a confused deputy.
//
// The check sits in the dialer rather than on the URL on purpose: it sees
// the address actually being connected to, which means every redirect hop is
// checked on its own, and a hostname that resolves to a private address —
// deliberately or by rebinding — is caught at the moment it matters.
func listTransport(allowLoopback bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%s is not a public address", address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicTarget(ip, allowLoopback) {
				return fmt.Errorf("%s is not a public address", host)
			}
			return nil
		},
	}).DialContext
	return t
}

func publicTarget(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return allowLoopback
	}
	return !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

func (r *Refresher) cachePath(id int64) string {
	return filepath.Join(r.dataDir, "lists", fmt.Sprintf("%d.txt", id))
}

// maxReasonLen bounds a stored failure reason. It has to fit a table cell in
// the dashboard and it is partly remote-controlled (a server picks its own
// HTTP reason phrase), so it is truncated rather than trusted.
const maxReasonLen = 160

// reason normalises an error or status into the short, single-line string
// persisted as lists.last_error and rendered verbatim in the UI. Newlines
// would break the row; unbounded length would let a remote server dictate
// how much of the page it gets.
func reason(format string, args ...any) string {
	s := strings.Join(strings.Fields(fmt.Sprintf(format, args...)), " ")
	if len(s) <= maxReasonLen {
		return s
	}
	// Cut on a rune boundary, leaving room for the ellipsis, so the result
	// is valid UTF-8 *and* within the byte budget — the ellipsis is three
	// bytes, and a naive s[:maxReasonLen-1]+"…" overshoots by two.
	cut := maxReasonLen - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// netReason strips the *url.Error wrapper net/http adds to transport errors.
// Its Error() repeats the whole URL ahead of the real cause
// (`Get "https://…": dial tcp: lookup …: no such host`), which in a table
// cell is all prefix and no information — the URL is already the first
// column of the same row.
func netReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return reason("%v", ue.Err)
	}
	return reason("%v", err)
}

// emptyReason explains a list that downloaded and parsed cleanly yet
// produced nothing. The size and the skipped-line count are the two facts
// that separate "the URL serves an HTML error page" from "this is a real
// blocklist in a format the parser rejects" — ParseResult.Skipped was
// already being computed and thrown away.
func emptyReason(size int64, skipped int) string {
	if skipped == 0 {
		return reason("fetched %s, but it contains no domain entries", humanBytes(size))
	}
	return reason("fetched %s, no usable entries — %s lines skipped", humanBytes(size), commas(int64(skipped)))
}

// humanBytes renders a byte count the way an admin would say it aloud.
func humanBytes(n int64) string {
	const unit = 1000 // decimal, to match how these lists advertise their size
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
}

// commas groups an integer in thousands: 250431 → "250,431". A quarter of a
// million skipped lines has to read as one at a glance.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fetchResult is what one download attempt produced. body is always the
// reader to parse — the freshly downloaded copy, or the on-disk cache when
// the network attempt failed. failure is "" only when the network attempt
// itself succeeded (200 or 304); when it is set, cached reports whether a
// usable copy was found anyway, which is the whole Stale-vs-Failed
// distinction.
type fetchResult struct {
	body    io.ReadCloser
	failure string
	cached  bool
	size    int64 // bytes of the copy in body, for the "fetched 4.5 MB but…" message
}

// open wraps os.Open with the file's size, which the parsed-but-empty
// message needs: "no usable entries" is a very different report for a 4.5 MB
// download than for a 0-byte one.
func open(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	var size int64
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}
	return f, size, nil
}

// fetch downloads url honoring a cached ETag; on failure or 304 it falls
// back to the cache file. The fallback is deliberate and unchanged — a
// blocklist that keeps working through a transient outage is the point —
// but every path now names *why* it fell back, so the outcome can be
// persisted instead of only logged.
func (r *Refresher) fetch(ctx context.Context, l store.List) fetchResult {
	return r.fetchOnce(ctx, l, true)
}

// fetchOnce is fetch's body. conditional is false on the one retry fetch
// allows itself: a 304 answering a request we could not satisfy from the
// cache (the cache file went missing under a surviving .etag, or the server
// answers 304 to anything) used to fail as "cache unreadable" on every
// refresh, forever, with no path back.
func (r *Refresher) fetchOnce(ctx context.Context, l store.List, conditional bool) fetchResult {
	// fallback opens the cached copy after a failed network attempt. A
	// missing cache file is not an error to report on its own: `why` is
	// already the real cause, and "no such file" would only describe the
	// symptom.
	fallback := func(why string) fetchResult {
		slog.Warn("list download failed, using cache", "url", l.URL, "reason", why)
		f, size, err := open(r.cachePath(l.ID))
		if err != nil {
			return fetchResult{failure: why}
		}
		return fetchResult{body: f, failure: why, cached: true, size: size}
	}

	etagPath := r.cachePath(l.ID) + ".etag"
	req, err := http.NewRequestWithContext(ctx, "GET", l.URL, nil)
	if err != nil {
		return fallback(reason("bad request: %v", err))
	}
	// Only offer the etag when the copy it describes is still there. An
	// etag outliving its cache file asks the server to confirm a file we
	// cannot read, and a 304 is not a body.
	if conditional {
		if _, serr := os.Stat(r.cachePath(l.ID)); serr == nil {
			if etag, rerr := os.ReadFile(etagPath); rerr == nil {
				req.Header.Set("If-None-Match", string(etag))
			}
		}
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return fallback(netReason(err))
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		if err := os.MkdirAll(filepath.Dir(r.cachePath(l.ID)), 0o755); err != nil {
			return fallback(reason("cache dir: %v", err))
		}
		tmp := r.cachePath(l.ID) + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			return fallback(reason("cache write: %v", err))
		}
		// One byte past the cap, so a body that exactly fills it is still
		// distinguishable from one that ran over.
		n, err := io.Copy(f, io.LimitReader(resp.Body, maxListBytes+1))
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tmp)
			return fallback(reason("download incomplete: %v", err))
		}
		if n > maxListBytes {
			_ = os.Remove(tmp)
			return fallback(reason("list is too large (over %s)", humanBytes(maxListBytes)))
		}
		if err := os.Rename(tmp, r.cachePath(l.ID)); err != nil {
			_ = os.Remove(tmp)
			return fallback(reason("cache write: %v", err))
		}
		if et := resp.Header.Get("ETag"); et != "" {
			_ = os.WriteFile(etagPath, []byte(et), 0o644)
		}
	case http.StatusNotModified:
		// Not a failure: the server confirmed the cached copy is current.
	default:
		// resp.Status is "404 Not Found" — the status line's own reason
		// phrase, which is what an admin recognises. reason() bounds it
		// because the phrase comes from the remote server.
		return fallback(reason("%s", resp.Status))
	}

	// 200 (cache just rewritten) or 304 (cache confirmed current): either
	// way the cache file is the authoritative copy to parse.
	f, size, err := open(r.cachePath(l.ID))
	if err != nil {
		if resp.StatusCode == http.StatusNotModified && conditional {
			// The server confirmed a copy we do not have. Drop the etag
			// that asked the question and take the answer as a body.
			_ = os.Remove(etagPath)
			return r.fetchOnce(ctx, l, false)
		}
		return fetchResult{failure: reason("cache unreadable: %v", err)}
	}
	return fetchResult{body: f, size: size}
}

// compileList turns one list's parsed entries into the set the ruleset
// evaluates.
//
// A block list's own `@@||` exceptions become a companion set rather than
// being dropped: a list that blocks `||example.com^` and exempts
// `@@||cdn.example.com^` means both, and since DomainSet.Match covers
// subdomains, discarding the second one blocked exactly what the list's
// author wrote a line to protect. See Ruleset.Evaluate for where it is
// consulted — the exception excuses its own list, not everyone else's.
func compileList(l store.List, res ParseResult) CompiledList {
	entries, exceptions := res.Block, res.Allow
	if l.Kind == "allow" {
		// A plain domains file subscribed as an allowlist parses into
		// Block; accept both, and there is nothing left to except.
		entries, exceptions = append(res.Allow, res.Block...), nil
	}
	c := CompiledList{ID: l.ID, Kind: l.Kind, Set: NewDomainSet()}
	for _, d := range entries {
		c.Set.Add(d)
	}
	if len(exceptions) > 0 {
		c.Except = NewDomainSet()
		for _, d := range exceptions {
			c.Except.Add(d)
		}
	}
	return c
}

// install compiles each enabled group's ruleset from its rules and the
// subset of compiled it is assigned, then swaps the whole map in.
func (r *Refresher) install(ctx context.Context, compiled map[int64]CompiledList) error {
	groups, err := r.cs.Groups(ctx)
	if err != nil {
		return err
	}
	out := map[int64]*Ruleset{}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		gl, err := r.fs.ListsForGroup(ctx, g.ID)
		if err != nil {
			return err
		}
		var cls []CompiledList
		for _, l := range gl {
			if c, ok := compiled[l.ID]; ok {
				cls = append(cls, c)
			}
		}
		rules, err := r.fs.Rules(ctx, g.ID)
		if err != nil {
			return err
		}
		out[g.ID] = Compile(rules, cls)
	}
	r.eng.SetGroups(out)
	return nil
}

// Recompile rebuilds every group's ruleset from the stored rules and the
// list copies already in the cache directory. It touches no network and
// records no list state — nothing was attempted, so nothing is reported.
//
// This is what every rule, list and assignment write runs, and what Start
// runs before the listeners bind. Fusing it with the download is what made a
// single unreachable URL cost a rule save the full fetch timeout, and what
// left the LAN unfiltered after a restart while usable copies sat on disk.
func (r *Refresher) Recompile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lists, err := r.fs.Lists(ctx)
	if err != nil {
		return err
	}
	compiled := map[int64]CompiledList{}
	for _, l := range lists {
		if !l.Enabled {
			continue
		}
		f, _, err := open(r.cachePath(l.ID))
		if err != nil {
			// Never downloaded, or the cache was cleared. The next
			// download makes a copy; until then this list enforces
			// nothing, which is already what its stored state says.
			continue
		}
		res, perr := ParseList(f)
		_ = f.Close()
		if perr != nil {
			slog.Warn("cached list copy is unparseable", "list", l.ID, "err", perr)
			continue
		}
		compiled[l.ID] = compileList(l, res)
	}
	return r.install(ctx, compiled)
}

// RefreshAll downloads every enabled list into the cache, records what each
// attempt produced, and then compiles — the periodic, manual and
// initial-load path. Callers that only need the ruleset rebuilt want
// Recompile.
func (r *Refresher) RefreshAll(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lists, err := r.fs.Lists(ctx)
	if err != nil {
		return err
	}
	compiled := map[int64]CompiledList{}
	for _, l := range lists {
		if !l.Enabled {
			continue
		}
		if c, ok := r.refreshList(ctx, l); ok {
			compiled[l.ID] = c
		}
	}
	return r.install(ctx, compiled)
}

// refreshList downloads one list, records what the attempt produced, and
// returns what it contributes to a ruleset. ok is false only when there was
// nothing to compile at all — no body and no cache, or a copy that would
// not parse; a stale copy and an empty one both still count, the first
// because it is genuinely still enforcing and the second because an empty
// set is what its row already claims.
//
// Callers hold r.mu.
func (r *Refresher) refreshList(ctx context.Context, l store.List) (CompiledList, bool) {
	at := r.now().UnixMilli()
	fr := r.fetch(ctx, l)
	if fr.body == nil {
		// Nothing to parse and nothing cached: this list is blocking
		// nothing. entry_count 0 is what marks it Failed rather than
		// Stale, and it is also the truth — it contributes no entries
		// to any group's ruleset this round.
		slog.Error("list unavailable", "url", l.URL, "reason", fr.failure)
		logListState(l.ID, r.fs.MarkListFailed(ctx, l.ID, at, 0, fr.failure))
		return CompiledList{}, false
	}
	res, err := ParseList(fr.body)
	_ = fr.body.Close()
	if err != nil {
		// A copy we can't parse is as useless as one we couldn't
		// fetch, even if the download itself was fine.
		why := reason("parse failed: %v", err)
		slog.Error("list parse failed", "url", l.URL, "reason", why)
		logListState(l.ID, r.fs.MarkListFailed(ctx, l.ID, at, 0, why))
		return CompiledList{}, false
	}
	c := compileList(l, res)
	set := c.Set
	switch {
	case fr.failure != "":
		// The fetch failed but a copy parsed. MarkListFailed reads
		// stale (entries still enforcing) or failed (none) from the
		// count.
		slog.Warn("list serving an older copy", "url", l.URL, "reason", fr.failure,
			"from_cache", fr.cached, "entries", set.Len())
		logListState(l.ID, r.fs.MarkListFailed(ctx, l.ID, at, int64(set.Len()), fr.failure))
	case set.Len() == 0:
		// Downloaded and parsed cleanly, and still blocks nothing.
		// This is the wildcard-list case: 4.5 MB of `*.host.tld` that
		// the parser rejected line by line. Reporting it as a plain
		// "0 entries, refreshed just now" is what sends an admin
		// hunting for a download bug that isn't there, so name the
		// parser's side of it explicitly.
		why := emptyReason(fr.size, res.Skipped)
		slog.Warn("list produced no entries", "url", l.URL, "reason", why)
		logListState(l.ID, r.fs.MarkListEmpty(ctx, l.ID, at, why))
	default:
		logListState(l.ID, r.fs.TouchList(ctx, l.ID, at, int64(set.Len())))
	}
	return c, true
}

// RefreshOne downloads exactly one list and then recompiles — the row's own
// "Refresh now", as opposed to RefreshAll's every-subscription pass. Same
// fetch, same size and address limits, same recorded outcome; the
// difference is that no other list is downloaded or re-stated, so one
// unreachable URL elsewhere costs this nothing.
//
// An id naming no list is store.ErrNotFound. A disabled one is still
// fetched if asked — Recompile then ignores the copy, as it ignores every
// disabled list — because the decision about whether that is a sensible
// thing to ask for belongs to the caller, and the API refuses it (see
// handleListRefresh).
func (r *Refresher) RefreshOne(ctx context.Context, id int64) error {
	if err := r.downloadOne(ctx, id); err != nil {
		return err
	}
	return r.Recompile(ctx)
}

// downloadOne is RefreshOne's locked half, split out so the Recompile that
// follows can take r.mu for itself rather than needing it to be reentrant.
func (r *Refresher) downloadOne(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lists, err := r.fs.Lists(ctx)
	if err != nil {
		return err
	}
	for _, l := range lists {
		if l.ID == id {
			r.refreshList(ctx, l)
			return nil
		}
	}
	return store.ErrNotFound
}

// NextRefresh is unix ms of the moment the periodic download will next run,
// or 0 when no cadence is running — nothing configured, or nothing started
// yet. Read by the API so the lists table can say how long the current
// copies have left rather than only how old they are.
func (r *Refresher) NextRefresh() int64 { return r.nextRefresh.Load() }

// logListState surfaces a failed write of a list's own refresh outcome.
// Discarded, it leaves the dashboard row describing the previous round —
// "ok, 99,559 entries" over a list that just 404'd — with nothing anywhere
// saying why.
func logListState(id int64, err error) {
	if err != nil {
		slog.Warn("recording list refresh state failed", "list", id, "err", err)
	}
}

// Run re-downloads and recompiles every enabled list on a fixed cadence,
// until ctx ends.
//
// A non-positive interval means no cadence at all, not a crash:
// time.NewTicker panics on one, and internal/app calls this in a background
// goroutine with nothing to recover it, so a stored lists.refresh_hours of 0
// used to take the process down at every start. Lists still compile on
// demand — the initial load, every settings change and every manual refresh
// call RefreshAll directly — so "no periodic download" is a configuration
// the server can honestly serve, and it says so once rather than silently.
func (r *Refresher) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		slog.Warn("periodic blocklist refresh disabled: the configured interval is not positive", "every", every)
		<-ctx.Done()
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	r.nextRefresh.Store(r.now().Add(every).UnixMilli())
	for {
		select {
		case <-ctx.Done():
			// Nothing is scheduled any more, and a timestamp left behind
			// would keep the dashboard counting down to a tick that will
			// never fire.
			r.nextRefresh.Store(0)
			return
		case <-t.C:
			// Before the download, not after: a ticker's next fire is one
			// period after this one regardless of how long the round takes,
			// and a refresh of a dozen lists can take minutes.
			r.nextRefresh.Store(r.now().Add(every).UnixMilli())
			if err := r.RefreshAll(ctx); err != nil {
				slog.Error("blocklist refresh failed", "err", err)
			}
		}
	}
}
