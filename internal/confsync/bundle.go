// Package confsync holds the pure logic a replica's pull loop runs on a
// bundle before it ever reaches the store: turning the main's zones into the
// replica's own (§4.2), and refusing a bundle whose references don't hold
// together before ImportBundle spends a transaction on it.
package confsync

import (
	"fmt"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
)

// DeriveZones rewrites zones for a replica per spec §4.2 and returns the
// copy; the input is never mutated. primaryDNS is the main's DNS address
// (host:port) a derived secondary pulls from; syncKey is the sync key's id,
// shared by both sides.
//
// A main's primary becomes a secondary of primaryDNS, signed with syncKey,
// keeping the allow-transfer and notify lists it already had so the replica
// serves its own downstreams the same way. Every other type (secondary,
// stub, forwarder) already names its own upstream and travels unchanged.
// internal never reaches here — ExportBundle never carries it — but is
// dropped rather than copied, matching the table in §4.2.
func DeriveZones(zones []store.Zone, primaryDNS string, syncKey int64) []store.Zone {
	out := make([]store.Zone, 0, len(zones))
	for _, z := range zones {
		switch strings.ToLower(z.Type) {
		case "internal":
			continue
		case "primary":
			z.Type = "secondary"
			z.Primaries = primaryDNS
			z.TSIGKeyID = syncKey
		}
		out = append(out, z)
	}
	return out
}

// Validate checks a bundle before import: the format this build reads, that
// every client/rule group_id and every list group assignment names a group
// in the bundle, that every zone's non-zero tsig_key_id names a key in the
// bundle, that no zone is of type internal (§4.2: it never travels), and
// that settings carries no local key (§4.3: a main must not set or clear
// what stays on the instance).
func Validate(b store.Bundle) error {
	if b.Format != store.BundleFormat {
		return fmt.Errorf("bundle format %d: this instance reads %d", b.Format, store.BundleFormat)
	}

	groupIDs := make(map[int64]bool, len(b.Groups))
	for _, g := range b.Groups {
		groupIDs[g.ID] = true
	}
	keyIDs := make(map[int64]bool, len(b.TSIGKeys))
	for _, k := range b.TSIGKeys {
		keyIDs[k.ID] = true
	}

	for _, c := range b.Clients {
		if !groupIDs[c.GroupID] {
			return fmt.Errorf("client %d: group %d not found", c.ID, c.GroupID)
		}
	}
	for _, r := range b.Rules {
		if !groupIDs[r.GroupID] {
			return fmt.Errorf("rule %d: group %d not found", r.ID, r.GroupID)
		}
	}
	for _, l := range b.Lists {
		for _, gid := range l.Groups {
			if !groupIDs[gid] {
				return fmt.Errorf("list %d: group %d not found", l.ID, gid)
			}
		}
	}
	for _, z := range b.Zones {
		if strings.ToLower(z.Type) == "internal" {
			return fmt.Errorf("zone %d %q: type internal never travels in a bundle", z.ID, z.Name)
		}
		if z.TSIGKeyID != 0 && !keyIDs[z.TSIGKeyID] {
			return fmt.Errorf("zone %d: tsig key %d not found", z.ID, z.TSIGKeyID)
		}
	}
	for key := range b.Settings {
		if store.LocalSettingKey(key) {
			return fmt.Errorf("settings: local key %q may not be synced", key)
		}
	}
	return nil
}
