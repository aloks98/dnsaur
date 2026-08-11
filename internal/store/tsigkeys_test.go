package store

import (
	"context"
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
