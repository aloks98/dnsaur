package store

import (
	"context"
	"testing"
	"time"
)

func cleanupSettings(t *testing.T, s Store) {
	// Helper to clean up settings between tests (for shared databases like postgres in testcontainers)
	ctx := context.Background()
	ss := s.(*sqlStore)
	_, _ = ss.db.ExecContext(ctx, ss.q(`DELETE FROM settings`))
	// Reset config_version back to 1
	_, _ = ss.db.ExecContext(ctx, `UPDATE config_version SET version = 1 WHERE id = 1`)
}

func TestSettingsSetBumpsVersionAndNotifies(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupSettings(t, s)
		v0, err := s.Settings().ConfigVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ch := s.Settings().Changes()
		if err := s.Settings().Set(ctx, "blocking.mode", "nxdomain"); err != nil {
			t.Fatal(err)
		}
		v1, _ := s.Settings().ConfigVersion(ctx)
		if v1 != v0+1 {
			t.Fatalf("version not bumped: %d -> %d", v0, v1)
		}
		select {
		case got := <-ch:
			if got != v1 {
				t.Fatalf("notified %d want %d", got, v1)
			}
		case <-time.After(time.Second):
			t.Fatal("no change notification")
		}
		val, ok, _ := s.Settings().Get(ctx, "blocking.mode")
		if !ok || val != "nxdomain" {
			t.Fatalf("get: %q %v", val, ok)
		}
	})
}

func TestSeedDefaultsOnlyFillsMissing(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupSettings(t, s)
		_ = s.Settings().Set(ctx, "blocking.ttl", "60")
		v0, _ := s.Settings().ConfigVersion(ctx)
		if err := s.Settings().SeedDefaults(ctx, map[string]string{"blocking.ttl": "30", "blocking.mode": "null-ip"}); err != nil {
			t.Fatal(err)
		}
		ttl, _ := s.Settings().GetInt(ctx, "blocking.ttl")
		if ttl != 60 {
			t.Fatalf("seed overwrote existing value: %d", ttl)
		}
		mode, ok, _ := s.Settings().Get(ctx, "blocking.mode")
		if !ok || mode != "null-ip" {
			t.Fatalf("seed missed absent key: %q", mode)
		}
		if v1, _ := s.Settings().ConfigVersion(ctx); v1 != v0 {
			t.Fatal("seeding must not bump config version")
		}
	})
}

func TestSetInternalDoesNotBumpVersion(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		cleanupSettings(t, s)
		v0, _ := s.Settings().ConfigVersion(ctx)
		if err := s.Settings().SetInternal(ctx, "stats.watermark", "1000"); err != nil {
			t.Fatal(err)
		}
		v1, _ := s.Settings().ConfigVersion(ctx)
		if v1 != v0 {
			t.Fatalf("SetInternal bumped version: %d -> %d", v0, v1)
		}
		val, ok, _ := s.Settings().Get(ctx, "stats.watermark")
		if !ok || val != "1000" {
			t.Fatalf("SetInternal failed to set value: %q %v", val, ok)
		}
	})
}
