package qlog

import (
	"context"
	"fmt"
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
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked, ListID: 7}, nil
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
			if e.QName != "ads.example" || e.Decision != "blocked" || e.ListID != 7 || e.InstanceID != "i1" {
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

func TestPrunerUsesRetention(t *testing.T) {
	fs := &fakeQLStore{}
	p := NewPruner(fs, func() int64 { return 90 })
	p.pruneOnce(context.Background(), time.UnixMilli(100*24*3600*1000))
	want := int64((100 - 90) * 24 * 3600 * 1000)
	if fs.cutoff != want {
		t.Fatalf("cutoff %d want %d", fs.cutoff, want)
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
