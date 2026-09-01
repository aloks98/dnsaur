package zones_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// The SQL guard (tsigKeyStore.Delete, internal/store) and the Go parser
// (zones.ACLKeys) must agree about which keys a value names. Two
// implementations of one rule is how they drift.
//
// This lives here rather than in internal/store because it needs
// zones.ACLKeys, and internal/zones already imports internal/store —
// importing zones from a store test would be a cycle. It runs against
// sqlite only: forEachDriver (internal/store's postgres/sqlite harness) is
// unexported, and the guard's postgres half is exercised by the two sibling
// tests that stayed in internal/store/tsigkeys_test.go.
func TestDeleteGuardAgreesWithACLKeys(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const acl = "10.0.0.0/24, key:ns2., key:ns3."
	if names := zones.ACLKeys(acl); len(names) != 2 || names[0] != "ns2." || names[1] != "ns3." {
		t.Fatalf("ACLKeys(%q) = %v", acl, names)
	}
	if _, err := st.Zones().AddZone(ctx, store.Zone{
		Name: "e412.in", Type: "primary", Enabled: true, AllowTransfer: acl,
	}); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	for _, name := range []string{"ns2.", "ns3."} {
		id, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
			Name: name, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		if err := st.TSIGKeys().Delete(ctx, id); !errors.Is(err, store.ErrInUse) {
			t.Errorf("Delete(%s) = %v, want ErrInUse", name, err)
		}
	}
}
