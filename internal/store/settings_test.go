package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// TestConfigVersionTracksSyncedWrites pins what config_version counts. It is
// the number a replica polls to decide whether the main's configuration
// moved (the config-sync design, §3), so every write to a table that travels
// in a bundle has to advance it — a groups or zones write that left it alone
// was a change no replica would ever pull. Records and transfer bookkeeping
// are the other half of the rule: they are not in a bundle, so moving the
// counter for them would make every secondary's refresh look like a config
// change to the whole fleet.
func TestConfigVersionTracksSyncedWrites(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// The postgres half shares one database across this package's tests,
		// so every name here has to be this run's own.
		uniq := strconv.FormatInt(time.Now().UnixNano(), 36)
		version := func() int64 {
			v, err := s.Settings().ConfigVersion(ctx)
			if err != nil {
				t.Fatalf("ConfigVersion: %v", err)
			}
			return v
		}
		moves := func(what string, want int64, fn func() error) {
			t.Helper()
			before := version()
			if err := fn(); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if got := version() - before; got != want {
				t.Errorf("%s moved config_version by %d, want %d", what, got, want)
			}
		}

		var groupID, listID, zoneID int64
		moves("AddGroup", 1, func() (err error) {
			groupID, err = s.Clients().AddGroup(ctx, "g"+uniq)
			return err
		})
		moves("AddClient", 1, func() (err error) {
			_, err = s.Clients().AddClient(ctx, Client{Name: "c" + uniq, Matcher: "10.77.0.1", GroupID: groupID})
			return err
		})
		moves("AddList", 1, func() (err error) {
			listID, err = s.Filters().AddList(ctx, List{URL: "https://example.test/" + uniq, Kind: "block", Enabled: true})
			return err
		})
		moves("AssignList", 1, func() error { return s.Filters().AssignList(ctx, groupID, listID) })
		moves("AddRule", 1, func() (err error) {
			_, err = s.Filters().AddRule(ctx, Rule{GroupID: groupID, Action: "block", Pattern: "ads." + uniq})
			return err
		})
		moves("TSIGKeys().Create", 1, func() (err error) {
			_, err = s.TSIGKeys().Create(ctx, TSIGKey{Name: "k" + uniq + ".", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0"})
			return err
		})
		moves("Zones().AddZone", 1, func() (err error) {
			zoneID, err = s.Zones().AddZone(ctx, Zone{Name: "z" + uniq + ".test", Type: "primary", Enabled: true})
			return err
		})
		moves("Zones().UpdateZone", 1, func() error {
			z, err := s.Zones().Zone(ctx, zoneID)
			if err != nil {
				return err
			}
			z.AllowTransfer = "10.0.0.0/8"
			return s.Zones().UpdateZone(ctx, z)
		})

		// The other half: not configuration, not in a bundle.
		moves("Zones().AddRecord", 0, func() (err error) {
			_, err = s.Zones().AddRecord(ctx, ZoneRecord{ZoneID: zoneID, Name: "www", Type: "A", TTL: 300, RData: "10.0.0.1", Enabled: true})
			return err
		})
		moves("Zones().NoteTransferAttempt", 0, func() error {
			return s.Zones().NoteTransferAttempt(ctx, zoneID, time.Now().UnixMilli(), "")
		})
		moves("Zones().BumpSerial", 0, func() error { return s.Zones().BumpSerial(ctx, zoneID) })

		// A write that matches nothing changed no configuration.
		moves("DeleteRule on an unknown id", 0, func() error {
			if err := s.Filters().DeleteRule(ctx, 987654321); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("DeleteRule(unknown) = %v, want ErrNotFound", err)
			}
			return nil
		})

		// And the change reaches a subscriber, the way a settings write does.
		ch := s.Settings().Changes()
		want := version() + 1
		if _, err := s.Clients().AddGroup(ctx, "g2"+uniq); err != nil {
			t.Fatalf("AddGroup: %v", err)
		}
		select {
		case got := <-ch:
			if got != want {
				t.Errorf("Changes() published %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a synced write published nothing on Changes()")
		}
	})
}
