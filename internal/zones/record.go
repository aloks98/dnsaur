package zones

import (
	"fmt"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// The rules a record has to satisfy to be written into a zone, and the diff
// that turns one set of records into another.
//
// Both lived in internal/api until Milestone D2, next to the REST handlers
// that were their only callers. They moved here when the zone transfer
// arrived, and the direction of the move was not a choice: internal/api
// imports internal/zones, so a transfer living here could reach them only
// by growing a second copy of the rules. An automatic write path with its
// own copy is this project's most repeated defect — Milestones B and C each
// shipped one, three times in B alone — so there is one validator and one
// differ with three callers: the hand write (POST/PUT /zones/{id}/records),
// the zone-file import (POST /zones/{id}/file), and the AXFR transfer.
//
// What stayed in internal/api is the HTTP half: which status code a
// refusal becomes. That is a question about REST, and the answer is derived
// from RecordProblem.Conflict rather than decided here.

// maxRecordTTL is the largest TTL RFC 2181 §8 allows a record to carry: the
// top of a signed 32-bit range. A resolver reads anything above this as its
// two's-complement wraparound — for a value with the high bit set, that
// wraps to zero, meaning "never cache", the opposite of what was typed.
const maxRecordTTL uint32 = 2147483647

// RecordWrite is one record as its writer has it, before validation: a name
// in whatever form they had it, rdata as typed or as it came off the wire.
// BuildRecord turns it into the store.ZoneRecord that may actually be
// stored, or says why it may not.
type RecordWrite struct {
	Name  string
	Type  string
	TTL   uint32
	RData string
	// Enabled is a pointer so "not sent" differs from "false" — the API's
	// request body carries that distinction and passes it through unchanged.
	Enabled *bool
	Comment string
}

// RecordProblem is why BuildRecord refused a record.
//
// Conflict separates the two kinds of refusal, because they are two
// different answers over HTTP and this package must not be the one that
// knows that: false is a value that is wrong on its own terms — rdata that
// does not parse, a TTL out of range — which the API answers 400; true is a
// record that is fine in isolation and cannot coexist with one already in
// the zone, which the API answers 409.
type RecordProblem struct {
	Msg      string
	Conflict bool
}

func (p *RecordProblem) Error() string { return p.Msg }

// invalidRecord and conflictingRecord are the two constructors, named so a
// reader of BuildRecord sees which kind of refusal each check is without
// having to decode a boolean at every call site.
func invalidRecord(format string, args ...any) error {
	return &RecordProblem{Msg: fmt.Sprintf(format, args...)}
}

func conflictingRecord(msg string) error {
	return &RecordProblem{Msg: msg, Conflict: true}
}

// RelRecordName lowercases a record name and resolves it to zoneName-relative
// form. "" and the zone apex both fold to "@" — the convention
// zone_records.name and the rest of this package already use (apexName).
//
// A name that already carries the zone's own apex as a suffix — a fully
// qualified name typed out of habit, e.g. "www.e412.in" (or "www.e412.in.")
// in zone "e412.in" — has that suffix stripped so it lands on the same
// relative name as "www". Left un-stripped, RecordFQDN would silently double
// it into "www.e412.in.e412.in": it parses (ToRR doesn't know any better), so
// nothing else would catch it.
//
// This is not RelName, and the two are not interchangeable. RelName answers
// "what is this qname relative to that apex" for a name already established
// to be inside the zone, and leaves a name that is not alone. This one is
// the write-side normaliser: it accepts anything a caller might have written,
// including an empty string, and always produces a storable relative name. A
// caller that needs to know whether a name is inside the zone at all has to
// ask separately — see Transferrer.build, which does.
func RelRecordName(raw, zoneName string) string {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	zoneName = strings.ToLower(strings.TrimSuffix(zoneName, "."))
	if name == "" || name == zoneName {
		return apexName
	}
	// RelName strips the apex by whole labels and returns a name that is not
	// inside the zone unchanged, which is exactly the two cases this needs.
	// Stripping the byte suffix instead would turn `foo\.e412.in` — one
	// escaped label under "in", not a name in this zone at all — into the
	// record `foo\`.
	return RelName(name, zoneName)
}

// RecordFQDN rebuilds a record's owner FQDN from its zone-relative name —
// the same "relative + apex" construction answer.go uses when it builds an
// owner name to hand to ToRR. The result carries no trailing dot; callers
// that need one apply dns.Fqdn.
func RecordFQDN(zoneName, relName string) string {
	if relName == apexName {
		return zoneName
	}
	return relName + "." + zoneName
}

// forbiddenNameChars are the characters a relative record name may not
// contain. dns.IsDomainName rules out none of them — it documents itself as
// "extremely liberal — almost any string is a valid domain name" — and every
// one of them changes how the line Render writes is read back:
//
//	space, tab, CR, LF   split the line into different fields
//	;                    makes the rest of the line a comment
//	" ( )                open a quoted string or a multi-line group
//	\                    escapes the next character, so a stored `a\.b` is
//	                     one label that every label-counting reader here
//	                     (RelName, owns, Index.Find) has to agree about
//	/                    never appears in a hostname, and is how a path
//	                     typed into the wrong field arrives
//
// The first three groups are the reason a record was storable, servable and
// impossible to reimport; ';' as the first character was the reason a write
// could return no response at all (see ToRR).
const forbiddenNameChars = " \t\r\n;\"()\\/"

// validRecordName reports whether name — already relative to the apex, as
// RelRecordName returns it — is one this zone can hold, serve and export.
//
// It is normalizeZoneName's check (internal/api/zones_handlers.go) applied to
// the other half of a record's name, which is what makes the hand write, the
// zone-file import and the AXFR transfer agree about it: they share this
// validator and have no second copy to drift from.
//
// The apex, a wildcard label and a leading '_' are all allowed, because all
// three are names this server already holds: "@", "*.nexus", "_sip._tcp".
func validRecordName(name string) bool {
	if name == apexName {
		return true
	}
	if name == "" || strings.ContainsAny(name, forbiddenNameChars) {
		return false
	}
	// A '$' only opens a directive as the first token on a line, which is
	// exactly where Render writes the name: a record called "$ttl" exports
	// as a line the parser reads as $TTL.
	if strings.HasPrefix(name, "$") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return false
		}
	}
	_, ok := dns.IsDomainName(name)
	return ok
}

// resolvesBareAt reports whether rdata carries a standalone "@" token, the
// one spelling whose meaning depends on the origin the line is read under.
//
// The test is deliberately coarse: an "@" inside a quoted string matches it
// too, and all that costs is reading such a line under the zone's origin
// instead of the root, which changes nothing about a quoted string.
func resolvesBareAt(rdata string) bool {
	for _, f := range strings.Fields(rdata) {
		if f == "@" {
			return true
		}
	}
	return false
}

// rrForWrite parses a record being written, reading a standalone "@" in its
// rdata as the zone apex rather than as the root.
//
// "@" means the origin, and dns.NewRR — which is ToRR, and the root — is not
// the origin a record of this zone is written under. A master file loaded
// into the same zone resolves the identical token to the apex, so `10 @`
// stored as the null MX `10 .` (RFC 7505) by one writer and as `10 e412.in.`
// by the other is one rdata spelling with two meanings, the drift RDataOf's
// comment says it exists to prevent.
//
// The substitution is miekg's rather than a replacement of the text, because
// "@" is a name only where the type says it is: `MX 10 @` names the apex,
// while `TXT @` is the one-character string "@" on both paths and has to
// stay that way. Reading the line under the zone's origin is what makes this
// path produce, for every type, what the import path already produces.
//
// Only the "@" case takes this route. Every other relative name in rdata is
// read under the root here by design — a bare "nas" is served as "nas." and
// stored as "nas." so that the stored text says what is served (see the
// rdata comment in BuildRecord) — and reading those under the zone origin
// would silently move them.
func rrForWrite(fqdn, apex string, rec store.ZoneRecord) (dns.RR, error) {
	if !resolvesBareAt(rec.RData) {
		return ToRR(fqdn, rec)
	}
	line := fmt.Sprintf("%s %d IN %s %s\n", dns.Fqdn(fqdn), rec.TTL, rec.Type, rec.RData)
	zp := dns.NewZoneParser(strings.NewReader(line), dns.Fqdn(apex), "")
	rr, ok := zp.Next()
	if err := zp.Err(); err != nil {
		return nil, err
	}
	if !ok || rr == nil {
		return nil, errParsesToNothing
	}
	return rr, nil
}

// BuildRecord validates w against zone and its existing records and returns
// the store.ZoneRecord ready to write. existing is every record already in
// the zone (the full set, not pre-filtered by name); selfID and hasSelf
// identify the record being replaced by a PUT so it is excluded from the
// sibling/RRSet checks below — otherwise replacing a record would always
// conflict with itself.
//
// Check order is part of the contract, not an implementation detail: name
// (invalid), type (invalid), parse (invalid), TTL range (invalid), apex
// CNAME (conflict), CNAME siblings both directions (conflict), RRSet TTL
// match (conflict). See docs/superpowers/specs/2026-08-08-zones-design.md §6.
//
// Every error is a *RecordProblem.
func BuildRecord(zone store.Zone, w RecordWrite, existing []store.ZoneRecord, selfID int64, hasSelf bool) (store.ZoneRecord, error) {
	name := RelRecordName(w.Name, zone.Name)
	recType := strings.ToUpper(strings.TrimSpace(w.Type))
	enabled := true
	if w.Enabled != nil {
		enabled = *w.Enabled
	}
	rec := store.ZoneRecord{
		ZoneID:  zone.ID,
		Name:    name,
		Type:    recType,
		TTL:     w.TTL,
		RData:   w.RData,
		Enabled: enabled,
		Comment: w.Comment,
	}

	// A name is not just rdata's owner: it is written at the start of a line
	// on every export and read back off one on every import, and the parser
	// tolerates far more there than survives that trip. See validRecordName.
	if !validRecordName(name) {
		return store.ZoneRecord{}, invalidRecord("name %q is not a valid record name", w.Name)
	}

	// RFC 1034 §4.1.1: a zone has exactly one SOA, and this one's lives on
	// the zones row because its serial needs managed increments. A record row
	// of type SOA is therefore a second SOA — ignored by the apex answer,
	// written out by Render beside the real one, and rejected by Parse on the
	// way back in, so a single accepted write breaks the export/import round
	// trip spec §8 rests on. Off the apex it is worse: it is served with AA
	// set, claiming an authority this server does not have.
	if recType == "SOA" {
		return store.ZoneRecord{}, invalidRecord("the SOA lives on the zone, not in its records")
	}

	// A type miekg/dns cannot name is a type this server cannot serve.
	// dns.NewRR accepts RFC 3597's TYPEnnn syntax and hands back an
	// *dns.RFC3597, which then breaks every path that reads the row: RDataOf
	// stores the entire RR text (RFC3597 prints its class as CLASS1 where the
	// header prints IN, so the prefix trim matches nothing), rrType maps the
	// stored "TYPE65280" to 0 so no query ever matches it, and Render exports
	// the result. Spec §3 promises the types miekg/dns parses, not a way to
	// carry opaque ones, so this is refused rather than made to work.
	if _, known := dns.StringToType[recType]; !known {
		return store.ZoneRecord{}, invalidRecord("unsupported record type %q", w.Type)
	}

	// Validation is dns.NewRR itself, via the same ToRR the resolver uses to
	// build the RR it serves. One validator, so an accepted record is by
	// construction a servable one — there is no second copy to drift from
	// the parser.
	rr, err := rrForWrite(RecordFQDN(zone.Name, name), zone.Name, rec)
	if err != nil {
		return store.ZoneRecord{}, invalidRecord("%s", err.Error())
	}

	// What gets stored is the rdata that parse produced, not the text that
	// was typed. The two are not interchangeable, because rdata is read back
	// in two places that do not agree about what a given string means:
	// ToRR reads it under no origin, where "nas.e412.in" is already
	// absolute, and Render writes it into a master file under
	// "$ORIGIN <zone>.", where that identical text is *relative* and means
	// nas.e412.in.<zone>. Keeping the raw text let one record be served at
	// one target and exported pointing at another — and a dotless absolute
	// target is the spelling users arrive with, since Cloudflare and Route
	// 53 both accept it.
	//
	// Every type is normalised, not just the ones whose rdata embeds a
	// domain name, because origin ambiguity is not the only way raw text
	// says more than the RR does: dns.NewRR reads one RR and silently
	// discards whatever follows it, so an A record's rdata can carry a
	// trailing newline and a second record's worth of text that validates,
	// serves as nothing, and lands verbatim in the exported file. Scoping
	// this to name-valued types would leave that standing, and would need a
	// type list that goes stale as miekg/dns gains types. The cost is that
	// a spelling with no ambiguity in it is still rewritten to the server's
	// own ("hello" gains its quotes, an expanded IPv6 address contracts) —
	// a change to how the value is written down, never to what it answers.
	//
	// This is a no-op on the import path. Parse already derives
	// ParsedRecord.RData through exactly this call (classify), so a record
	// arriving from a zone file is normalised before it gets here, and
	// spec §8's objection to normalising an imported file does not reach
	// it — there is nothing left to normalise. The same is true of a record
	// arriving over AXFR, which Transferrer.build spells through RDataOf.
	rec.RData = RDataOf(rr)

	// An rdata that parses to nothing is not a record. It gets this far
	// because a value that is entirely a ';' comment, or only whitespace,
	// is not a parse error for every type — dns.NewRR reads "TXT ; note" as
	// a TXT whose rdata is simply absent, and says nothing. Stored, that row
	// renders as "note 300 IN TXT " with nothing after the type, which no
	// parser reads back, so the zone would export to a file it cannot
	// reimport — the round trip spec §8 rests on. Still part of "the parser
	// decides", just the half of its answer that is carried in the rdata it
	// produced rather than in an error.
	if rec.RData == "" {
		return store.ZoneRecord{}, invalidRecord("rdata is empty: it must carry the record's value, not only a comment")
	}

	// RFC 2181 §8: reject a TTL a resolver would not read back as typed.
	if rec.TTL > maxRecordTTL {
		return store.ZoneRecord{}, invalidRecord("ttl must not exceed %d", maxRecordTTL)
	}

	// RFC 1912 §2.4: no CNAME at the zone apex. The zone's SOA lives on the
	// zones row, not a zone_records row, so the sibling check below would
	// see an apex with nothing recorded there and miss this on its own.
	if name == apexName && recType == "CNAME" {
		return store.ZoneRecord{}, conflictingRecord("CNAME is not allowed at the zone apex")
	}

	for _, sib := range existing {
		if sib.Name != name || (hasSelf && sib.ID == selfID) {
			continue
		}
		// RFC 1034 §3.6.2: a CNAME must be the only record at its name, in
		// both write orders — a CNAME landing beside an existing record, or
		// a record landing beside an existing CNAME.
		if recType == "CNAME" || sib.Type == "CNAME" {
			return store.ZoneRecord{}, conflictingRecord("CNAME cannot coexist with another record at the same name")
		}
		// RFC 2181 §5.2: every RR in an RRSet (same name, same type) must
		// share one TTL, or the zone answers differently depending on which
		// row a lookup happens to read first.
		if sib.Type == recType && sib.TTL != rec.TTL {
			return store.ZoneRecord{}, conflictingRecord("records in the same RRSet must share one TTL")
		}
	}

	return rec, nil
}

// RecordChange is one record replaced in place: same name, type and rdata,
// different TTL or enabled state. Both sides are kept because making a
// destructive replace legible before it happens is the import dry run's
// entire job — "3 changed" with no values is not that.
type RecordChange struct {
	From store.ZoneRecord `json:"from"`
	To   store.ZoneRecord `json:"to"`
}

// RecordDiff is what turning one set of records into another would do. Every
// field is non-nil even when empty, because the API marshals this diff
// straight into its import response and a bucket with nothing in it has to
// come out as `[]` rather than `null` (the convention every list-shaped
// response in this codebase follows — see clientStore.Groups in store/sql.go).
type RecordDiff struct {
	Add    []store.ZoneRecord `json:"add"`
	Change []RecordChange     `json:"change"`
	Delete []store.ZoneRecord `json:"delete"`
}

// recordIdentity is what makes two rows the same record for diffing: the
// RR itself. TTL and Enabled are the two attributes a replace can change
// on a record that stays; a different name, type or rdata is a different
// record, and shows up as a delete plus an add.
type recordIdentity struct{ name, recType, rdata string }

// identify keys rec by its canonical RR. The rdata is normalized through
// ToRR — the same round trip Parse's records have been through (classify),
// and, since the rdata-normalisation fix, the one every new write is stored
// in too. It is still done here rather than assumed, because the store holds
// rows written before that fix and no migration rewrote them: "ns.e412.in"
// and "ns.e412.in.", or a bare `hello` and a quoted `"hello"`, are one RR
// spelled two ways, and compared literally they would come out as a delete
// and an add of the same record on every single replace, churning row ids
// and burying the one real change in a page of noise.
//
// A row ToRR cannot parse falls back to its literal rdata rather than
// failing the diff: it can only mean a record already in the store is
// unservable, which is not the incoming zone's fault and not something a
// diff can fix. It compares equal to nothing, so the replace replaces it.
func identify(zoneName string, rec store.ZoneRecord) recordIdentity {
	rdata := rec.RData
	if rr, err := ToRR(RecordFQDN(zoneName, rec.Name), rec); err == nil {
		rdata = RDataOf(rr)
	}
	return recordIdentity{name: rec.Name, recType: rec.Type, rdata: rdata}
}

// DiffRecords reports what turning existing into want would do, in the three
// buckets store.ZoneStore.ReplaceRecords applies.
//
// Records are matched by identity (name, type, rdata) rather than by row
// id — neither a zone file nor an AXFR stream carries ids — and matched one
// for one, because nothing in the schema stops a zone holding two identical
// rows: zone_records has an index on (zone_id, name, type) but no uniqueness
// constraint anywhere (migration 0004). Counting matches rather than testing
// membership is what keeps two copies of a record in the zone and one in the
// incoming set from looking like no change at all.
func DiffRecords(zoneName string, existing, want []store.ZoneRecord) RecordDiff {
	diff := RecordDiff{
		Add:    []store.ZoneRecord{},
		Change: []RecordChange{},
		Delete: []store.ZoneRecord{},
	}

	unmatched := map[recordIdentity][]store.ZoneRecord{}
	for _, rec := range existing {
		key := identify(zoneName, rec)
		unmatched[key] = append(unmatched[key], rec)
	}

	matched := map[int64]bool{}
	for _, rec := range want {
		key := identify(zoneName, rec)
		pool := unmatched[key]
		if len(pool) == 0 {
			diff.Add = append(diff.Add, rec)
			continue
		}
		old := pool[0]
		unmatched[key] = pool[1:]
		matched[old.ID] = true

		// The record survives, so it keeps its row id, and its comment:
		// neither a master file nor an AXFR stream has any way to carry one,
		// so treating their silence as "no comment" would delete a note they
		// could never have preserved in the first place. Enabled comes from
		// the incoming set unconditionally — a record present in a master
		// file is a record that is served (Render omits disabled ones for
		// exactly that reason), so importing a file that lists a
		// currently-disabled record enables it.
		rec.ID = old.ID
		rec.Comment = old.Comment
		if rec.TTL != old.TTL || rec.Enabled != old.Enabled {
			diff.Change = append(diff.Change, RecordChange{From: old, To: rec})
		}
	}

	// Whatever no incoming record claimed. Tested by row id rather than by
	// re-counting identities, because with duplicate rows the two are not
	// the same question: recounting would name the first row carrying a
	// leftover identity, which is the row the loop above just matched and
	// may be about to update. Deleting that one and keeping its twin leaves
	// the right number of records standing, so the zone still looks
	// correct — but the update lands on a row that is already gone.
	//
	// existing is iterated rather than the map so deletes come back in the
	// zone's own row order, the order every other endpoint lists records
	// in; ranging a Go map would shuffle them on every request.
	for _, rec := range existing {
		if !matched[rec.ID] {
			diff.Delete = append(diff.Delete, rec)
		}
	}
	return diff
}

// DeleteIDs is the delete half of a diff in the form ReplaceRecords takes.
// The store deletes by row id and nothing else, so that is what it is
// handed: a whole record would carry six other fields it must not act on.
func (d RecordDiff) DeleteIDs() []int64 {
	ids := make([]int64, 0, len(d.Delete))
	for _, rec := range d.Delete {
		ids = append(ids, rec.ID)
	}
	return ids
}

// Updates is the change half of a diff in the form ReplaceRecords takes.
// Only the "to" side of each change is written; the "from" side exists for
// the import's diff report, not for the store.
func (d RecordDiff) Updates() []store.ZoneRecord {
	recs := make([]store.ZoneRecord, 0, len(d.Change))
	for _, ch := range d.Change {
		recs = append(recs, ch.To)
	}
	return recs
}
