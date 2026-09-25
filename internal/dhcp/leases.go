package dhcp

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// leaseActive is Kea's state 0. Declined (1) and expired-reclaimed (2)
// addresses are not handed out, so they are not leases anyone can look up
// (§7.1).
const leaseActive = 0

// maxLabel is RFC 1123's limit on one hostname label.
const maxLabel = 63

// LeaseEntry is one row of the table: an address Kea has handed out, or a
// reservation that has a hostname and no lease yet — the latter has a zero
// ExpiresAt, which is what tells the two apart (§7.1).
type LeaseEntry struct {
	ScopeID   int64
	IP        netip.Addr
	MAC       string
	Hostname  string
	ExpiresAt time.Time
	Reserved  bool
	// Suffix is the domain this entry's name sits under: the suffix of the
	// class whose pool the address came from, else its scope's, else the
	// dhcp.domain setting. Per entry, because two leases in one scope can
	// sit under two suffixes.
	Suffix string
}

// Table is one poll's leases, indexed the three ways they are asked for: by
// address (the query log and the reverse stage), by MAC (the client matcher)
// and by name (the forward stage). It is immutable — a poll builds a new one
// and the manager swaps it in — so a reader that holds one is looking at a
// consistent set of leases for as long as it holds it.
type Table struct {
	at      time.Time
	entries []LeaseEntry
	byIP    map[netip.Addr]int
	byMAC   map[string]int
	byName  map[string]int
}

// ByIP returns the entry holding ip.
func (t *Table) ByIP(ip netip.Addr) (LeaseEntry, bool) {
	i, ok := t.byIP[ip]
	if !ok {
		return LeaseEntry{}, false
	}
	return t.entries[i], true
}

// ByMAC returns the entry for a hardware address in any spelling
// net.ParseMAC accepts.
func (t *Table) ByMAC(mac string) (LeaseEntry, bool) {
	canonical, ok := store.CanonicalMAC(mac)
	if !ok {
		return LeaseEntry{}, false
	}
	i, ok := t.byMAC[canonical]
	if !ok {
		return LeaseEntry{}, false
	}
	return t.entries[i], true
}

// ByName returns the entry a name resolves to: the sanitised hostname label
// under the suffix its scope hands out. Both are matched case-insensitively,
// and a scope with no suffix has no names at all.
//
// The label is matched, not sanitised: "my_laptop" is a different name from
// the "my-laptop" a lease was given, and answering one with the other would
// invent a name nothing hands out.
func (t *Table) ByName(label, suffix string) (LeaseEntry, bool) {
	label = strings.ToLower(label)
	if label != SanitizeLabel(label) {
		return LeaseEntry{}, false
	}
	key := nameKey(label, suffix)
	if key == "" {
		return LeaseEntry{}, false
	}
	i, ok := t.byName[key]
	if !ok {
		return LeaseEntry{}, false
	}
	return t.entries[i], true
}

// All returns the entries, copied: the table is shared by every reader of it.
func (t *Table) All() []LeaseEntry { return slices.Clone(t.entries) }

// Age is how long ago the poll that built this table read it. It is what the
// dashboard shows beside an unreachable engine, because a table nothing has
// refreshed is still the table being answered from (§7.1).
func (t *Table) Age() time.Duration { return time.Since(t.at) }

// tableFrom builds the snapshot one poll produces: every active lease, with
// the reservations that back them marked, plus the reservations that have a
// name and nothing leasing it. domain is the dhcp.domain setting, which a
// scope that names no suffix of its own hands out.
func tableFrom(leases []Lease, scopes []store.Scope, classes []store.Class, reservations []store.Reservation, domain string) *Table {
	byScope := make(map[int64]store.Scope, len(scopes))
	for _, s := range scopes {
		byScope[s.ID] = s
	}
	classDomain := make(map[int64]string, len(classes))
	for _, c := range classes {
		classDomain[c.ID] = c.Domain
	}
	// suffix is where a name sits: under the class of the pool the address
	// came from when that class sets a domain, else the scope's own. A
	// reservation passes no address and keeps the scope's: it was never
	// drawn from a pool.
	suffix := func(scopeID int64, ip netip.Addr) string {
		s := byScope[scopeID]
		if ip.IsValid() {
			for _, p := range s.Pools {
				if d := classDomain[p.ClassID]; d != "" && inPool(p, ip) {
					return d
				}
			}
		}
		return cmp.Or(s.Domain, domain)
	}
	byMAC := make(map[scopeKey]int, len(reservations))
	byIP := make(map[scopeKey]int, len(reservations))
	for i, r := range reservations {
		byMAC[scopeKey{r.ScopeID, r.MAC}] = i
		byIP[scopeKey{r.ScopeID, r.IP}] = i
	}

	entries := make([]LeaseEntry, 0, len(leases)+len(reservations))
	leased := make([]bool, len(reservations))
	for _, l := range leases {
		if l.State != leaseActive {
			continue
		}
		ip, err := netip.ParseAddr(l.IP)
		if err != nil {
			continue
		}
		mac := l.MAC
		if canonical, ok := store.CanonicalMAC(mac); ok {
			mac = canonical
		}
		e := LeaseEntry{
			ScopeID:   l.SubnetID,
			IP:        ip,
			MAC:       mac,
			Hostname:  l.Hostname,
			ExpiresAt: time.Unix(l.CLTT+l.ValidLft, 0),
			Suffix:    suffix(l.SubnetID, ip),
		}
		// A lease is reserved when the operator pinned either end of it: the
		// MAC in this scope, or the address in it.
		res, ok := byMAC[scopeKey{l.SubnetID, mac}]
		if !ok {
			res, ok = byIP[scopeKey{l.SubnetID, ip.String()}]
		}
		if ok {
			e.Reserved = true
			// The operator's pin, not the pool, placed this address.
			e.Suffix = suffix(l.SubnetID, netip.Addr{})
			leased[res] = true
			// A client that sent no hostname still answers to the name the
			// operator gave its reservation.
			if e.Hostname == "" {
				e.Hostname = reservations[res].Hostname
			}
		}
		entries = append(entries, e)
	}
	// A reservation nothing has leased is still the address that MAC is
	// pinned to, whether or not it was given a name: the `mac` client
	// matcher and the leases page both ask the table for it. Only the name
	// indexes care about the hostname, and newTable skips an entry with
	// nothing to build a label from.
	for i, r := range reservations {
		if leased[i] {
			continue
		}
		ip, err := netip.ParseAddr(r.IP)
		if err != nil {
			continue
		}
		entries = append(entries, LeaseEntry{
			ScopeID: r.ScopeID, IP: ip, MAC: r.MAC, Hostname: r.Hostname, Reserved: true,
			Suffix: suffix(r.ScopeID, netip.Addr{}),
		})
	}
	return newTable(entries)
}

// inPool reports whether ip is inside p, both ends included. A pool that
// does not parse holds nothing.
func inPool(p store.Pool, ip netip.Addr) bool {
	start, err := netip.ParseAddr(p.Start)
	if err != nil {
		return false
	}
	end, err := netip.ParseAddr(p.End)
	if err != nil {
		return false
	}
	return start.Compare(ip) <= 0 && ip.Compare(end) <= 0
}

// without is this table with the lease on ip gone: what the engine holds the
// moment after a lease4-del it accepted (§8.3). A reservation keeps its row
// with the expiry cleared, because the operator's pin outlives the lease —
// that is the state the next poll would rebuild it in, and dropping it whole
// would take the device's `mac` matcher and its name with it for an interval.
//
// The read timestamp goes with it: a release is not a poll, and the table is
// no fresher for one. An address nothing held returns the table unchanged,
// so a double click on Release does not rebuild anything.
func (t *Table) without(ip netip.Addr) *Table {
	if _, held := t.byIP[ip]; !held {
		return t
	}
	entries := make([]LeaseEntry, 0, len(t.entries))
	for _, e := range t.entries {
		if e.IP != ip {
			entries = append(entries, e)
			continue
		}
		if e.Reserved {
			e.ExpiresAt = time.Time{}
			entries = append(entries, e)
		}
	}
	out := newTable(entries)
	out.at = t.at
	return out
}

// scopeKey is a reservation looked up by what it pins, inside the scope that
// holds it: the same MAC or address in another scope is another reservation.
type scopeKey struct {
	scope int64
	value string
}

func newTable(entries []LeaseEntry) *Table {
	t := &Table{
		at:      time.Now(),
		entries: entries,
		byIP:    make(map[netip.Addr]int, len(entries)),
		byMAC:   make(map[string]int, len(entries)),
		byName:  make(map[string]int, len(entries)),
	}
	for i, e := range entries {
		t.byIP[e.IP] = i
		if e.MAC != "" && !claimed(t.byMAC, e.MAC, e, entries) {
			t.byMAC[e.MAC] = i
		}
		key := nameKey(SanitizeLabel(e.Hostname), e.Suffix)
		if key == "" {
			continue
		}
		if !claimed(t.byName, key, e, entries) {
			t.byName[key] = i
		}
	}
	return t
}

// claimed reports whether an index already holds a better entry under key: one
// device answers to one address, and one name names one device.
func claimed(index map[string]int, key string, e LeaseEntry, entries []LeaseEntry) bool {
	held, taken := index[key]
	return taken && !outranks(e, entries[held])
}

// outranks decides which of two entries claiming one key keeps it (§8.1):
// the newer lease, and a reservation over any lease, because a reservation is
// what the operator typed and has no expiry to compare.
func outranks(a, b LeaseEntry) bool {
	if a.Reserved != b.Reserved {
		return a.Reserved
	}
	return a.ExpiresAt.After(b.ExpiresAt)
}

// nameKey joins a sanitised label to a scope's suffix. An empty half means
// there is no name: a lease whose hostname sanitises to nothing, or a scope
// on no domain at all.
func nameKey(label, suffix string) string {
	suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
	if label == "" || suffix == "" {
		return ""
	}
	return label + "." + suffix
}

// SanitizeLabel turns whatever a client called itself into an RFC 1123
// hostname label: lowercased, every run of characters a label may not hold
// collapsed to one hyphen, no hyphen at either end, and 63 bytes at most. A
// name with nothing usable in it comes back empty, which is how a lease ends
// up with no name rather than a nonsense one.
func SanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dashed := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
			dashed = r == '-'
		case !dashed && b.Len() > 0:
			b.WriteByte('-')
			dashed = true
		}
	}
	label := strings.Trim(b.String(), "-")
	if len(label) > maxLabel {
		label = strings.TrimRight(label[:maxLabel], "-")
	}
	return label
}
