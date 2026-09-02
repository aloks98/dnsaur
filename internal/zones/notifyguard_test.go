package zones_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// The store's notify_to key guard matches ` key:<name>,` inside the column
// in SQL (notifyKeyRef, internal/store/tsigkeys.go). That pattern is only
// correct because FormatNotifyTo writes exactly that spelling — so this
// feeds the formatter's real output to the guard rather than a hand-written
// string, which is the difference between pinning the coupling and
// restating it.
//
// It lives here rather than in internal/store because internal/store cannot
// import internal/zones — that direction is an import cycle. aclguard_test.go
// records the same reasoning for the allow_transfer half.
func TestNotifyToSpellingMatchesTheStoreGuard(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	keyID, err := st.TSIGKeys().Create(ctx, store.TSIGKey{
		Name: "ns2-xfer.", Algorithm: "hmac-sha256.", Secret: "c2VjcmV0",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Round-trip through the real parser and formatter, the same way the API
	// layer would before writing notify_to — not a hand-built string that
	// could silently drift from what FormatNotifyTo actually produces.
	targets, err := zones.ParseNotifyTo("10.0.0.2 key:ns2-xfer")
	if err != nil {
		t.Fatalf("ParseNotifyTo: %v", err)
	}
	notifyTo := zones.FormatNotifyTo(targets)

	zoneID, err := st.Zones().AddZone(ctx, store.Zone{
		Name: "e412.in", Type: "primary", Enabled: true, NotifyTo: notifyTo,
	})
	if err != nil {
		t.Fatalf("AddZone: %v", err)
	}

	// The guard must refuse: the zone's notify_to, in FormatNotifyTo's real
	// spelling, names this key. If the guard's pattern and the formatter's
	// spelling have drifted apart, this is where it shows up — the delete
	// would silently succeed.
	if err := st.TSIGKeys().Delete(ctx, keyID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("Delete = %v, want ErrInUse", err)
	}

	// And it must not refuse forever: clearing the reference releases the
	// key, so this fails just as hard if the guard matches everything as if
	// it matches nothing.
	z, err := st.Zones().Zone(ctx, zoneID)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	z.NotifyTo = ""
	if err := st.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("clearing UpdateZone: %v", err)
	}
	if err := st.TSIGKeys().Delete(ctx, keyID); err != nil {
		t.Fatalf("Delete after clearing = %v, want nil", err)
	}
}
