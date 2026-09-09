package filter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aloks98/dnsaur/internal/store"
)

type Refresher struct {
	fs      store.FilterStore
	cs      store.ClientStore
	eng     *Engine
	dataDir string
	hc      *http.Client
	now     func() time.Time

	// mu serializes RefreshAll runs. Without it, the initial-load goroutine,
	// the periodic ticker, and the settings-change watcher (see internal/app)
	// can invoke RefreshAll concurrently, letting their list-cache .tmp
	// writes and compiled-ruleset swaps interleave.
	mu sync.Mutex
}

func NewRefresher(fs store.FilterStore, cs store.ClientStore, eng *Engine, dataDir string) *Refresher {
	return &Refresher{fs: fs, cs: cs, eng: eng, dataDir: dataDir,
		hc: &http.Client{Timeout: 60 * time.Second}, now: time.Now}
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
	if etag, err := os.ReadFile(etagPath); err == nil {
		req.Header.Set("If-None-Match", string(etag))
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
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fallback(reason("download incomplete: %v", err))
		}
		_ = f.Close()
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
		return fetchResult{failure: reason("cache unreadable: %v", err)}
	}
	return fetchResult{body: f, size: size}
}

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
		at := r.now().UnixMilli()
		fr := r.fetch(ctx, l)
		if fr.body == nil {
			// Nothing to parse and nothing cached: this list is blocking
			// nothing. entry_count 0 is what marks it Failed rather than
			// Stale, and it is also the truth — it contributes no entries
			// to any group's ruleset this round.
			slog.Error("list unavailable", "url", l.URL, "reason", fr.failure)
			_ = r.fs.MarkListFailed(ctx, l.ID, at, 0, fr.failure)
			continue
		}
		res, err := ParseList(fr.body)
		_ = fr.body.Close()
		if err != nil {
			// A copy we can't parse is as useless as one we couldn't
			// fetch, even if the download itself was fine.
			why := reason("parse failed: %v", err)
			slog.Error("list parse failed", "url", l.URL, "reason", why)
			_ = r.fs.MarkListFailed(ctx, l.ID, at, 0, why)
			continue
		}
		set := NewDomainSet()
		entries := res.Block
		if l.Kind == "allow" {
			entries = res.Allow
			// a plain domains file subscribed as an allowlist parses into Block; accept both
			entries = append(entries, res.Block...)
		}
		for _, d := range entries {
			set.Add(d)
		}
		compiled[l.ID] = CompiledList{ID: l.ID, Kind: l.Kind, Set: set}
		if fr.failure != "" {
			// The fetch failed but a copy parsed. MarkListFailed reads
			// stale (entries still enforcing) or failed (none) from the
			// count.
			slog.Warn("list serving an older copy", "url", l.URL, "reason", fr.failure,
				"from_cache", fr.cached, "entries", set.Len())
			_ = r.fs.MarkListFailed(ctx, l.ID, at, int64(set.Len()), fr.failure)
			continue
		}
		if set.Len() == 0 {
			// Downloaded and parsed cleanly, and still blocks nothing.
			// This is the wildcard-list case: 4.5 MB of `*.host.tld` that
			// the parser rejected line by line. Reporting it as a plain
			// "0 entries, refreshed just now" is what sends an admin
			// hunting for a download bug that isn't there, so name the
			// parser's side of it explicitly.
			why := emptyReason(fr.size, res.Skipped)
			slog.Warn("list produced no entries", "url", l.URL, "reason", why)
			_ = r.fs.MarkListEmpty(ctx, l.ID, at, why)
			continue
		}
		_ = r.fs.TouchList(ctx, l.ID, at, int64(set.Len()))
	}
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RefreshAll(ctx); err != nil {
				slog.Error("blocklist refresh failed", "err", err)
			}
		}
	}
}
