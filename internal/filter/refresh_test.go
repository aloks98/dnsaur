package filter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

type fakeFilterStore struct {
	lists   []store.List
	touched atomic.Int64
}

func (f *fakeFilterStore) Lists(ctx context.Context) ([]store.List, error) { return f.lists, nil }
func (f *fakeFilterStore) ListsForGroup(ctx context.Context, g int64) ([]store.List, error) {
	return f.lists, nil
}
func (f *fakeFilterStore) Rules(ctx context.Context, g int64) ([]store.Rule, error) {
	return nil, nil
}
func (f *fakeFilterStore) AddList(ctx context.Context, l store.List) (int64, error)  { return 0, nil }
func (f *fakeFilterStore) AssignList(ctx context.Context, g, l int64) error          { return nil }
func (f *fakeFilterStore) AddRule(ctx context.Context, r store.Rule) (int64, error)  { return 0, nil }
func (f *fakeFilterStore) TouchList(ctx context.Context, id, at, n int64) error {
	f.touched.Add(1)
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
	groups []store.Group
	clients []store.Client
}

func (f *fakeClientStore) Groups(ctx context.Context) ([]store.Group, error) { return f.groups, nil }
func (f *fakeClientStore) Clients(ctx context.Context) ([]store.Client, error) { return f.clients, nil }
func (f *fakeClientStore) AddGroup(ctx context.Context, name string) (int64, error) { return 0, nil }
func (f *fakeClientStore) AddClient(ctx context.Context, c store.Client) (int64, error) { return 0, nil }
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
	ref := NewRefresher(fs, cs, eng, t.TempDir())

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
	ref := NewRefresher(fs, cs, eng, t.TempDir())

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
	ref := NewRefresher(fs, cs, eng, t.TempDir())

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
