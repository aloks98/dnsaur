package store

import (
	"context"
	"errors"
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
