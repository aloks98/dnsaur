package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// Sentinels for the two DHCP rules a caller has to tell apart from a
// malformed field: a subnet already claimed by another scope, and an address
// that belongs to no scope at all. Everything else these validators refuse is
// a plain error whose message names the column — the API renders it beside
// the field, and there is nothing for code to branch on.
var (
	// ErrScopeOverlap is §4.3's "no two enabled scopes overlap". Two scopes
	// claiming the same address would have the engine hand the same lease
	// out twice, which is not a configuration anything downstream can
	// recover from.
	ErrScopeOverlap = errors.New("overlaps an enabled scope")
	// ErrOutsideScope is a reservation whose address is not inside the
	// subnet of the scope that holds it. The API answers 400, naming the
	// scope's CIDR.
	ErrOutsideScope = errors.New("address is outside the scope")
)

// leaseFloor is the shortest non-zero lease a scope may ask for (§4.3). A
// lease measured in seconds has every client on the segment renewing
// continuously, and the operator who typed it meant minutes. 0 is not a
// short lease, it is "use the dhcp.lease_seconds setting".
const leaseFloor = 300

// Scope is one DHCP subnet: what the engine is told to hand out, and the
// only thing an operator edits (§4.3). Its ids travel in the config bundle,
// so a scope means the same row on the main and on its replica.
type Scope struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// CIDR is an IPv4 prefix in masked form, e.g. "192.168.1.0/24".
	CIDR string `json:"cidr"`
	// PoolStart and PoolEnd bound the dynamic range, inclusive. A
	// reservation may sit inside it or outside it.
	PoolStart string `json:"pool_start"`
	PoolEnd   string `json:"pool_end"`
	// Gateway is the router option; empty hands out no router.
	Gateway string `json:"gateway"`
	// DNSServers is a comma-separated list of addresses; empty means the
	// automatic answer of §5.3 rather than "no DNS".
	DNSServers string `json:"dns_servers"`
	// Domain is the suffix handed to clients; empty falls back to the
	// dhcp.domain setting.
	Domain string `json:"domain"`
	// LeaseSeconds is at least leaseFloor, or 0 for the dhcp.lease_seconds
	// setting.
	LeaseSeconds int `json:"lease_seconds"`
	// Enabled false keeps the scope out of the rendered config, and out of
	// the overlap rule with it.
	Enabled bool `json:"enabled"`
	// DomainSearch is option 119: comma-separated suffixes, or empty.
	DomainSearch string `json:"domain_search"`
	// NTPServers is option 42: comma-separated IPv4 addresses, or empty.
	NTPServers string `json:"ntp_servers"`
	// StaticRoutes is option 121. Stored as JSON text in one column: the
	// list is written whole and read whole, and nothing joins against it.
	StaticRoutes []StaticRoute `json:"static_routes"`
	// NextServer, ServerHostname and BootFile are PXE's three fields —
	// siaddr, sname (option 66) and file (option 67) — each optional alone.
	NextServer     string `json:"next_server"`
	ServerHostname string `json:"server_hostname"`
	BootFile       string `json:"boot_file"`
	// Options is every option with no field of its own, as raw hex. Codes
	// the renderer emits by name are refused, so one option code never has
	// two answers.
	Options []GenericOption `json:"options"`
	// MatchClientID false makes the engine key a lease on the hardware
	// address alone and ignore option 61, which is what cloned VMs sharing
	// a client id need. The column defaults to true and the Go zero value
	// is false, so a Scope built in code — rather than read from a row or
	// decoded from a form — has to say so.
	MatchClientID bool `json:"match_client_id"`
	// ReservationsOnly renders the subnet with no pool: only reserved
	// devices get an address, and the pool columns may be left empty.
	ReservationsOnly bool  `json:"reservations_only"`
	CreatedAt        int64 `json:"created_at"`
	ModifiedAt       int64 `json:"modified_at"`
}

// UnmarshalJSON decodes a scope, supplying match_client_id's default when
// the key is absent. It is the one field whose column default (true) and Go
// zero value (false) disagree, so a body that says nothing about it — an API
// request, or a bundle from a main that predates the column — would
// otherwise ask for client-id matching to be switched off on every scope it
// carried. Decoding is the one place every such body passes through.
func (s *Scope) UnmarshalJSON(b []byte) error {
	// The alias drops this method, so the embedded decode does not recurse;
	// the outer field shadows the embedded one, being the shallower of the
	// two with that tag.
	type scope Scope
	aux := struct {
		*scope
		MatchClientID *bool `json:"match_client_id"`
	}{scope: (*scope)(s)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	s.MatchClientID = aux.MatchClientID == nil || *aux.MatchClientID
	return nil
}

// StaticRoute is one entry of option 121: a destination prefix and the
// router on this segment that reaches it.
type StaticRoute struct {
	Destination string `json:"destination"`
	Router      string `json:"router"`
}

// GenericOption is one option the scope has no field for, as the engine
// takes it: a code and its value in hex. Hex is stored uppercase without
// separators (normaliseScope), so the same bytes are one string here.
type GenericOption struct {
	Code int    `json:"code"`
	Hex  string `json:"hex"`
}

// Reservation pins one MAC to one address inside a scope (§4.4).
type Reservation struct {
	ID      int64 `json:"id"`
	ScopeID int64 `json:"scope_id"`
	// MAC is canonical lowercase "aa:bb:cc:dd:ee:ff" — CanonicalMAC's
	// spelling, which is what makes the unique index mean "this NIC".
	MAC string `json:"mac"`
	IP  string `json:"ip"`
	// Hostname is an RFC 1123 label or empty, unique within the scope when
	// set.
	Hostname   string `json:"hostname"`
	Comment    string `json:"comment"`
	CreatedAt  int64  `json:"created_at"`
	ModifiedAt int64  `json:"modified_at"`
}

// DHCPStore manages DHCP scopes and their reservations. Like every other
// synced table, each write advances config_version in its own transaction.
//
// It validates nothing beyond what the schema does: the rules of §4.3 and
// §4.4 are ValidateScope and ValidateReservation, pure functions the API
// calls before it writes, so the same answer can be given to a form before
// anything is stored. What does reach here is the two unique indexes, which
// surface as ErrDuplicate exactly as groups.name and lists.url do.
type DHCPStore interface {
	Scopes(ctx context.Context) ([]Scope, error)
	Scope(ctx context.Context, id int64) (Scope, error)
	AddScope(ctx context.Context, s Scope) (int64, error)
	UpdateScope(ctx context.Context, s Scope) error
	// DeleteScope removes the scope and its reservations in one
	// transaction. The foreign key carries no ON DELETE CASCADE, for the
	// reason the 0016 migration gives: the bundle's prune orders children
	// before parents for every table, and this pair is no exception.
	DeleteScope(ctx context.Context, id int64) error
	// Reservations returns every reservation of every scope, ordered by id:
	// the engine's config is rendered from all of them at once, and so is
	// the bundle.
	Reservations(ctx context.Context) ([]Reservation, error)
	AddReservation(ctx context.Context, r Reservation) (int64, error)
	UpdateReservation(ctx context.Context, r Reservation) error
	DeleteReservation(ctx context.Context, id int64) error
}

type dhcpStore struct{ s *sqlStore }

const (
	scopeColumns       = `id, name, cidr, pool_start, pool_end, gateway, dns_servers, domain, lease_seconds, enabled, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, match_client_id, reservations_only, created_at, modified_at`
	reservationColumns = `id, scope_id, mac, ip, hostname, comment, created_at, modified_at`
)

func scanScope(row interface{ Scan(...any) error }, s *Scope) error {
	var routes, options string
	if err := row.Scan(&s.ID, &s.Name, &s.CIDR, &s.PoolStart, &s.PoolEnd, &s.Gateway, &s.DNSServers, &s.Domain, &s.LeaseSeconds, &s.Enabled,
		&s.DomainSearch, &s.NTPServers, &routes, &s.NextServer, &s.ServerHostname, &s.BootFile, &options, &s.MatchClientID, &s.ReservationsOnly,
		&s.CreatedAt, &s.ModifiedAt); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(routes), &s.StaticRoutes); err != nil {
		return fmt.Errorf("scope %d static_routes: %w", s.ID, err)
	}
	if err := json.Unmarshal([]byte(options), &s.Options); err != nil {
		return fmt.Errorf("scope %d options: %w", s.ID, err)
	}
	return nil
}

// marshalList is how a scope's two list columns reach the database. Both
// hold nothing but strings and ints, which json.Marshal has no failing case
// for; an empty list is written "[]" rather than the null a nil slice
// marshals to, so the column reads back as a list whether or not anything
// was ever put in it.
func marshalList[T any](v []T) string {
	b, err := json.Marshal(v)
	if err != nil || len(v) == 0 {
		return "[]"
	}
	return string(b)
}

func (d *dhcpStore) Scopes(ctx context.Context) ([]Scope, error) {
	rows, err := d.s.db.QueryContext(ctx, `SELECT `+scopeColumns+` FROM dhcp_scopes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil so no scopes marshals to `[]` rather than `null`, like every
	// other list this package hands out — and a box with DHCP switched off
	// is the default state, not an edge case.
	out := []Scope{}
	for rows.Next() {
		var s Scope
		if err := scanScope(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *dhcpStore) Scope(ctx context.Context, id int64) (Scope, error) {
	var s Scope
	row := d.s.db.QueryRowContext(ctx, d.s.q(`SELECT `+scopeColumns+` FROM dhcp_scopes WHERE id = ?`), id)
	if err := scanScope(row, &s); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Scope{}, ErrNotFound
		}
		return Scope{}, err
	}
	return s, nil
}

// normaliseScope trims the columns that hold an address. They are written by
// hand and pasted from spreadsheets, and a stored " 10.0.0.0/24" parses
// nowhere: the renderer, the overlap rule and every reservation check would
// each have to trim it, or quietly fail to. The comma-separated ones go
// through normaliseList, which is that rule applied per entry — dns_servers
// among them, because the renderer hands that column to Kea exactly as it is
// stored and Kea parses " 1.1.1.1" no better than anything else does.
//
// name, domain and comment are the operator's own text, and trimming text is
// a different decision from canonicalising an address.
func normaliseScope(s Scope) Scope {
	s.CIDR = strings.TrimSpace(s.CIDR)
	s.PoolStart = strings.TrimSpace(s.PoolStart)
	s.PoolEnd = strings.TrimSpace(s.PoolEnd)
	s.Gateway = strings.TrimSpace(s.Gateway)
	s.NextServer = strings.TrimSpace(s.NextServer)
	s.ServerHostname = strings.TrimSpace(s.ServerHostname)
	s.BootFile = strings.TrimSpace(s.BootFile)
	s.DNSServers = normaliseList(s.DNSServers)
	s.DomainSearch = normaliseList(s.DomainSearch)
	s.NTPServers = normaliseList(s.NTPServers)
	// Cloned first: the caller's Scope is its own, and normalising in place
	// would rewrite the slice it still holds.
	s.StaticRoutes = slices.Clone(s.StaticRoutes)
	for i, r := range s.StaticRoutes {
		s.StaticRoutes[i] = StaticRoute{Destination: strings.TrimSpace(r.Destination), Router: strings.TrimSpace(r.Router)}
	}
	s.Options = slices.Clone(s.Options)
	for i, o := range s.Options {
		// One spelling of one value: an option pasted from a vendor
		// document arrives with colons and in whichever case that document
		// used, and the engine reads plain hex.
		s.Options[i].Hex = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(o.Hex), ":", ""))
	}
	return s
}

// normaliseList is the one spelling of a comma-separated list this system
// stores: entries trimmed, empties dropped, ", " between them. The renderer
// hands these columns to the engine as they are, so whatever spacing was
// typed into the form would otherwise be what the engine parses.
func normaliseList(list string) string {
	parts := strings.Split(list, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// normaliseReservation is what the two unique indexes rest on. (scope_id,
// mac) means "this NIC" only if one NIC has one spelling here, so a MAC that
// parses is stored as CanonicalMAC wrote it — otherwise the same machine is
// reservable once per notation an operator can type.
//
// A MAC that does not parse is stored as typed rather than refused: the
// store validates nothing, ValidateReservation does, and a half-rule in the
// write path would be a second owner of the same question giving a different
// answer.
func normaliseReservation(r Reservation) Reservation {
	if mac, ok := CanonicalMAC(r.MAC); ok {
		r.MAC = mac
	}
	r.IP = strings.TrimSpace(r.IP)
	return r
}

func (d *dhcpStore) AddScope(ctx context.Context, sc Scope) (int64, error) {
	s := normaliseScope(sc)
	return d.s.configInsert(ctx,
		`INSERT INTO dhcp_scopes (name, cidr, pool_start, pool_end, gateway, dns_servers, domain, lease_seconds, enabled, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, match_client_id, reservations_only, created_at, modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Name, s.CIDR, s.PoolStart, s.PoolEnd, s.Gateway, s.DNSServers, s.Domain, s.LeaseSeconds, s.Enabled,
		s.DomainSearch, s.NTPServers, marshalList(s.StaticRoutes), s.NextServer, s.ServerHostname, s.BootFile,
		marshalList(s.Options), s.MatchClientID, s.ReservationsOnly, s.CreatedAt, s.ModifiedAt)
}

func (d *dhcpStore) UpdateScope(ctx context.Context, sc Scope) error {
	s := normaliseScope(sc)
	return d.s.configExecOne(ctx,
		`UPDATE dhcp_scopes SET name = ?, cidr = ?, pool_start = ?, pool_end = ?, gateway = ?, dns_servers = ?, domain = ?, lease_seconds = ?, enabled = ?,
		 domain_search = ?, ntp_servers = ?, static_routes = ?, next_server = ?, server_hostname = ?, boot_file = ?, options = ?, match_client_id = ?, reservations_only = ?, modified_at = ? WHERE id = ?`,
		s.Name, s.CIDR, s.PoolStart, s.PoolEnd, s.Gateway, s.DNSServers, s.Domain, s.LeaseSeconds, s.Enabled,
		s.DomainSearch, s.NTPServers, marshalList(s.StaticRoutes), s.NextServer, s.ServerHostname, s.BootFile,
		marshalList(s.Options), s.MatchClientID, s.ReservationsOnly, s.ModifiedAt, s.ID)
}

func (d *dhcpStore) DeleteScope(ctx context.Context, id int64) error {
	return d.s.configWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, d.s.q(`DELETE FROM dhcp_reservations WHERE scope_id = ?`), id); err != nil {
			return wrapDBErr(err)
		}
		// Last, and in the same transaction: an unknown id makes this
		// ErrNotFound, which unwinds the delete above rather than leaving a
		// scope's reservations gone without the scope.
		return execOneTx(ctx, tx, d.s.dialect, `DELETE FROM dhcp_scopes WHERE id = ?`, id)
	})
}

func scanReservation(row interface{ Scan(...any) error }, r *Reservation) error {
	return row.Scan(&r.ID, &r.ScopeID, &r.MAC, &r.IP, &r.Hostname, &r.Comment, &r.CreatedAt, &r.ModifiedAt)
}

func (d *dhcpStore) Reservations(ctx context.Context) ([]Reservation, error) {
	rows, err := d.s.db.QueryContext(ctx, `SELECT `+reservationColumns+` FROM dhcp_reservations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reservation{}
	for rows.Next() {
		var r Reservation
		if err := scanReservation(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *dhcpStore) AddReservation(ctx context.Context, res Reservation) (int64, error) {
	r := normaliseReservation(res)
	return d.s.configInsert(ctx,
		`INSERT INTO dhcp_reservations (scope_id, mac, ip, hostname, comment, created_at, modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ScopeID, r.MAC, r.IP, r.Hostname, r.Comment, r.CreatedAt, r.ModifiedAt)
}

// UpdateReservation does not move scope_id: a reservation is deleted and
// recreated to change scope, exactly as a zone record is (updateRecordSQL).
// Moving it would carry an address validated against one subnet into
// another.
func (d *dhcpStore) UpdateReservation(ctx context.Context, res Reservation) error {
	r := normaliseReservation(res)
	return d.s.configExecOne(ctx,
		`UPDATE dhcp_reservations SET mac = ?, ip = ?, hostname = ?, comment = ?, modified_at = ? WHERE id = ?`,
		r.MAC, r.IP, r.Hostname, r.Comment, r.ModifiedAt, r.ID)
}

func (d *dhcpStore) DeleteReservation(ctx context.Context, id int64) error {
	return d.s.configExecOne(ctx, `DELETE FROM dhcp_reservations WHERE id = ?`, id)
}

// ValidateScope is §4.3's column table as one function. others is every other
// scope this instance holds — the overlap rule is a claim about a set of rows,
// so it cannot be a column constraint, and the caller is what knows the set.
//
// Pure, and separate from the store, because the API answers a form with it
// before anything is written, and the renderer reads the same rules when it
// builds the engine's config.
func ValidateScope(s Scope, others []Scope) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("name is required")
	}
	prefix, err := parseSubnet(s.CIDR)
	if err != nil {
		return fmt.Errorf("cidr: %w", err)
	}
	// A reservations_only scope is rendered with no pool, so it is the one
	// kind that may leave both pool columns empty. A pool it does give is
	// checked like anyone else's — half a pool included, which is a form
	// half filled in rather than a scope that wanted none.
	if !s.ReservationsOnly || strings.TrimSpace(s.PoolStart) != "" || strings.TrimSpace(s.PoolEnd) != "" {
		if err := validatePool(s, prefix); err != nil {
			return err
		}
	}
	if s.Gateway != "" {
		gw, err := parseV4(s.Gateway)
		if err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
		if !prefix.Contains(gw) {
			return fmt.Errorf("gateway %s is not inside %s", s.Gateway, s.CIDR)
		}
	}
	if err := validateV4List("dns_servers", s.DNSServers); err != nil {
		return err
	}
	// The scope's own suffix, judged the way the dhcp.domain setting it
	// overrides is: a dotted name, no trailing dot. It is handed to clients
	// as option 15 and is the suffix a lease's name is published under
	// (§8.1), so a value that is not a domain produces names nothing
	// resolves and an option Kea hands out regardless.
	if s.Domain != "" && !validHostname(s.Domain) {
		return fmt.Errorf("domain %q is not a domain suffix like home.lan, with no trailing dot", s.Domain)
	}
	if s.LeaseSeconds != 0 && s.LeaseSeconds < leaseFloor {
		return fmt.Errorf("lease_seconds %d is below the %d second floor (0 uses the dhcp.lease_seconds setting)", s.LeaseSeconds, leaseFloor)
	}
	if err := validateSearchList(s.DomainSearch); err != nil {
		return err
	}
	if err := validateV4List("ntp_servers", s.NTPServers); err != nil {
		return err
	}
	for _, r := range s.StaticRoutes {
		dest, err := parseSubnet(r.Destination)
		if err != nil {
			return fmt.Errorf("static route destination: %w", err)
		}
		router, err := parseV4(r.Router)
		if err != nil {
			return fmt.Errorf("static route %s router: %w", dest, err)
		}
		// A router a client cannot reach without the very route it is being
		// given is not a route; on a DHCP segment "reachable" is "in this
		// subnet".
		if !prefix.Contains(router) {
			return fmt.Errorf("static route %s router %s is not inside %s", dest, r.Router, s.CIDR)
		}
	}
	if s.NextServer != "" {
		if _, err := parseV4(s.NextServer); err != nil {
			return fmt.Errorf("next_server: %w", err)
		}
	}
	if s.ServerHostname != "" && !validHostname(s.ServerHostname) {
		return fmt.Errorf("server_hostname %q is not a hostname", s.ServerHostname)
	}
	if len(s.ServerHostname) > serverHostnameLimit {
		return fmt.Errorf("server_hostname is %d bytes, above the %d the sname field holds", len(s.ServerHostname), serverHostnameLimit)
	}
	if len(s.BootFile) > bootFileLimit {
		return fmt.Errorf("boot_file is %d bytes, above the %d the file field holds", len(s.BootFile), bootFileLimit)
	}
	for _, o := range s.Options {
		if err := validateOption(o); err != nil {
			return err
		}
	}
	// A disabled scope is rendered into nothing, so it can overlap whatever
	// it likes in both directions — which is what makes "disable it, then
	// renumber it" a usable sequence rather than a deadlock.
	if !s.Enabled {
		return nil
	}
	for _, o := range others {
		if o.ID == s.ID || !o.Enabled {
			continue
		}
		op, err := parseSubnet(o.CIDR)
		if err != nil {
			// Refusing is the safe direction: a subnet that cannot be parsed
			// cannot be shown not to overlap this one.
			return fmt.Errorf("scope %q has an unreadable cidr %q: %w", o.Name, o.CIDR, err)
		}
		if op.Overlaps(prefix) {
			return fmt.Errorf("%s %w: %s (%s)", s.CIDR, ErrScopeOverlap, o.Name, o.CIDR)
		}
	}
	return nil
}

// ValidateReservation is §4.4's column table. scope is the parent row — every
// one of these rules is relative to it — and others is every reservation the
// instance holds, which is where the per-scope uniqueness rules are decided.
func ValidateReservation(r Reservation, scope Scope, others []Reservation) error {
	if r.ScopeID != scope.ID {
		return fmt.Errorf("reservation names scope %d but was checked against scope %d", r.ScopeID, scope.ID)
	}
	mac, ok := CanonicalMAC(r.MAC)
	if !ok {
		return fmt.Errorf("mac %q is not a MAC address", r.MAC)
	}
	prefix, err := parseSubnet(scope.CIDR)
	if err != nil {
		return fmt.Errorf("scope cidr: %w", err)
	}
	ip, err := parseV4(r.IP)
	if err != nil {
		return fmt.Errorf("ip: %w", err)
	}
	if !prefix.Contains(ip) {
		return fmt.Errorf("%w: %s is not inside %s", ErrOutsideScope, r.IP, scope.CIDR)
	}
	// The pool is deliberately not consulted: §4.4 allows a reservation on
	// either side of it, and Kea keeps a reserved address out of dynamic
	// allocation either way.
	if scope.Gateway != "" {
		if gw, err := parseV4(scope.Gateway); err == nil && gw == ip {
			return fmt.Errorf("%s is the scope's gateway", r.IP)
		}
	}
	if r.Hostname != "" && !validHostLabel(r.Hostname) {
		return fmt.Errorf("hostname %q is not a hostname label (letters, digits and hyphens, not starting or ending with one)", r.Hostname)
	}
	for _, o := range others {
		if o.ID == r.ID || o.ScopeID != r.ScopeID {
			continue
		}
		if om, ok := CanonicalMAC(o.MAC); ok && om == mac {
			return fmt.Errorf("%w: mac %s is reserved in this scope", ErrDuplicate, mac)
		}
		// Both spellings are canonical: netip.ParseAddr is what let either
		// of them be stored.
		if o.IP == r.IP {
			return fmt.Errorf("%w: %s is reserved in this scope", ErrDuplicate, r.IP)
		}
		// Two blank hostnames are not a collision — most reservations have
		// none. Case-insensitively, because a hostname is a DNS label and
		// "NAS" and "nas" are the same name.
		if r.Hostname != "" && strings.EqualFold(o.Hostname, r.Hostname) {
			return fmt.Errorf("%w: hostname %q is used in this scope", ErrDuplicate, r.Hostname)
		}
	}
	return nil
}

// CanonicalMAC is the one spelling of a MAC address this system stores:
// lowercase, colon-separated, six bytes. It accepts everything net.ParseMAC
// does — "AA-BB-CC-DD-EE-FF", "aabb.ccdd.eeff" — so an operator can paste
// from whatever printed it, and reports false for anything else.
//
// Longer forms (EUI-64, InfiniBand) parse fine and are still refused: a
// DHCPv4 reservation is keyed on a 6-byte hardware address, and storing one
// the engine cannot match would be a reservation that silently never fires.
func CanonicalMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return hw.String(), true
}

// parseSubnet reads an IPv4 CIDR and insists it is written in masked form.
// "10.0.0.5/24" is refused rather than quietly treated as 10.0.0.0/24: the
// difference between the two is exactly the kind of typo that looks right in
// a form and hands out the wrong subnet.
func parseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s is not an IPv4 subnet", s)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("%s has host bits set; write it as %s", s, p.Masked())
	}
	return p, nil
}

// parseV4 reads a plain IPv4 address. An IPv4-mapped IPv6 literal is refused
// along with everything else that is not Is4: it would compare unequal to the
// same address written plainly, and these values are compared.
func parseV4(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, err
	}
	if !a.Is4() {
		return netip.Addr{}, fmt.Errorf("%s is not an IPv4 address", s)
	}
	return a, nil
}

// lastAddr is the broadcast address of an IPv4 prefix: every host bit set.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	// Go defines a shift wider than the type as 0, so a /0 gives all ones.
	host := ^uint32(0) >> uint(p.Bits())
	for i := 3; i >= 0; i-- {
		a[i] |= byte(host)
		host >>= 8
	}
	return netip.AddrFrom4(a)
}

// validatePool is §4.3's four pool rules, split out because a
// reservations_only scope skips all four together.
func validatePool(s Scope, prefix netip.Prefix) error {
	start, err := parseV4(s.PoolStart)
	if err != nil {
		return fmt.Errorf("pool_start: %w", err)
	}
	end, err := parseV4(s.PoolEnd)
	if err != nil {
		return fmt.Errorf("pool_end: %w", err)
	}
	if !prefix.Contains(start) || !prefix.Contains(end) {
		return fmt.Errorf("pool %s-%s is not inside %s", s.PoolStart, s.PoolEnd, s.CIDR)
	}
	if start.Compare(end) > 0 {
		return fmt.Errorf("pool_start %s is above pool_end %s", s.PoolStart, s.PoolEnd)
	}
	// Neither end of the pool may be the subnet's own two addresses. They
	// are not host addresses, and a client handed one would answer to every
	// broadcast on the segment. (On a /31 or /32 that leaves no pool at all,
	// which is the honest answer: there is no room for one.)
	network, broadcast := prefix.Addr(), lastAddr(prefix)
	if start == network || end == network || start == broadcast || end == broadcast {
		return fmt.Errorf("pool %s-%s includes the network or broadcast address of %s", s.PoolStart, s.PoolEnd, s.CIDR)
	}
	return nil
}

// The two BOOTP fields these columns land in are fixed and NUL-terminated:
// sname holds 64 bytes and file 128, so the longest name either can carry is
// one byte shorter. A value over the cap is truncated somewhere down the
// wire, which is a boot failure nobody would trace back to this form.
const (
	serverHostnameLimit = 63
	bootFileLimit       = 127
)

// namedOptionCodes are the options the renderer already emits from a field
// of its own (§5.2). A generic option repeating one of them would be a
// second answer to the same code, and which one the engine kept would depend
// on the order they happened to be written in.
var namedOptionCodes = map[int]string{
	1: "the subnet mask", 3: "the gateway", 6: "dns_servers", 15: "domain",
	42: "ntp_servers", 51: "lease_seconds", 54: "the server identifier",
	58: "the renew timer", 59: "the rebind timer", 66: "server_hostname",
	67: "boot_file", 119: "domain_search", 121: "static_routes",
}

func validateOption(o GenericOption) error {
	if o.Code < 1 || o.Code > 254 {
		return fmt.Errorf("option code %d is outside 1-254", o.Code)
	}
	if named, ok := namedOptionCodes[o.Code]; ok {
		return fmt.Errorf("option code %d is %s, which the scope sets by name", o.Code, named)
	}
	// Colons are how a vendor document prints bytes; the value is the bytes.
	// DecodeString is both rules at once — even length, and hex digits.
	value := strings.ReplaceAll(strings.TrimSpace(o.Hex), ":", "")
	if value == "" {
		return fmt.Errorf("option %d has no value", o.Code)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("option %d value %q: %w", o.Code, o.Hex, err)
	}
	return nil
}

// validateV4List checks §4.3's "comma-separated addresses, or empty" for the
// two columns that hold one. Empty means neither "no DNS" nor "no NTP" — it
// is §5.3's automatic answer for one and no option 42 at all for the other —
// so it is the one value that skips every check here.
func validateV4List(column, list string) error {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	for _, part := range strings.Split(list, ",") {
		if _, err := parseV4(part); err != nil {
			return fmt.Errorf("%s: %w", column, err)
		}
	}
	return nil
}

// validateSearchList checks option 119's "comma-separated suffixes, or
// empty". A suffix is a dotted name rather than a label: "lab.lan" is one
// entry, and the comma is what separates two of them.
func validateSearchList(list string) error {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	for _, part := range strings.Split(list, ",") {
		if name := strings.TrimSpace(part); !validHostname(name) {
			return fmt.Errorf("domain_search: %q is not a domain suffix", name)
		}
	}
	return nil
}

// validHostname is a dotted name: one or more labels with a dot between
// them. Splitting is what refuses a leading, trailing or doubled dot — the
// empty string between two of them is not a label.
func validHostname(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !validHostLabel(label) {
			return false
		}
	}
	return true
}

// validHostLabel reports whether s is a single RFC 1123 hostname label:
// letters, digits and hyphens, 1 to 63 of them, with a hyphen at neither end.
// One label, not a name — the suffix comes from the scope's domain, so a
// dotted value here would produce a name nobody meant.
func validHostLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
