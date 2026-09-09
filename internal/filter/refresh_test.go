package filter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// listState is what the store would have persisted for one list after a
// refresh round — the fields the dashboard reads to tell OK from Stale from
// Failed.
type listState struct {
	lastRefreshed int64
	lastAttempt   int64
	entryCount    int64
	status        string
	lastError     string
}

type fakeFilterStore struct {
	lists   []store.List
	touched atomic.Int64

	mu    sync.Mutex
	state map[int64]listState
}

// stateOf returns what the refresher recorded for list id.
func (f *fakeFilterStore) stateOf(id int64) listState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[id]
}

func (f *fakeFilterStore) Lists(ctx context.Context) ([]store.List, error) { return f.lists, nil }
func (f *fakeFilterStore) ListsForGroup(ctx context.Context, g int64) ([]store.List, error) {
	return f.lists, nil
}
func (f *fakeFilterStore) Rules(ctx context.Context, g int64) ([]store.Rule, error) {
	return nil, nil
}
func (f *fakeFilterStore) AddList(ctx context.Context, l store.List) (int64, error) { return 0, nil }
func (f *fakeFilterStore) AssignList(ctx context.Context, g, l int64) error         { return nil }
func (f *fakeFilterStore) ReplaceGroupLists(ctx context.Context, g int64, ids []int64) error {
	return nil
}
func (f *fakeFilterStore) AddRule(ctx context.Context, r store.Rule) (int64, error) { return 0, nil }
func (f *fakeFilterStore) TouchList(ctx context.Context, id, at, n int64) error {
	f.touched.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[int64]listState{}
	}
	// Mirrors the real UPDATE: a success clears last_error.
	f.state[id] = listState{lastRefreshed: at, lastAttempt: at, entryCount: n, status: store.ListStatusOK}
	return nil
}

func (f *fakeFilterStore) MarkListFailed(ctx context.Context, id, at, n int64, why string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[int64]listState{}
	}
	// Mirrors the real UPDATE: last_refreshed is untouched, so a stale list
	// keeps dating the copy it is still serving, and the status follows
	// from whether anything is still enforcing.
	st := f.state[id]
	st.lastAttempt, st.entryCount, st.lastError = at, n, why
	st.status = store.ListStatusFailed
	if n > 0 {
		st.status = store.ListStatusStale
	}
	f.state[id] = st
	return nil
}

func (f *fakeFilterStore) MarkListEmpty(ctx context.Context, id, at int64, why string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		f.state = map[int64]listState{}
	}
	f.state[id] = listState{lastRefreshed: at, lastAttempt: at, status: store.ListStatusEmpty, lastError: why}
	return nil
}
func (f *fakeFilterStore) RenameList(ctx context.Context, id int64, name string) error {
	return nil
}
func (f *fakeFilterStore) SetListEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (f *fakeFilterStore) DeleteList(ctx context.Context, id int64) error {
	return nil
}
func (f *fakeFilterStore) UnassignList(ctx context.Context, groupID, listID int64) error {
	return nil
}
func (f *fakeFilterStore) DeleteRule(ctx context.Context, id int64) error {
	return nil
}

type fakeClientStore struct {
	groups  []store.Group
	clients []store.Client
}

func (f *fakeClientStore) Groups(ctx context.Context) ([]store.Group, error)        { return f.groups, nil }
func (f *fakeClientStore) Clients(ctx context.Context) ([]store.Client, error)      { return f.clients, nil }
func (f *fakeClientStore) AddGroup(ctx context.Context, name string) (int64, error) { return 0, nil }
func (f *fakeClientStore) AddClient(ctx context.Context, c store.Client) (int64, error) {
	return 0, nil
}
func (f *fakeClientStore) UpdateClient(ctx context.Context, c store.Client) error {
	return nil
}
func (f *fakeClientStore) DeleteClient(ctx context.Context, id int64) error {
	return nil
}
func (f *fakeClientStore) RenameGroup(ctx context.Context, id int64, name string) error {
	return nil
}
func (f *fakeClientStore) SetGroupEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (f *fakeClientStore) DeleteGroup(ctx context.Context, id int64) error {
	return nil
}

func TestRefreshDownloadsCompilesAndKeepsOldOnFailure(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 1, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("not compiled: %+v", v)
	}

	failing.Store(true) // server breaks; cached file must keep the block working
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("lost list on failed refresh: %+v", v)
	}
}

func TestRefreshPartialDownloadFallsBackToCache(t *testing.T) {
	var partial atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if partial.Load() {
			// Simulate incomplete download: declare large Content-Length but write only partial data
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("0.0.0.0 partial"))
			// Return without writing the rest → client io.Copy fails with unexpected EOF
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 2, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	// First RefreshAll: healthy server, list is cached
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("initial refresh failed to compile: %+v", v)
	}

	// Switch to partial download mode and refresh again
	partial.Store(true)
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Cache fallback must have preserved the list; ads.example.com should still be blocked
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("partial download did not fall back to cache: %+v", v)
	}
}

// TestRefreshRecordsFailedWhenNothingIsServed is the reported bug: a list
// whose URL 404s with no cached copy used to be persisted as nothing at all
// — RefreshAll logged "list cache not accessible" and moved on, leaving the
// row reading `0 entries, never refreshed`, which is also exactly what a
// list that hasn't refreshed yet looks like. The state must now say failed
// and name the HTTP status.
func TestRefreshRecordsFailedWhenNothingIsServed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 7, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(7)
	if got.status != store.ListStatusFailed {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusFailed)
	}
	if !strings.Contains(got.lastError, "404 Not Found") {
		t.Fatalf("lastError = %q, want it to name the 404", got.lastError)
	}
	if got.entryCount != 0 {
		t.Fatalf("entryCount = %d, want 0 — nothing is being served", got.entryCount)
	}
	if got.lastAttempt == 0 {
		t.Fatal("lastAttempt not recorded; the row would still read 'never'")
	}
	if got.lastRefreshed != 0 {
		t.Fatalf("lastRefreshed = %d, want 0 — no copy was ever fetched", got.lastRefreshed)
	}
}

// TestRefreshRecordsStaleWhenCacheStillServes is the distinction that makes
// the failed state meaningful: the same 404, but after a good fetch, is not
// "blocking nothing" — the cached copy is still compiled and enforcing, so
// it must read stale, keep its entries, and keep last_refreshed pointing at
// the copy it is actually serving.
func TestRefreshRecordsStaleWhenCacheStillServes(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 8, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok := fs.stateOf(8)
	if ok.status != store.ListStatusOK || ok.lastError != "" {
		t.Fatalf("healthy fetch = %+v, want ok with no error", ok)
	}
	firstRefresh := ok.lastRefreshed

	failing.Store(true)
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(8)
	if got.status != store.ListStatusStale {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusStale)
	}
	if got.entryCount == 0 {
		t.Fatal("entryCount = 0; the cached copy is still enforcing, so this is stale, not failed")
	}
	if got.lastRefreshed != firstRefresh {
		t.Fatalf("lastRefreshed moved to %d; it must keep dating the copy being served (%d)", got.lastRefreshed, firstRefresh)
	}
	if got.lastAttempt <= 0 || !strings.Contains(got.lastError, "404 Not Found") {
		t.Fatalf("stale row must still name the failed attempt: %+v", got)
	}
	// The whole point of the cache fallback: it still blocks.
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("stale list stopped blocking: %+v", v)
	}
}

// TestRefreshRecordsEmptyWhenParsedButUseless covers the fourth outcome and
// the one that hurts most, because every number looks healthy: the download
// succeeds, ParseList returns no error, and the list is compiled — with zero
// entries. A hagezi wildcard file did this before wildcardDomain existed.
// "0 entries, refreshed just now" is not a report, so the reason has to name
// the size and the skipped-line count.
func TestRefreshRecordsEmptyWhenParsedButUseless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Well-formed, fetches fine, and yields nothing the parser can use.
		_, _ = w.Write([]byte("ads.*.example.com\nnot a domain at all\n<html>oops</html>\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 9, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(9)
	if got.status != store.ListStatusEmpty {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusEmpty)
	}
	if got.entryCount != 0 {
		t.Fatalf("entryCount = %d, want 0", got.entryCount)
	}
	if !strings.Contains(got.lastError, "no usable entries") || !strings.Contains(got.lastError, "3 lines skipped") {
		t.Fatalf("lastError = %q, want it to name the skipped lines", got.lastError)
	}
	// The fetch itself worked, so last_refreshed advances — the failure is
	// on the parser's side and last_error is what says so.
	if got.lastRefreshed == 0 {
		t.Fatal("lastRefreshed = 0; the download did succeed")
	}
}

// TestRefreshClearsErrorOnRecovery: a list that starts working again must
// stop reporting a failure it no longer has, otherwise the loud failed
// treatment becomes permanent noise and gets ignored.
func TestRefreshClearsErrorOnRecovery(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 10, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fs.stateOf(10); got.status != store.ListStatusFailed {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusFailed)
	}

	failing.Store(false)
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(10)
	if got.status != store.ListStatusOK {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusOK)
	}
	if got.lastError != "" {
		t.Fatalf("lastError = %q, want cleared", got.lastError)
	}
}

// TestRefreshReasonNamesTransportError: a DNS/connect failure has to arrive
// as a cause an admin can act on, not net/http's *url.Error prefix that
// repeats the URL already shown in the row's first column.
func TestRefreshReasonNamesTransportError(t *testing.T) {
	fs := &fakeFilterStore{lists: []store.List{
		{ID: 11, URL: "http://127.0.0.1:1/never-listening", Kind: "block", Enabled: true},
	}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(11)
	if got.status != store.ListStatusFailed {
		t.Fatalf("status = %q, want %q", got.status, store.ListStatusFailed)
	}
	if strings.HasPrefix(got.lastError, "Get \"") {
		t.Fatalf("lastError = %q, want the *url.Error wrapper stripped", got.lastError)
	}
	if !strings.Contains(got.lastError, "connect") && !strings.Contains(got.lastError, "refused") {
		t.Fatalf("lastError = %q, want the transport cause", got.lastError)
	}
}

// TestReasonIsShortAndSingleLine: last_error is rendered verbatim in a table
// cell and is partly remote-controlled (a server picks its own HTTP reason
// phrase), so it must not be able to smuggle newlines or unbounded length
// into the page.
func TestReasonIsShortAndSingleLine(t *testing.T) {
	got := reason("%s", "404 Not\nFound "+strings.Repeat("x", 500))
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("reason kept a newline: %q", got)
	}
	if len(got) > maxReasonLen {
		t.Fatalf("reason is %d bytes, want <= %d", len(got), maxReasonLen)
	}
}

func TestHumanBytesAndCommas(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{humanBytes(0), "0 B"},
		{humanBytes(812), "812 B"},
		{humanBytes(4_500_000), "4.5 MB"},
		{humanBytes(1_200), "1.2 kB"},
		{commas(0), "0"},
		{commas(999), "999"},
		{commas(250431), "250,431"},
		{commas(1000000), "1,000,000"},
	} {
		if tc.in != tc.want {
			t.Errorf("got %q, want %q", tc.in, tc.want)
		}
	}
}

// TestRefreshAllSerializesConcurrentCalls reproduces the interleaving hazard
// fixed by Refresher.mu: the initial-load goroutine, the periodic ticker, and
// the settings-change watcher can all call RefreshAll concurrently. Without
// the mutex their list-cache .tmp writes and ruleset compiles could
// interleave. This asserts at most one RefreshAll body is in flight at a time.
func TestRefreshAllSerializesConcurrentCalls(t *testing.T) {
	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // widen the window so overlap would be visible without the mutex
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
		inFlight.Add(-1)
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 1, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ref.RefreshAll(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := maxObserved.Load(); got > 1 {
		t.Fatalf("RefreshAll calls overlapped: max concurrent in-flight fetches = %d, want 1", got)
	}
}

// Run's interval comes from lists.refresh_hours, and it runs in a background
// goroutine with nothing to recover it: time.NewTicker(0) panicking there
// took the whole process down at every start, over and over, until the row
// was hand-edited. A non-positive interval has to mean "no periodic refresh"
// and nothing more.
func TestRunWithANonPositiveIntervalDoesNotPanic(t *testing.T) {
	for _, every := range []time.Duration{0, -time.Hour} {
		t.Run(every.String(), func(t *testing.T) {
			fs := &fakeFilterStore{}
			cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
			ref := NewRefresher(fs, cs, NewEngine(), t.TempDir())

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				ref.Run(ctx, every)
			}()

			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return when its context was cancelled")
			}
		})
	}
}

// countingList serves a fixed blocklist and counts the requests it answers,
// so a test can assert that a code path did not touch the network.
func countingList(t *testing.T, body string) (string, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// TestRecompileDoesNotDownload is the split every API write depends on:
// adding a rule recompiles from what is already on disk, so one unreachable
// list URL can no longer make a rule save wait out the fetch timeout.
func TestRecompileDoesNotDownload(t *testing.T) {
	url, hits := countingList(t, "0.0.0.0 ads.example.com\n")
	fs := &fakeFilterStore{lists: []store.List{{ID: 1, URL: url, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("download made %d requests, want 1", got)
	}
	if err := ref.Recompile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("Recompile made %d requests, want none", got-1)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("recompile lost the cached list: %+v", v)
	}
}

// TestRecompileServesCachedCopyWithoutTheNetwork is the restart case: the
// cached copy on disk has to be compiled and enforcing before anything is
// downloaded, so a reboot with the WAN down does not leave the LAN
// unfiltered for as long as the fetch timeout takes.
func TestRecompileServesCachedCopyWithoutTheNetwork(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lists", "3.txt"), []byte("0.0.0.0 ads.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeFilterStore{lists: []store.List{
		{ID: 3, URL: "http://192.0.2.1:9/never-answers", Kind: "block", Enabled: true},
	}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, dir)

	if err := ref.Recompile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("cached copy not compiled: %+v", v)
	}
}

// TestRecompileSkipsDisabledListsAndGroups: compiling from cache applies the
// same enabled checks the download path does, or disabling a list would keep
// enforcing it until the next download.
func TestRecompileSkipsDisabledListsAndGroups(t *testing.T) {
	url, _ := countingList(t, "0.0.0.0 ads.example.com\n")
	fs := &fakeFilterStore{lists: []store.List{{ID: 1, URL: url, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{
		{ID: 1, Name: "default", Enabled: true},
		{ID: 2, Name: "off", Enabled: false},
	}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*eng.groups.Load())[2]; ok {
		t.Fatal("disabled group got a compiled ruleset")
	}

	fs.lists[0].Enabled = false
	if err := ref.Recompile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action == "block" {
		t.Fatalf("disabled list still enforcing: %+v", v)
	}
}

// TestFetchSendsETagAndHandles304 covers the conditional-request round trip:
// a second refresh offers the stored ETag, and the 304 means "the cached
// copy is current", not a failure.
func TestFetchSendsETagAndHandles304(t *testing.T) {
	var conditional atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 4, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conditional.Load() != 1 {
		t.Fatalf("If-None-Match sent %d times, want 1", conditional.Load())
	}
	got := fs.stateOf(4)
	if got.status != store.ListStatusOK || got.lastError != "" {
		t.Fatalf("304 recorded as %+v, want ok with no error", got)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("304 lost the list: %+v", v)
	}
}

// TestFetchRecoversFromAStaleETag is the permanent-failure bug: the cache
// file goes missing while its .etag survives (a half-cleared data dir), so
// every refresh asked conditionally, got a 304, and failed on the cache it
// no longer had — forever. The etag must not be offered without the copy it
// describes.
func TestFetchRecoversFromAStaleETag(t *testing.T) {
	var conditional atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	fs := &fakeFilterStore{lists: []store.List{{ID: 5, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, dir, AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "lists", "5.txt")); err != nil {
		t.Fatal(err)
	}
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conditional.Load() != 0 {
		t.Fatalf("If-None-Match offered %d times with no cache file to back it", conditional.Load())
	}
	got := fs.stateOf(5)
	if got.status != store.ListStatusOK {
		t.Fatalf("status = %q (%q), want %q — the list must recover", got.status, got.lastError, store.ListStatusOK)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("list did not recover: %+v", v)
	}
}

// TestFetchRefetchesWhen304HasNoReadableCache covers the other half: a
// server that answers 304 to an unconditional request. The etag is dropped
// and the list is fetched once more rather than failing "cache unreadable"
// every 24h.
func TestFetchRefetchesWhen304HasNoReadableCache(t *testing.T) {
	var first atomic.Bool
	first.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.Swap(false) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 6, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir(), AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fs.stateOf(6); got.status != store.ListStatusOK {
		t.Fatalf("status = %q (%q), want the re-fetch to succeed", got.status, got.lastError)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("re-fetch did not compile: %+v", v)
	}
}

// TestFetchCapsTheBodySize: a list body is written straight into the data
// dir, so an oversized (or endless) one has to be a failed fetch rather than
// a full disk followed by a trie built from it.
func TestFetchCapsTheBodySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		line := []byte("0.0.0.0 ads.example.com\n")
		for written := 0; written <= maxListBytes; written += len(line) {
			if _, err := w.Write(line); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	fs := &fakeFilterStore{lists: []store.List{{ID: 12, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), dir, AllowLoopbackTargets())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fs.stateOf(12)
	if got.status != store.ListStatusFailed {
		t.Fatalf("status = %q (%q), want %q", got.status, got.lastError, store.ListStatusFailed)
	}
	if !strings.Contains(got.lastError, "too large") {
		t.Fatalf("lastError = %q, want it to name the size limit", got.lastError)
	}
	if _, err := os.Stat(filepath.Join(dir, "lists", "12.txt")); !os.IsNotExist(err) {
		t.Fatalf("oversized body was kept on disk: %v", err)
	}
}

// TestFetchRefusesPrivateTargets: list URLs are admin-supplied but fetched
// by the server itself, so one aimed at the host's own loopback, a
// link-local metadata endpoint or a neighbour on the LAN has to be refused
// rather than turning dnsaur into a confused deputy.
func TestFetchRefusesPrivateTargets(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"loopback_v6", "http://[::1]:8080/admin"},
		{"link_local_metadata", "http://169.254.169.254/latest/meta-data/"},
		{"rfc1918", "http://10.0.0.1/hosts"},
		{"unique_local_v6", "http://[fd00::1]/hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeFilterStore{lists: []store.List{{ID: 13, URL: tc.url, Kind: "block", Enabled: true}}}
			cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
			ref := NewRefresher(fs, cs, NewEngine(), t.TempDir())
			if err := ref.RefreshAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := fs.stateOf(13)
			if got.status != store.ListStatusFailed {
				t.Fatalf("status = %q, want %q", got.status, store.ListStatusFailed)
			}
			if !strings.Contains(got.lastError, "not a public address") {
				t.Fatalf("lastError = %q, want it to name the refusal", got.lastError)
			}
		})
	}
}

// TestFetchRefusesAPrivateRedirect: the check has to survive the hop, since
// the URL an admin sees is not necessarily the address that gets dialled.
// The first hop is loopback, which this refresher allows (see
// AllowLoopbackTargets); the second is the cloud metadata address, which
// nothing allows.
func TestFetchRefusesAPrivateRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 14, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	ref := NewRefresher(fs, cs, NewEngine(), t.TempDir(), AllowLoopbackTargets())
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fs.stateOf(14); !strings.Contains(got.lastError, "not a public address") {
		t.Fatalf("lastError = %q, want the redirect target refused", got.lastError)
	}
}
