package qlog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type fakeQLStore struct {
	mu      sync.Mutex
	batches [][]store.QueryLogEntry
	cutoff  int64
}

func (f *fakeQLStore) InsertBatch(ctx context.Context, b []store.QueryLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]store.QueryLogEntry{}, b...)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeQLStore) DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff = cutoffMs
	return 0, nil
}

func (f *fakeQLStore) Search(ctx context.Context, filter store.QueryLogFilter) ([]store.QueryLogEntry, error) {
	return nil, nil
}

func (f *fakeQLStore) entries() []store.QueryLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.QueryLogEntry
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func blockedHandler() dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked, ListID: 7, Matched: "ads.example"}, nil
	})
}

func TestMiddlewareEmitsAndFlushes(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{BatchSize: 1, FlushEvery: 10 * time.Millisecond, InstanceID: "i1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	h := l.Middleware()(blockedHandler())
	m := new(dns.Msg)
	m.SetQuestion("ads.example.", dns.TypeA)
	_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if es := fs.entries(); len(es) == 1 {
			e := es[0]
			// Matched is what makes list #7 explicable: the middleware has
			// to carry it off the response onto the entry, or the stored row
			// names a list of a hundred thousand entries and not the one line.
			if e.QName != "ads.example" || e.Decision != "blocked" || e.ListID != 7 || e.InstanceID != "i1" || e.Matched != "ads.example" {
				t.Fatalf("%+v", e)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("entry never flushed")
}

func TestAnonPrivacy(t *testing.T) {
	if got := anonymize("10.1.2.3"); got != "10.1.2.0" {
		t.Fatalf("v4 anon: %s", got)
	}
	if got := anonymize("2001:db8:abcd:1234::1"); got != "2001:db8:abcd::" {
		t.Fatalf("v6 anon: %s", got)
	}
}

// fakeStatsStore records the bucket cutoff the prune asked for. The rest of
// the interface is unreachable from the Pruner.
type fakeStatsStore struct {
	mu     sync.Mutex
	cutoff int64
	called bool
}

func (f *fakeStatsStore) Rollup(ctx context.Context, afterID int64) (int64, error) {
	return afterID, nil
}

func (f *fakeStatsStore) PruneBefore(ctx context.Context, bucketBeforeSec int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff, f.called = bucketBeforeSec, true
	return 0, nil
}

func (f *fakeStatsStore) Counter(ctx context.Context, bucketFromSec int64, metric string) (map[string]int64, error) {
	return nil, nil
}

func (f *fakeStatsStore) Timeline(ctx context.Context, fromSec int64) (map[int64]map[string]int64, error) {
	return nil, nil
}

func TestPrunerUsesRetention(t *testing.T) {
	fs := &fakeQLStore{}
	st := &fakeStatsStore{}
	p := NewPruner(fs, st, func() int64 { return 90 }, func() int64 { return 365 })
	p.pruneOnce(context.Background(), time.UnixMilli(1000*24*3600*1000))
	want := int64((1000 - 90) * 24 * 3600 * 1000)
	if fs.cutoff != want {
		t.Fatalf("cutoff %d want %d", fs.cutoff, want)
	}
	// stats_hourly.bucket is a unix *second*, not a millisecond.
	wantStats := int64((1000 - 365) * 24 * 3600)
	if !st.called {
		t.Fatal("stats_hourly was not pruned")
	}
	if st.cutoff != wantStats {
		t.Fatalf("stats cutoff %d want %d", st.cutoff, wantStats)
	}
}

func TestDropOldestKeepsNewest(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{Buffer: 4, BatchSize: 100, FlushEvery: time.Hour, InstanceID: "i1"})
	// Emit 10 entries without running the drain loop (no consumer).
	// Buffer size is 4, so oldest 6 should be dropped, newest 4 kept.
	h := l.Middleware()(blockedHandler())
	for i := 0; i < 10; i++ {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(fmt.Sprintf("q%d.example", i)), dns.TypeA)
		_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	}
	if got := l.Dropped(); got != 6 {
		t.Fatalf("Dropped() = %d, want 6", got)
	}
	// Start Run on the same logger (all 10 entries already buffered, deterministic).
	// Run drains the buffered entries (newest 4 q6-q9 survive in the buffer),
	// flushes to store, then closes done when it returns.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	cancel()
	<-done // wait for Run to return instead of using sleep
	entries := fs.entries()
	if len(entries) != 4 {
		t.Fatalf("expected 4 surviving entries, got %d", len(entries))
	}
	// Surviving entries should be q6, q7, q8, q9 (the newest ones).
	for i, e := range entries {
		want := fmt.Sprintf("q%d.example", i+6)
		if e.QName != want {
			t.Fatalf("entry %d: QName = %s, want %s", i, e.QName, want)
		}
	}
}

// dropCounter counts the warnings the drop report writes, so a test can
// assert both that one was written and that the next minute's worth were
// not.
type dropCounter struct {
	mu sync.Mutex
	n  int
}

func (h *dropCounter) Enabled(context.Context, slog.Level) bool { return true }

func (h *dropCounter) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "query log entries dropped") {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
	}
	return nil
}

func (h *dropCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *dropCounter) WithGroup(string) slog.Handler      { return h }

func (h *dropCounter) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// countDrops installs the counter as the default logger for one test.
func countDrops(t *testing.T) *dropCounter {
	t.Helper()
	h := &dropCounter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// TestDroppedEntriesWarnRateLimited: a full buffer discards queries, and
// until now it did so in total silence — the counter existed and nothing
// read it. The flush loop reports it instead, but at most once a minute:
// the burst that fills a 10k buffer would otherwise write a warning per
// second for as long as it lasts, which is a log nobody can read at the
// moment it matters most.
func TestDroppedEntriesWarnRateLimited(t *testing.T) {
	warnings := countDrops(t)
	l := New(&fakeQLStore{}, Options{Buffer: 2, BatchSize: 100, FlushEvery: time.Hour})
	h := l.Middleware()(blockedHandler())
	for i := 0; i < 5; i++ {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(fmt.Sprintf("q%d.example", i)), dns.TypeA)
		_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	}
	if l.Dropped() != 3 {
		t.Fatalf("Dropped() = %d, want 3", l.Dropped())
	}

	start := time.Unix(1700000000, 0)
	l.reportDropped(start)
	if warnings.count() != 1 {
		t.Fatalf("%d warnings after the first report, want 1", warnings.count())
	}
	// More drops, still inside the same minute: counted, not logged.
	for i := 0; i < 3; i++ {
		m := new(dns.Msg)
		m.SetQuestion("more.example.", dns.TypeA)
		_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	}
	l.reportDropped(start.Add(30 * time.Second))
	if warnings.count() != 1 {
		t.Fatalf("%d warnings inside the rate-limit window, want 1", warnings.count())
	}
	l.reportDropped(start.Add(61 * time.Second))
	if warnings.count() != 2 {
		t.Fatalf("%d warnings after the window, want 2", warnings.count())
	}
	// Nothing new dropped since: silence, however long we wait.
	l.reportDropped(start.Add(10 * time.Minute))
	if warnings.count() != 2 {
		t.Fatalf("%d warnings with no new drops, want 2", warnings.count())
	}
}

// TestRunReportsDrops: the report has to be wired into the flush loop, not
// merely available — a burst that fills the buffer while the store is busy
// is exactly when nobody is calling anything by hand.
func TestRunReportsDrops(t *testing.T) {
	warnings := countDrops(t)
	l := New(&fakeQLStore{}, Options{Buffer: 2, BatchSize: 100, FlushEvery: 5 * time.Millisecond})
	h := l.Middleware()(blockedHandler())
	for i := 0; i < 5; i++ {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(fmt.Sprintf("q%d.example", i)), dns.TypeA)
		_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if warnings.count() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the flush loop never reported the dropped entries")
}

// TestSetPrivacyHotReload verifies privacy is a live setting: flipping it
// via SetPrivacy while the Logger is running must stop/start logging on the
// very next query, without recreating the Logger (mirrors how app.go
// applies a qlog.privacy setting change to the running logger).
func TestSetPrivacyHotReload(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{Privacy: "full", FlushEvery: 10 * time.Millisecond, InstanceID: "i1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	h := l.Middleware()(blockedHandler())
	ask := func(name string) {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	}

	ask("before.example")
	waitForCount(t, fs, 1)

	l.SetPrivacy("none")
	ask("after.example")
	time.Sleep(100 * time.Millisecond) // give the flush loop a chance to (not) pick it up
	if got := len(fs.entries()); got != 1 {
		t.Fatalf("expected logging to stop after SetPrivacy(none), have %d entries", got)
	}

	l.SetPrivacy("full")
	ask("again.example")
	waitForCount(t, fs, 2)
}

func waitForCount(t *testing.T, fs *fakeQLStore, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fs.entries()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d entries, have %d", n, len(fs.entries()))
}

func TestSubscribeReceivesEntries(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{InstanceID: "i1", FlushEvery: time.Hour, BatchSize: 100})
	ch, cancel := l.Subscribe()
	defer cancel()
	h := l.Middleware()(blockedHandler())
	m := new(dns.Msg)
	m.SetQuestion("live.example.", dns.TypeA)
	_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	select {
	case e := <-ch:
		if e.QName != "live.example" {
			t.Fatalf("entry: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no live entry received")
	}
	cancel()
	_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m}) // must not panic after unsubscribe
}

func TestPrivacyNone(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{Privacy: "none", FlushEvery: 10 * time.Millisecond, InstanceID: "i1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	h := l.Middleware()(blockedHandler())
	m := new(dns.Msg)
	m.SetQuestion("test.example.", dns.TypeA)
	_, _ = h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})

	time.Sleep(100 * time.Millisecond) // give flush loop time to process

	if got := l.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
	if got := len(fs.entries()); got != 0 {
		t.Fatalf("store received %d entries, want 0 (privacy=none should not log)", got)
	}
}
