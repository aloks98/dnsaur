package confsync

import (
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

func TestDeriveZonesMapsTypes(t *testing.T) {
	in := []store.Zone{
		{ID: 1, Name: "e412.in", Type: "primary", AllowTransfer: "10.0.0.0/8", NotifyTo: "10.0.0.7"},
		{ID: 2, Name: "ext.example", Type: "secondary", Primaries: "203.0.113.5:53", TSIGKeyID: 7},
		{ID: 3, Name: "corp.example", Type: "forwarder", ForwardTo: "10.1.1.1"},
		{ID: 4, Name: "lab.example", Type: "stub", Primaries: "10.2.2.2"},
	}
	out := DeriveZones(in, "10.0.0.5:53", 3)
	byName := map[string]store.Zone{}
	for _, z := range out {
		byName[z.Name] = z
	}
	if z := byName["e412.in"]; z.Type != "secondary" || z.Primaries != "10.0.0.5:53" || z.TSIGKeyID != 3 || z.AllowTransfer != "10.0.0.0/8" || z.NotifyTo != "10.0.0.7" {
		t.Fatalf("primary derived as %+v", z)
	}
	if z := byName["ext.example"]; z.Type != "secondary" || z.Primaries != "203.0.113.5:53" || z.TSIGKeyID != 7 {
		t.Fatalf("secondary must keep its own primary: %+v", z)
	}
	if byName["corp.example"].Type != "forwarder" || byName["lab.example"].Type != "stub" {
		t.Fatal("forwarder and stub must pass through unchanged")
	}
	if in[0].Type != "primary" {
		t.Fatal("DeriveZones must not mutate its input")
	}
}

func TestValidateRejectsDanglingReferences(t *testing.T) {
	b := store.Bundle{Format: store.BundleFormat, Groups: []store.Group{{ID: 1, Name: "default"}}}
	b.Clients = []store.Client{{ID: 1, Matcher: "10.0.0.1", GroupID: 9}}
	if err := Validate(b); err == nil || !strings.Contains(err.Error(), "group 9") {
		t.Fatalf("err = %v", err)
	}
	b.Clients = nil
	b.Settings = map[string]string{"serve.dot.listen": ":853"}
	if err := Validate(b); err == nil || !strings.Contains(err.Error(), "serve.dot.listen") {
		t.Fatalf("err = %v", err)
	}
	b.Settings = nil
	b.Zones = []store.Zone{{ID: 5, Name: "localhost", Type: "internal"}}
	if err := Validate(b); err == nil {
		t.Fatal("internal zones never travel")
	}
}
