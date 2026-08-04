package clients

import (
	"context"
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

type fakeClientStore struct {
	groups  []store.Group
	clients []store.Client
}

func (f *fakeClientStore) Groups(ctx context.Context) ([]store.Group, error)   { return f.groups, nil }
func (f *fakeClientStore) Clients(ctx context.Context) ([]store.Client, error) { return f.clients, nil }
func (f *fakeClientStore) AddGroup(ctx context.Context, name string) (int64, error) {
	return 0, nil
}
func (f *fakeClientStore) AddClient(ctx context.Context, c store.Client) (int64, error) {
	return 0, nil
}

func TestLookupPrecedence(t *testing.T) {
	fs := &fakeClientStore{
		groups: []store.Group{{ID: 1, Name: "default", Enabled: true}, {ID: 2, Name: "kids", Enabled: true}, {ID: 3, Name: "iot", Enabled: true}},
		clients: []store.Client{
			{ID: 10, Name: "tablet", Matcher: "10.0.0.5", GroupID: 2},
			{ID: 11, Name: "iot-net", Matcher: "10.0.0.0/24", GroupID: 3},
		},
	}
	r := NewRegistry(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.5")); c.GroupName != "kids" {
		t.Fatalf("exact should beat cidr: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("::ffff:10.0.0.5")); c.GroupName != "kids" {
		t.Fatalf("mapped v4 addr should exact-match: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.77")); c.GroupName != "iot" {
		t.Fatalf("cidr match: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("192.168.1.1")); c.GroupName != "default" || c.GroupID != 1 {
		t.Fatalf("unknown -> default: %+v", c)
	}
}
