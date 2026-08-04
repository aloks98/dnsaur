package filter

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// fetch downloads url honoring a cached ETag; on failure or 304 it falls
// back to the cache file. Returns an open reader or an error if neither
// network nor cache is available.
func (r *Refresher) fetch(ctx context.Context, l store.List) (io.ReadCloser, error) {
	etagPath := r.cachePath(l.ID) + ".etag"
	req, err := http.NewRequestWithContext(ctx, "GET", l.URL, nil)
	if err != nil {
		slog.Warn("list request build failed, using cache", "url", l.URL, "err", err)
		return os.Open(r.cachePath(l.ID))
	}
	if etag, err := os.ReadFile(etagPath); err == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		slog.Warn("list download failed, using cache", "url", l.URL, "err", err)
		return os.Open(r.cachePath(l.ID))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		if err := os.MkdirAll(filepath.Dir(r.cachePath(l.ID)), 0o755); err != nil {
			slog.Warn("list cache dir creation failed, using cache", "url", l.URL, "err", err)
			return os.Open(r.cachePath(l.ID))
		}
		tmp := r.cachePath(l.ID) + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			slog.Warn("list temp file creation failed, using cache", "url", l.URL, "err", err)
			return os.Open(r.cachePath(l.ID))
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			slog.Warn("list download incomplete, using cache", "url", l.URL, "err", err)
			return os.Open(r.cachePath(l.ID))
		}
		_ = f.Close()
		if err := os.Rename(tmp, r.cachePath(l.ID)); err != nil {
			_ = os.Remove(tmp)
			slog.Warn("list cache update failed, using cache", "url", l.URL, "err", err)
			return os.Open(r.cachePath(l.ID))
		}
		if et := resp.Header.Get("ETag"); et != "" {
			_ = os.WriteFile(etagPath, []byte(et), 0o644)
		}
	} else if resp.StatusCode != http.StatusNotModified {
		slog.Warn("list download failed, using cache", "url", l.URL, "status", resp.StatusCode)
	}
	return os.Open(r.cachePath(l.ID))
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
		body, err := r.fetch(ctx, l)
		if err != nil {
			slog.Error("list cache not accessible", "url", l.URL, "err", err)
			continue
		}
		res, err := ParseList(body)
		_ = body.Close()
		if err != nil {
			slog.Error("list parse failed", "url", l.URL, "err", err)
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
		_ = r.fs.TouchList(ctx, l.ID, r.now().UnixMilli(), int64(set.Len()))
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

func (r *Refresher) Run(ctx context.Context, every time.Duration) {
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
