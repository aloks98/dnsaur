package store

import (
	"context"
	"errors"
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
