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
	"unicode"
	"unicode/utf8"
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
	// ErrClassInUse is DeleteClass refusing a class a pool still names.
	// The text reads as the middle of DeleteClass's message — `class "iot"
	// is in use by 2 pools (Office, IoT)` — which the API hands on verbatim.
	ErrClassInUse = errors.New("is in use")
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
	// Pools are the dynamic ranges, in the order the operator listed them;
	// the slice order is what the position column stores. A reservation
	// may sit inside any of them or outside all of them.
	Pools []Pool `json:"pools"`
	// Gateway is the router option; empty hands out no router.
	Gateway string `json:"gateway"`
	// LeaseSeconds is at least leaseFloor, or 0 for the dhcp.lease_seconds
	// setting.
	LeaseSeconds int `json:"lease_seconds"`
	// Enabled false keeps the scope out of the rendered config, and out of
	// the overlap rule with it.
	Enabled bool `json:"enabled"`
	// ClientOptions is embedded, so its keys sit at the top level of a
	// scope's JSON exactly where they sat before classes shared them.
	ClientOptions
	// MatchClientID false makes the engine key a lease on the hardware
	// address alone and ignore option 61, which is what cloned VMs sharing
	// a client id need. The column defaults to true and the Go zero value
	// is false, so a Scope built in code — rather than read from a row or
	// decoded from a form — has to say so.
	MatchClientID bool `json:"match_client_id"`
	// ReservationsOnly renders the subnet with no pool: only reserved
	// devices get an address, and Pools may be left empty.
	ReservationsOnly bool  `json:"reservations_only"`
	CreatedAt        int64 `json:"created_at"`
	ModifiedAt       int64 `json:"modified_at"`
}

// ClientOptions is what a client is told beyond its address: the set a
// scope hands to everyone on it and a class hands to its members. One type,
// so the two are validated, normalised and stored by the same code.
type ClientOptions struct {
	// DNSServers is a comma-separated list of addresses. On a scope, empty
	// means the automatic answer of §5.3 rather than "no DNS"; on a class,
	// the scope's value.
	DNSServers string `json:"dns_servers"`
	// Domain is the suffix handed to clients; empty falls back to the
	// scope's, then to the dhcp.domain setting.
	Domain string `json:"domain"`
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
}

// Pool is one dynamic range of a scope, inclusive at both ends.
type Pool struct {
	ID      int64  `json:"id"`
	ScopeID int64  `json:"scope_id"`
	Start   string `json:"start"`
	End     string `json:"end"`
	// ClassID reserves the pool for one class's members; 0 serves any
	// client not in a class that owns a pool in this scope.
	ClassID int64 `json:"class_id"`
}

// Class sorts clients by what they are and gives its members their own
// options, and — through a pool naming it — their own addresses. Global to
// the instance and synced, like a scope.
type Class struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Matchers are "vendor:<prefix>" and "mac:<hex>" tests; a client is a
	// member when any one matches. See validateMatcher.
	Matchers []string `json:"matchers"`
	ClientOptions
	CreatedAt  int64 `json:"created_at"`
	ModifiedAt int64 `json:"modified_at"`
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
// §4.4 are ValidateScope, ValidateClass and ValidateReservation, pure
// functions the API calls before it writes, so the same answer can be given
// to a form before anything is stored. What does reach here is the unique
// indexes, which surface as ErrDuplicate exactly as groups.name and
// lists.url do, and DeleteClass's in-use refusal — the one reference no
// foreign key can hold, since a pool's class_id of 0 names no row.
type DHCPStore interface {
	Scopes(ctx context.Context) ([]Scope, error)
	Scope(ctx context.Context, id int64) (Scope, error)
	AddScope(ctx context.Context, s Scope) (int64, error)
	UpdateScope(ctx context.Context, s Scope) error
	// DeleteScope removes the scope, its reservations and its pools in one
	// transaction. The foreign keys carry no ON DELETE CASCADE, for the
	// reason the 0016 migration gives: the bundle's prune orders children
	// before parents for every table, and these are no exception.
	DeleteScope(ctx context.Context, id int64) error
	// Classes returns every class ordered by id, which is the order the
	// renderer emits them in.
	Classes(ctx context.Context) ([]Class, error)
	Class(ctx context.Context, id int64) (Class, error)
	AddClass(ctx context.Context, c Class) (int64, error)
	UpdateClass(ctx context.Context, c Class) error
	// DeleteClass refuses, with ErrClassInUse naming the scopes, while any
	// pool names the class: deleting it would silently turn a class-only
	// pool into one that serves anyone.
	DeleteClass(ctx context.Context, id int64) error
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
	scopeColumns       = `id, name, cidr, gateway, dns_servers, domain, lease_seconds, enabled, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, match_client_id, reservations_only, created_at, modified_at`
	classColumns       = `id, name, matchers, dns_servers, domain, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, created_at, modified_at`
	poolColumns        = `id, scope_id, start, "end", class_id`
	reservationColumns = `id, scope_id, mac, ip, hostname, comment, created_at, modified_at`
)

// scanScope reads a scope's own row; its pools are a second query, which
// withPools attaches.
func scanScope(row interface{ Scan(...any) error }, s *Scope) error {
	var routes, options string
	if err := row.Scan(&s.ID, &s.Name, &s.CIDR, &s.Gateway, &s.DNSServers, &s.Domain, &s.LeaseSeconds, &s.Enabled,
		&s.DomainSearch, &s.NTPServers, &routes, &s.NextServer, &s.ServerHostname, &s.BootFile, &options, &s.MatchClientID, &s.ReservationsOnly,
		&s.CreatedAt, &s.ModifiedAt); err != nil {
		return err
	}
	return s.decodeLists(fmt.Sprintf("scope %d", s.ID), routes, options)
}

func scanClass(row interface{ Scan(...any) error }, c *Class) error {
	var matchers, routes, options string
	if err := row.Scan(&c.ID, &c.Name, &matchers, &c.DNSServers, &c.Domain, &c.DomainSearch, &c.NTPServers, &routes,
		&c.NextServer, &c.ServerHostname, &c.BootFile, &options, &c.CreatedAt, &c.ModifiedAt); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(matchers), &c.Matchers); err != nil {
		return fmt.Errorf("class %d matchers: %w", c.ID, err)
	}
	return c.decodeLists(fmt.Sprintf("class %d", c.ID), routes, options)
}

// decodeLists parses the two JSON columns every options set has, naming the
// row and the column when one does not parse.
func (o *ClientOptions) decodeLists(row, routes, options string) error {
	if err := json.Unmarshal([]byte(routes), &o.StaticRoutes); err != nil {
		return fmt.Errorf("%s static_routes: %w", row, err)
	}
	if err := json.Unmarshal([]byte(options), &o.Options); err != nil {
		return fmt.Errorf("%s options: %w", row, err)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Closed before the second query: sqlite runs on one connection, and
	// the pools query would otherwise wait on this one for ever.
	rows.Close()
	return out, d.withPools(ctx, out, `SELECT `+poolColumns+` FROM dhcp_pools ORDER BY scope_id, position`)
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
	out := []Scope{s}
	if err := d.withPools(ctx, out, `SELECT `+poolColumns+` FROM dhcp_pools WHERE scope_id = ? ORDER BY position`, id); err != nil {
		return Scope{}, err
	}
	return out[0], nil
}

// withPools reads pools with q and hands each to its scope in scopes, in the
// order q returns them. Every scope gets a non-nil list, so one with no pool
// marshals `[]`.
func (d *dhcpStore) withPools(ctx context.Context, scopes []Scope, q string, args ...any) error {
	rows, err := d.s.db.QueryContext(ctx, d.s.q(q), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byScope := map[int64][]Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.ScopeID, &p.Start, &p.End, &p.ClassID); err != nil {
			return err
		}
		byScope[p.ScopeID] = append(byScope[p.ScopeID], p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range scopes {
		scopes[i].Pools = byScope[scopes[i].ID]
		if scopes[i].Pools == nil {
			scopes[i].Pools = []Pool{}
		}
	}
	return nil
}

func (d *dhcpStore) Classes(ctx context.Context) ([]Class, error) {
	rows, err := d.s.db.QueryContext(ctx, `SELECT `+classColumns+` FROM dhcp_classes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Class{}
	for rows.Next() {
		var c Class
		if err := scanClass(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *dhcpStore) Class(ctx context.Context, id int64) (Class, error) {
	var c Class
	row := d.s.db.QueryRowContext(ctx, d.s.q(`SELECT `+classColumns+` FROM dhcp_classes WHERE id = ?`), id)
	if err := scanClass(row, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Class{}, ErrNotFound
		}
		return Class{}, err
	}
	return c, nil
}

// normaliseScope trims the columns that hold an address. They are written by
// hand and pasted from spreadsheets, and a stored " 10.0.0.0/24" parses
// nowhere: the renderer, the overlap rule and every reservation check would
// each have to trim it, or quietly fail to.
//
// name and comment are the operator's own text, and trimming text is a
// different decision from canonicalising an address.
func normaliseScope(s Scope) Scope {
	s.CIDR = strings.TrimSpace(s.CIDR)
	s.Gateway = strings.TrimSpace(s.Gateway)
	s.ClientOptions = normaliseClientOptions(s.ClientOptions)
	// Cloned first, like the option lists below: the caller's slice is its
	// own.
	s.Pools = slices.Clone(s.Pools)
	for i, p := range s.Pools {
		s.Pools[i].Start, s.Pools[i].End = strings.TrimSpace(p.Start), strings.TrimSpace(p.End)
	}
	return s
}

// normaliseClientOptions is normaliseScope's rule for the options a scope and
// a class share. The comma-separated ones go through normaliseList, which is
// that rule applied per entry — dns_servers among them, because the renderer
// hands that column to Kea exactly as it is stored and Kea parses " 1.1.1.1"
// no better than anything else does. domain is left as typed.
func normaliseClientOptions(o ClientOptions) ClientOptions {
	o.NextServer = strings.TrimSpace(o.NextServer)
	o.ServerHostname = strings.TrimSpace(o.ServerHostname)
	o.BootFile = strings.TrimSpace(o.BootFile)
	o.DNSServers = normaliseList(o.DNSServers)
	o.DomainSearch = normaliseList(o.DomainSearch)
	o.NTPServers = normaliseList(o.NTPServers)
	// Cloned first: the caller's value is its own, and normalising in place
	// would rewrite the slice it still holds.
	o.StaticRoutes = slices.Clone(o.StaticRoutes)
	for i, r := range o.StaticRoutes {
		o.StaticRoutes[i] = StaticRoute{Destination: strings.TrimSpace(r.Destination), Router: strings.TrimSpace(r.Router)}
	}
	o.Options = slices.Clone(o.Options)
	for i, opt := range o.Options {
		// One spelling of one value: an option pasted from a vendor
		// document arrives with colons and in whichever case that document
		// used, and the engine reads plain hex.
		o.Options[i].Hex = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(opt.Hex), ":", ""))
	}
	return o
}

// normaliseClass is normaliseScope for a class. The name is trimmed here,
// unlike a scope's: it is rendered into the engine's config as the class's
// identity and a pool refers to it, so " iot" and "iot" must not be two
// classes. A mac: matcher is lower-cased because the renderer turns it into
// a hex literal and the case-insensitive duplicate of one is the same test.
func normaliseClass(c Class) Class {
	c.Name = strings.TrimSpace(c.Name)
	c.ClientOptions = normaliseClientOptions(c.ClientOptions)
	c.Matchers = slices.Clone(c.Matchers)
	for i, m := range c.Matchers {
		m = strings.TrimSpace(m)
		if strings.HasPrefix(m, "mac:") {
			m = strings.ToLower(m)
		}
		c.Matchers[i] = m
	}
	return c
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
	var id int64
	err := d.s.configWrite(ctx, func(tx *sql.Tx) (err error) {
		id, err = d.s.insertTx(ctx, tx,
			`INSERT INTO dhcp_scopes (name, cidr, gateway, dns_servers, domain, lease_seconds, enabled, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, match_client_id, reservations_only, created_at, modified_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.Name, s.CIDR, s.Gateway, s.DNSServers, s.Domain, s.LeaseSeconds, s.Enabled,
			s.DomainSearch, s.NTPServers, marshalList(s.StaticRoutes), s.NextServer, s.ServerHostname, s.BootFile,
			marshalList(s.Options), s.MatchClientID, s.ReservationsOnly, s.CreatedAt, s.ModifiedAt)
		if err != nil {
			return err
		}
		return d.writePools(ctx, tx, id, s.Pools)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (d *dhcpStore) UpdateScope(ctx context.Context, sc Scope) error {
	s := normaliseScope(sc)
	return d.s.configWrite(ctx, func(tx *sql.Tx) error {
		// First: an unknown id is ErrNotFound before any pool is touched,
		// and the transaction unwinds rather than leaving orphan pools.
		if err := execOneTx(ctx, tx, d.s.dialect,
			`UPDATE dhcp_scopes SET name = ?, cidr = ?, gateway = ?, dns_servers = ?, domain = ?, lease_seconds = ?, enabled = ?,
			 domain_search = ?, ntp_servers = ?, static_routes = ?, next_server = ?, server_hostname = ?, boot_file = ?, options = ?, match_client_id = ?, reservations_only = ?, modified_at = ? WHERE id = ?`,
			s.Name, s.CIDR, s.Gateway, s.DNSServers, s.Domain, s.LeaseSeconds, s.Enabled,
			s.DomainSearch, s.NTPServers, marshalList(s.StaticRoutes), s.NextServer, s.ServerHostname, s.BootFile,
			marshalList(s.Options), s.MatchClientID, s.ReservationsOnly, s.ModifiedAt, s.ID); err != nil {
			return wrapDBErr(err)
		}
		return d.writePools(ctx, tx, s.ID, s.Pools)
	})
}

// writePools replaces a scope's pools with pools, positioned by slice order.
// Rewritten whole rather than diffed: the list is edited as one table in one
// form, and the ids a pool had before carry nothing that anything keeps.
func (d *dhcpStore) writePools(ctx context.Context, tx *sql.Tx, scopeID int64, pools []Pool) error {
	if err := d.checkPoolClasses(ctx, tx, pools); err != nil {
		return err
	}
	if err := d.s.execTx(ctx, tx, `DELETE FROM dhcp_pools WHERE scope_id = ?`, scopeID); err != nil {
		return err
	}
	for i, p := range pools {
		if err := d.s.execTx(ctx, tx, `INSERT INTO dhcp_pools (scope_id, position, start, "end", class_id) VALUES (?, ?, ?, ?, ?)`,
			scopeID, i, p.Start, p.End, p.ClassID); err != nil {
			return err
		}
	}
	return nil
}

// checkPoolClasses is ValidateScope's class rule again, inside the write's
// transaction. The API validates against the classes it read a moment
// earlier, and a DeleteClass landing in between would otherwise leave a pool
// reserved for a class that no longer exists — no foreign key catches it,
// since class_id 0 names no row. On sqlite, one connection makes this check
// and the write a unit; on postgres it narrows the window to the commit.
func (d *dhcpStore) checkPoolClasses(ctx context.Context, tx *sql.Tx, pools []Pool) error {
	var ids []int64
	for _, p := range pools {
		if p.ClassID != 0 && !slices.Contains(ids, p.ClassID) {
			ids = append(ids, p.ClassID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, d.s.q(`SELECT id FROM dhcp_classes WHERE id IN (`+placeholders(len(ids))+`)`), anyIDs(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, p := range pools {
		if p.ClassID != 0 && !found[p.ClassID] {
			return &UnknownClassError{Pool: i, ID: p.ClassID}
		}
	}
	return nil
}

func (d *dhcpStore) DeleteScope(ctx context.Context, id int64) error {
	return d.s.configWrite(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM dhcp_reservations WHERE scope_id = ?`, `DELETE FROM dhcp_pools WHERE scope_id = ?`} {
			if err := d.s.execTx(ctx, tx, q, id); err != nil {
				return err
			}
		}
		// Last, and in the same transaction: an unknown id makes this
		// ErrNotFound, which unwinds the deletes above rather than leaving a
		// scope's reservations and pools gone without the scope.
		return execOneTx(ctx, tx, d.s.dialect, `DELETE FROM dhcp_scopes WHERE id = ?`, id)
	})
}

func (d *dhcpStore) AddClass(ctx context.Context, cl Class) (int64, error) {
	c := normaliseClass(cl)
	return d.s.configInsert(ctx,
		`INSERT INTO dhcp_classes (name, matchers, dns_servers, domain, domain_search, ntp_servers, static_routes, next_server, server_hostname, boot_file, options, created_at, modified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, marshalList(c.Matchers), c.DNSServers, c.Domain, c.DomainSearch, c.NTPServers, marshalList(c.StaticRoutes),
		c.NextServer, c.ServerHostname, c.BootFile, marshalList(c.Options), c.CreatedAt, c.ModifiedAt)
}

func (d *dhcpStore) UpdateClass(ctx context.Context, cl Class) error {
	c := normaliseClass(cl)
	return d.s.configExecOne(ctx,
		`UPDATE dhcp_classes SET name = ?, matchers = ?, dns_servers = ?, domain = ?, domain_search = ?, ntp_servers = ?, static_routes = ?,
		 next_server = ?, server_hostname = ?, boot_file = ?, options = ?, modified_at = ? WHERE id = ?`,
		c.Name, marshalList(c.Matchers), c.DNSServers, c.Domain, c.DomainSearch, c.NTPServers, marshalList(c.StaticRoutes),
		c.NextServer, c.ServerHostname, c.BootFile, marshalList(c.Options), c.ModifiedAt, c.ID)
}

func (d *dhcpStore) DeleteClass(ctx context.Context, id int64) error {
	return d.s.configWrite(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, d.s.q(`SELECT name FROM dhcp_classes WHERE id = ?`), id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// In the same transaction as the delete, so a pool written between
		// the check and the delete cannot be left naming a class that is
		// gone. No foreign key does this: class_id 0 names no row.
		scopes, n, err := d.poolsNaming(ctx, tx, id)
		if err != nil {
			return err
		}
		if n > 0 {
			noun := "pools"
			if n == 1 {
				noun = "pool"
			}
			return fmt.Errorf("class %q %w by %d %s (%s)", name, ErrClassInUse, n, noun, strings.Join(scopes, ", "))
		}
		return execOneTx(ctx, tx, d.s.dialect, `DELETE FROM dhcp_classes WHERE id = ?`, id)
	})
}

// poolsNaming counts the pools reserved for class id and names the scopes
// that hold them, each once, in scope order.
func (d *dhcpStore) poolsNaming(ctx context.Context, tx *sql.Tx, id int64) ([]string, int, error) {
	rows, err := tx.QueryContext(ctx, d.s.q(`SELECT s.name FROM dhcp_pools p JOIN dhcp_scopes s ON s.id = p.scope_id
		WHERE p.class_id = ? ORDER BY s.id, p.position`), id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var names []string
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, 0, err
		}
		n++
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, n, rows.Err()
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
// classes is every class, which a pool's class_id has to name.
//
// Pure, and separate from the store, because the API answers a form with it
// before anything is written, and the renderer reads the same rules when it
// builds the engine's config.
func ValidateScope(s Scope, others []Scope, classes []Class) error {
	if err := validateName(s.Name); err != nil {
		return err
	}
	prefix, err := parseSubnet(s.CIDR)
	if err != nil {
		return fmt.Errorf("cidr: %w", err)
	}
	if err := validatePools(s, prefix, classes); err != nil {
		return err
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
	if s.LeaseSeconds != 0 && s.LeaseSeconds < leaseFloor {
		return fmt.Errorf("lease_seconds %d is below the %d second floor (0 uses the dhcp.lease_seconds setting)", s.LeaseSeconds, leaseFloor)
	}
	if err := validateClientOptions(s.ClientOptions); err != nil {
		return err
	}
	for _, r := range s.StaticRoutes {
		// A router a client cannot reach without the very route it is being
		// given is not a route; on a DHCP segment "reachable" is "in this
		// subnet". A class has no subnet, so this half is the scope's alone.
		if router, _ := parseV4(r.Router); !prefix.Contains(router) {
			return fmt.Errorf("static route %s router %s is not inside %s", r.Destination, r.Router, s.CIDR)
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

// ValidateClass is the class column table of the round-two spec (§3.1).
// others is every class this instance holds, which is where the
// case-insensitive name rule is decided: "IoT" and "iot" would render as two
// engine classes an operator cannot tell apart in a pool's select.
func ValidateClass(c Class, others []Class) error {
	if err := validateName(c.Name); err != nil {
		return err
	}
	name := strings.TrimSpace(c.Name)
	// The renderer names the class inside member('…'), which Kea gives no
	// way to escape.
	if strings.Contains(name, "'") {
		return errors.New("name cannot contain '")
	}
	// The class is rendered under this name into Kea's one namespace, which
	// already holds the renderer's own guard classes (dnsaur-open-<scope>)
	// and Kea's built-ins — DROP among them, which drops its members'
	// packets unanswered.
	if strings.HasPrefix(strings.ToLower(name), "dnsaur-") {
		return errors.New("names starting with dnsaur- are reserved")
	}
	for _, k := range []string{"ALL", "KNOWN", "UNKNOWN", "BOOTING", "DROP"} {
		if strings.EqualFold(name, k) {
			return fmt.Errorf("%s is a class Kea defines itself", name)
		}
	}
	for _, o := range others {
		if o.ID != c.ID && strings.EqualFold(strings.TrimSpace(o.Name), name) {
			return fmt.Errorf("%w: a class named %q exists", ErrDuplicate, o.Name)
		}
	}
	// A class with no matcher matches nobody, and a pool reserved for it
	// would hand out nothing: a form left half filled in, not a class.
	if len(c.Matchers) == 0 {
		return errors.New("matchers: a class needs at least one")
	}
	for i, m := range c.Matchers {
		if err := validateMatcher(strings.TrimSpace(m)); err != nil {
			return fmt.Errorf("matchers[%d]: %w", i, err)
		}
	}
	return validateClientOptions(c.ClientOptions)
}

// UnknownClassError is a pool naming a class that does not exist, from the
// validator or from the write's own re-check. It matches ErrReference, and
// its text is the field and the id alone, so the API can hand it on — which
// it must not do for a driver's own foreign-key text.
type UnknownClassError struct {
	Pool int
	ID   int64
}

func (e *UnknownClassError) Error() string {
	return fmt.Sprintf("pools[%d]: no class with id %d", e.Pool, e.ID)
}

func (e *UnknownClassError) Unwrap() error { return ErrReference }

// nameLimit is the longest scope or class name, in characters: a DNS label's
// length, which is also what fits a table cell and a select.
const nameLimit = 63

// validateName is the rule scope and class names share.
func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name is required")
	}
	// A name is a table cell, a select option and, for a class, an engine
	// identifier: a newline or a tab in one is a paste gone wrong.
	if strings.ContainsFunc(name, unicode.IsControl) {
		return errors.New("name cannot contain control characters")
	}
	if n := utf8.RuneCountInString(name); n > nameLimit {
		return fmt.Errorf("name is %d characters, above the %d allowed", n, nameLimit)
	}
	return nil
}

// macMatcherLimit is the longest hardware-address prefix a mac: matcher
// takes: the whole of an Ethernet address, which is the most a prefix of one
// can be.
const macMatcherLimit = 6

// validateMatcher checks one class matcher. The renderer turns each into a
// Kea expression, so what is refused here is what the engine could not be
// handed: a vendor prefix is rendered inside single quotes, which Kea's
// expression language gives no way to escape, and a mac prefix becomes a hex
// literal of whole bytes.
func validateMatcher(m string) error {
	kind, value, _ := strings.Cut(m, ":")
	switch kind {
	case "vendor":
		switch {
		case value == "" || len(value) > 255:
			return fmt.Errorf("vendor prefix is %d bytes, not 1 to 255", len(value))
		case !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl):
			return fmt.Errorf("vendor prefix %q has a control character or is not text", value)
		case strings.Contains(value, "'"):
			return fmt.Errorf("vendor prefix %q has a single quote, which the engine cannot match on", value)
		}
		return nil
	case "mac":
		octets := strings.Split(value, ":")
		if value == "" || len(octets) > macMatcherLimit {
			return fmt.Errorf("mac prefix %q is not 1 to %d bytes like aa:bb:cc", value, macMatcherLimit)
		}
		for _, o := range octets {
			if _, err := hex.DecodeString(o); err != nil || len(o) != 2 {
				return fmt.Errorf("mac prefix %q is not hex bytes separated by colons, like aa:bb:cc", value)
			}
		}
		return nil
	}
	return fmt.Errorf("%q is neither vendor:<prefix> nor mac:<hex>", m)
}

// validateClientOptions is the half of the column table a scope and a class
// share: every option either can hand a client, judged the same way for
// both.
func validateClientOptions(o ClientOptions) error {
	if err := validateV4List("dns_servers", o.DNSServers); err != nil {
		return err
	}
	// The suffix, judged the way the dhcp.domain setting it overrides is: a
	// dotted name, no trailing dot. It is handed to clients as option 15 and
	// is the suffix a lease's name is published under (§8.1), so a value
	// that is not a domain produces names nothing resolves and an option Kea
	// hands out regardless.
	if o.Domain != "" && !validHostname(o.Domain) {
		return fmt.Errorf("domain %q is not a domain suffix like home.lan, with no trailing dot", o.Domain)
	}
	if err := validateSearchList(o.DomainSearch); err != nil {
		return err
	}
	if err := validateV4List("ntp_servers", o.NTPServers); err != nil {
		return err
	}
	for _, r := range o.StaticRoutes {
		dest, err := parseSubnet(r.Destination)
		if err != nil {
			return fmt.Errorf("static route destination: %w", err)
		}
		if _, err := parseV4(r.Router); err != nil {
			return fmt.Errorf("static route %s router: %w", dest, err)
		}
	}
	if o.NextServer != "" {
		if _, err := parseV4(o.NextServer); err != nil {
			return fmt.Errorf("next_server: %w", err)
		}
	}
	if o.ServerHostname != "" && !validHostname(o.ServerHostname) {
		return fmt.Errorf("server_hostname %q is not a hostname", o.ServerHostname)
	}
	if len(o.ServerHostname) > serverHostnameLimit {
		return fmt.Errorf("server_hostname is %d bytes, above the %d the sname field holds", len(o.ServerHostname), serverHostnameLimit)
	}
	if len(o.BootFile) > bootFileLimit {
		return fmt.Errorf("boot_file is %d bytes, above the %d the file field holds", len(o.BootFile), bootFileLimit)
	}
	for _, opt := range o.Options {
		if err := validateOption(opt); err != nil {
			return err
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

// validatePools is §3.2's pool rules. A reservations_only scope is rendered
// with no pool, so it is the one kind that may have none; a pool it does
// give is checked like anyone else's.
func validatePools(s Scope, prefix netip.Prefix, classes []Class) error {
	if len(s.Pools) == 0 {
		if s.ReservationsOnly {
			return nil
		}
		return errors.New("pools: a scope needs at least one pool, unless it is reservations_only")
	}
	known := make(map[int64]bool, len(classes))
	for _, c := range classes {
		known[c.ID] = true
	}
	type span struct {
		i          int
		start, end netip.Addr
	}
	spans := make([]span, len(s.Pools))
	network, broadcast := prefix.Addr(), lastAddr(prefix)
	for i, p := range s.Pools {
		start, err := parseV4(p.Start)
		if err != nil {
			return fmt.Errorf("pools[%d]: start: %w", i, err)
		}
		end, err := parseV4(p.End)
		if err != nil {
			return fmt.Errorf("pools[%d]: end: %w", i, err)
		}
		if !prefix.Contains(start) || !prefix.Contains(end) {
			return fmt.Errorf("pools[%d]: %s-%s is not inside %s", i, p.Start, p.End, s.CIDR)
		}
		if start.Compare(end) > 0 {
			return fmt.Errorf("pools[%d]: start %s is above end %s", i, p.Start, p.End)
		}
		// Neither end of a pool may be the subnet's own two addresses. They
		// are not host addresses, and a client handed one would answer to
		// every broadcast on the segment. (On a /31 or /32 that leaves no
		// pool at all, which is the honest answer: there is no room for one.)
		if start == network || end == network || start == broadcast || end == broadcast {
			return fmt.Errorf("pools[%d]: %s-%s includes the network or broadcast address of %s", i, p.Start, p.End, s.CIDR)
		}
		if p.ClassID != 0 && !known[p.ClassID] {
			return &UnknownClassError{Pool: i, ID: p.ClassID}
		}
		spans[i] = span{i: i, start: start, end: end}
	}
	// Two pools sharing an address would have the engine refuse the config
	// — or, across two classes, hand one address to whichever class asked
	// first. Sorted by start, any overlap shows between neighbours: a pool
	// that overlaps an earlier one overlaps every pool starting in between.
	slices.SortFunc(spans, func(a, b span) int { return a.start.Compare(b.start) })
	for k := 1; k < len(spans); k++ {
		if prev, next := spans[k-1], spans[k]; next.start.Compare(prev.end) <= 0 {
			i, j := min(prev.i, next.i), max(prev.i, next.i)
			return fmt.Errorf("pools[%d] %s-%s overlaps pools[%d] %s-%s", i, s.Pools[i].Start, s.Pools[i].End, j, s.Pools[j].Start, s.Pools[j].End)
		}
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
		return fmt.Errorf("option code %d is %s, which is set by name", o.Code, named)
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
