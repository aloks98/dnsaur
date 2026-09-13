package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestTSIGKeyCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		id, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: "xfer.e412.in.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQ=",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, ok, err := st.TSIGKeys().ByName(ctx, "xfer.e412.in.")
		if err != nil || !ok {
			t.Fatalf("byName: ok=%v err=%v", ok, err)
		}
		if got.Secret != "c2VjcmV0LXNlY3JldC1zZWNyZXQ=" || got.Algorithm != "hmac-sha256." {
			t.Fatalf("round trip lost fields: %+v", got)
		}
		// The name is the lookup key on every signed message, so it must be unique.
		if _, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: "xfer.e412.in.", Algorithm: "hmac-sha256.", Secret: "b3RoZXI=",
		}); err == nil {
			t.Fatal("a duplicate key name was accepted; the provider would have to guess which one signs")
		}
		if err := st.TSIGKeys().Delete(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, ok, _ := st.TSIGKeys().ByName(ctx, "xfer.e412.in."); ok {
			t.Fatal("key still resolvable after delete")
		}
	})
}

// A zone that names a key depends on it to authenticate its transfers, so
// the key cannot be deleted out from under it. Enforced here rather than by
// a schema foreign key — see the 0009 migration for why — which is exactly
// why it needs a test on both drivers: nothing in the database would stop it.
func TestDeleteTSIGKeyInUseIsRefused(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		id, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: "xfer.e412.in.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		unused, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: "spare.e412.in.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("create spare key: %v", err)
		}
		if _, err := st.Zones().AddZone(ctx, Zone{
			Name: "e412.in", Type: "secondary", Enabled: true,
			Primaries: "192.168.150.5", TSIGKeyID: id,
		}); err != nil {
			t.Fatalf("add zone: %v", err)
		}

		if err := st.TSIGKeys().Delete(ctx, id); !errors.Is(err, ErrInUse) {
			t.Fatalf("delete of a key in use = %v, want ErrInUse", err)
		}
		if _, ok, _ := st.TSIGKeys().Get(ctx, id); !ok {
			t.Fatal("key gone after a refused delete")
		}
		// The guard must not swallow the other two answers.
		if err := st.TSIGKeys().Delete(ctx, unused); err != nil {
			t.Fatalf("delete of an unused key = %v, want nil", err)
		}
		if err := st.TSIGKeys().Delete(ctx, 999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete of a missing key = %v, want ErrNotFound", err)
		}
	})
}

// A `key:` entry in allow_transfer is the same reference by a different
// spelling as tsig_key_id, and gets the same treatment: the key it names
// cannot be deleted out from under it. Key and zone names are made unique per
// run (testGroupName) rather than the fixed "ns2."/"e412.in" of the design —
// tsig_keys.name is UNIQUE and postgres here is one database shared across
// this whole package's test run, so a literal name reused across test
// functions would collide with itself.
func TestDeleteRefusesAKeyNamedByAllowTransfer(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		name := testGroupName("ns2") + "."
		keyID, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: name, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := st.Zones().AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "primary", Enabled: true,
			AllowTransfer: "10.0.0.0/24, key:" + name,
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := st.TSIGKeys().Delete(ctx, keyID); !errors.Is(err, ErrInUse) {
			t.Fatalf("Delete = %v, want ErrInUse: a key an ACL names must not vanish under it", err)
		}
	})
}

// The delimiters in the guard are what make it exact. Without them, a key
// named "ns2." would be found inside "xns2." and "ns2extra." too, and every
// key merely resembled by an ACL entry would be undeletable.
func TestDeleteAllowsAKeyOnlyResembledByAnACLEntry(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		name := testGroupName("ns2") + "."
		keyID, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: name, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		bare := strings.TrimSuffix(name, ".")
		if _, err := st.Zones().AddZone(ctx, Zone{
			Name: testGroupName("e412.in"), Type: "primary", Enabled: true,
			AllowTransfer: "key:x" + name + ", key:" + bare + "extra.",
		}); err != nil {
			t.Fatalf("AddZone: %v", err)
		}
		if err := st.TSIGKeys().Delete(ctx, keyID); err != nil {
			t.Fatalf("Delete = %v, want nil: no ACL entry names this key", err)
		}
	})
}

// D3 extended the delete guard from tsig_key_id to key: references in
// allow_transfer. notify_to is a third reference, and without it deleting a
// key silently downgrades a signed NOTIFY to an unsigned one that the peer
// then refuses — a failure that surfaces nowhere near the delete that caused
// it.
func TestDeleteTSIGKeyRefusedWhileNotifyToNamesIt(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// Unique per run, for the same reason seedNotifyZone's name is (see
		// its comment): tsig_keys.name is UNIQUE and postgres is one
		// database shared across the whole package's test run.
		name := testGroupName("ns2-xfer") + "."
		keyID, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: name, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		zoneID := seedNotifyZone(t, s, "example.com")
		z, _ := s.Zones().Zone(ctx, zoneID)
		z.NotifyTo = "10.0.0.2:53 key:" + name
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		if err := s.TSIGKeys().Delete(ctx, keyID); !errors.Is(err, ErrInUse) {
			t.Fatalf("Delete = %v, want ErrInUse", err)
		}

		// And it is released once the reference goes, rather than being
		// permanently undeletable.
		z.NotifyTo = ""
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("clearing UpdateZone: %v", err)
		}
		if err := s.TSIGKeys().Delete(ctx, keyID); err != nil {
			t.Fatalf("Delete after clearing = %v, want nil", err)
		}
	})
}

// The guard matches whole names between delimiters, so a key whose name is a
// substring of another's is not held hostage by it. The same rule aclKeyRef
// already documents, applied to the second column.
func TestDeleteTSIGKeyNotifyToMatchesWholeNamesOnly(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// base is shared between the short and long names below, the same
		// way "ns2."/"ns2-xfer." shared "ns2" in the design — see
		// TestDeleteTSIGKeyRefusedWhileNotifyToNamesIt for why the name
		// itself is made unique per run.
		base := testGroupName("ns2")
		shortName := base + "."
		longName := base + "-xfer."
		shortID, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: shortName, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := s.TSIGKeys().Create(ctx, TSIGKey{
			Name: longName, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		}); err != nil {
			t.Fatalf("Add long: %v", err)
		}
		zoneID := seedNotifyZone(t, s, "example.com")
		z, _ := s.Zones().Zone(ctx, zoneID)
		z.NotifyTo = "10.0.0.2:53 key:" + longName
		if err := s.Zones().UpdateZone(ctx, z); err != nil {
			t.Fatalf("UpdateZone: %v", err)
		}

		// Only ns2-xfer. is referenced. ns2. is a prefix of it and must
		// still be deletable.
		if err := s.TSIGKeys().Delete(ctx, shortID); err != nil {
			t.Fatalf("Delete(ns2.) = %v, want nil", err)
		}
	})
}

// TestDeleteRefusesTheDesignatedSyncKey: sync.tsig_key_id names the key every
// replica's transfers are signed with (§4.1). It is a reference like a zone's
// tsig_key_id or an ACL entry, spelled in the settings table instead of the
// zones one, and deleting it would leave every derived secondary on every
// replica unable to sign — a whole installation's zone transfers, taken out
// by a delete the UI showed no reason to refuse.
func TestDeleteRefusesTheDesignatedSyncKey(t *testing.T) {
	forEachDriver(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		id, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: testGroupName("sync") + ".", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		other, err := st.TSIGKeys().Create(ctx, TSIGKey{
			Name: testGroupName("spare") + ".", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := st.Settings().Set(ctx, "sync.tsig_key_id", strconv.FormatInt(id, 10)); err != nil {
			t.Fatalf("designating the sync key: %v", err)
		}
		// postgres here is one database shared by this package's whole run,
		// so the designation must not outlive the subtest that made it.
		t.Cleanup(func() { _ = st.Settings().Set(context.Background(), "sync.tsig_key_id", "0") })

		if err := st.TSIGKeys().Delete(ctx, id); !errors.Is(err, ErrInUse) {
			t.Fatalf("Delete = %v, want ErrInUse: the designated sync key must not vanish", err)
		}
		if _, ok, _ := st.TSIGKeys().Get(ctx, id); !ok {
			t.Fatal("the sync key is gone after a refused delete")
		}
		// Only that one key: the guard is a reference check, not a freeze on
		// the table while sync is configured.
		if err := st.TSIGKeys().Delete(ctx, other); err != nil {
			t.Fatalf("Delete of a key nothing names = %v, want nil", err)
		}
	})
}
