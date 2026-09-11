package store

import (
	"context"
	"database/sql"
	"errors"
)

// TSIGKey authenticates a zone transfer (RFC 8945). Unlike AuthToken, whose
// hash only ever needs comparing, Secret must be recoverable — the server
// signs outgoing and verifies incoming messages with it — so it is stored in
// the clear (see the 0008 migration's comment). It is never marshalled to
// JSON by the store package itself; callers that expose it over the API are
// responsible for deciding what, if anything, leaves the server.
type TSIGKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`      // owner name of the TSIG RR, canonical form (lowercase, trailing dot)
	Algorithm string `json:"algorithm"` // e.g. "hmac-sha256." — miekg's own constants, trailing dot included
	Secret    string `json:"secret"`    // base64
	CreatedAt int64  `json:"created_at"`
}

// TSIGKeyStore manages TSIG keys used to authenticate zone transfers.
type TSIGKeyStore interface {
	List(ctx context.Context) ([]TSIGKey, error)
	Get(ctx context.Context, id int64) (TSIGKey, bool, error)
	// ByName looks a key up by its TSIG RR owner name — the lookup a signed
	// message actually arrives with.
	ByName(ctx context.Context, name string) (TSIGKey, bool, error)
	Create(ctx context.Context, k TSIGKey) (int64, error)
	Update(ctx context.Context, k TSIGKey) error
	Delete(ctx context.Context, id int64) error
}

type tsigKeyStore struct{ s *sqlStore }

const tsigKeyColumns = `id, name, algorithm, secret, created_at`

func scanTSIGKey(row interface{ Scan(...any) error }, k *TSIGKey) error {
	return row.Scan(&k.ID, &k.Name, &k.Algorithm, &k.Secret, &k.CreatedAt)
}

func (t *tsigKeyStore) List(ctx context.Context) ([]TSIGKey, error) {
	rows, err := t.s.db.QueryContext(ctx, `SELECT `+tsigKeyColumns+` FROM tsig_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil (not `var out []TSIGKey`) so zero keys marshals to JSON `[]`,
	// not `null` — matches every other list endpoint (see sql.go's
	// Groups/Clients/Lists/Rules/records.All, and ListAPI in tokenstore.go).
	// A brand-new instance starts with no TSIG keys, so this is the default
	// state, not an edge case.
	out := []TSIGKey{}
	for rows.Next() {
		var k TSIGKey
		if err := scanTSIGKey(rows, &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (t *tsigKeyStore) Get(ctx context.Context, id int64) (TSIGKey, bool, error) {
	var k TSIGKey
	row := t.s.db.QueryRowContext(ctx, t.s.q(`SELECT `+tsigKeyColumns+` FROM tsig_keys WHERE id = ?`), id)
	err := scanTSIGKey(row, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return TSIGKey{}, false, nil
	}
	return k, err == nil, err
}

func (t *tsigKeyStore) ByName(ctx context.Context, name string) (TSIGKey, bool, error) {
	var k TSIGKey
	row := t.s.db.QueryRowContext(ctx, t.s.q(`SELECT `+tsigKeyColumns+` FROM tsig_keys WHERE name = ?`), name)
	err := scanTSIGKey(row, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return TSIGKey{}, false, nil
	}
	return k, err == nil, err
}

func (t *tsigKeyStore) Create(ctx context.Context, k TSIGKey) (int64, error) {
	return t.s.configInsert(ctx, `INSERT INTO tsig_keys (name, algorithm, secret, created_at) VALUES (?, ?, ?, ?)`,
		k.Name, k.Algorithm, k.Secret, k.CreatedAt)
}

// Update replaces a key's three fields, refusing a *rename* while a zone
// still names the key in allow_transfer or notify_to — ErrInUse, which the
// API answers 409.
//
// It is Delete's guard applied to the other way a reference can be broken.
// Both of those columns hold the key's name as text, so renaming a key
// referenced by one leaves the zone naming a key that does not exist, and
// every signed transfer or NOTIFY under it is refused with nothing on the
// zone to say why — exactly the failure Delete refuses to create, reached by
// editing instead of deleting.
//
// zones.tsig_key_id is deliberately not part of the guard: that reference is
// by id and survives a rename untouched. Refusing on it would block an edit
// that breaks nothing.
//
// The `name = ?` disjunct is what keeps everything else editable. A write
// that keeps the name — rotating a secret, correcting an algorithm, which is
// most of what PUT is for — never consults the reference checks at all. As
// in Delete, the predicate lives in the UPDATE rather than in a SELECT
// before it, so nothing can slip between the check and the write.
func (t *tsigKeyStore) Update(ctx context.Context, k TSIGKey) error {
	n, err := t.s.configExecN(ctx,
		`UPDATE tsig_keys SET name = ?, algorithm = ?, secret = ? WHERE id = ?
		   AND (name = ?
		        OR (NOT EXISTS (`+aclKeyRef(t.s.dialect)+`)
		            AND NOT EXISTS (`+notifyKeyRef(t.s.dialect)+`)))`,
		k.Name, k.Algorithm, k.Secret, k.ID, k.Name)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// Nothing was updated, and the two reasons for that are different
	// answers to the caller — the same split Delete makes: the key never
	// existed (404), or it is spoken for by name (409).
	if _, found, err := t.Get(ctx, k.ID); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	return ErrInUse
}

// aclKeyRef reports the SQL that finds a zone whose allow_transfer names
// tsig_keys.name. Both halves are wrapped in commas so a name cannot match a
// longer one that contains it, and the substring function is dialect-specific
// because LIKE would treat an underscore in a key name -- legal in a domain
// name -- as a wildcard, silently refusing to delete unrelated keys.
func aclKeyRef(dialect string) string {
	fn := "instr"
	if dialect == "postgres" {
		fn = "strpos"
	}
	return `SELECT 1 FROM zones WHERE ` + fn +
		`(',' || replace(zones.allow_transfer, ' ', '') || ',', ',key:' || tsig_keys.name || ',') > 0`
}

// notifyKeyRef reports the SQL that finds a zone whose notify_to names
// tsig_keys.name. It is aclKeyRef's twin on the second column that can
// reference a key, and the two are separate functions rather than one
// parameterised by column name because each carries its own reasoning about
// the format it is matching inside.
//
// The format differs from allow_transfer's in one way that matters here: an
// entry is `host:port key:name`, so the key is preceded by a space rather
// than by the entry separator. Stripping spaces as aclKeyRef does would join
// the host to the key and stop `,key:` ever matching — so this strips nothing
// and anchors on ' key:' instead, which the canonical spelling guarantees is
// exactly how FormatNotifyTo writes it.
func notifyKeyRef(dialect string) string {
	fn := "instr"
	if dialect == "postgres" {
		fn = "strpos"
	}
	return `SELECT 1 FROM zones WHERE ` + fn +
		`(zones.notify_to || ',', ' key:' || tsig_keys.name || ',') > 0`
}

// Delete removes a key unless a zone still names it, in which case it
// returns ErrInUse and the API answers 409. A secondary that lost its key
// would keep trying to transfer and keep being refused by its primary, with
// nothing on the zone to say why — so the refusal belongs at the moment the
// key would go, where there is still something to say.
//
// This is the whole enforcement of zones.tsig_key_id: there is no foreign
// key on the column, for the reasons the 0009 migration records at length.
//
// What the single statement buys, stated narrowly because the obvious
// stronger claim is false. The NOT EXISTS is evaluated inside the DELETE,
// which removes the window between a separate SELECT and this DELETE. That
// is worth having and costs nothing. It does not close the window that
// actually matters, and that window is not in this function: the API checks
// the key exists (checkZoneTransferConfig, zones_handlers.go) and inserts
// the zone as two separate statements, and no statement here can observe a
// row that has not been written yet. Measured rather than assumed — on
// postgres, concurrent AddZone and Delete leave a zone naming a deleted key
// in the large majority of attempts.
//
// **sqlite narrows that window; it does not escape it.**
// SetMaxOpenConns(1) serialises individual statements, not the handler's
// check-then-insert sequence — the connection goes back to the pool between
// the two, and the handler spends that gap on SOA defaults and name
// normalisation. Measured there too: a few attempts in sixty land wrong
// with a deliberate pause in the gap, none in four hundred without one. So
// it is a narrower race on sqlite and a likely one on postgres, not a
// postgres-only bug.
//
// It is not fixable from here, and not cheaply fixable anywhere. Putting
// the check and the insert in one transaction does not help by itself: at
// READ COMMITTED the check takes no lock, so the two still interleave.
// Three things would actually close it, and each costs more than it saves
// here — a schema-level foreign key (which 0009 declines, and this is that
// decision's concrete cost); SELECT ... FOR UPDATE around the check, which
// is not sqlite syntax and would move the reference rule into the
// zone-insert path, leaving two owners of one rule; or SERIALIZABLE
// isolation on postgres, which does detect this write skew and would abort
// one of the two transactions, at the price of retry handling on a path
// that has none and an isolation level nothing else in this codebase uses.
//
// What the residual costs when it happens: the zone names an id nothing
// answers to, and its next transfer fails to find a key to sign with. That
// is a named failure reported against the attempt that suffered it, the
// same way an unreachable primary surfaces — not silent breakage.
func (t *tsigKeyStore) Delete(ctx context.Context, id int64) error {
	n, err := t.s.configExecN(ctx,
		`DELETE FROM tsig_keys WHERE id = ?
		   AND NOT EXISTS (SELECT 1 FROM zones WHERE zones.tsig_key_id = tsig_keys.id)
		   AND NOT EXISTS (`+aclKeyRef(t.s.dialect)+`)
		   AND NOT EXISTS (`+notifyKeyRef(t.s.dialect)+`)`, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// Nothing was deleted, and the two reasons for that are different
	// answers to the caller: the key never existed (404), or it is spoken
	// for (409).
	if _, found, err := t.Get(ctx, id); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	return ErrInUse
}
