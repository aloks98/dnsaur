package zones

import (
	"fmt"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
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
	if suffix := "." + zoneName; strings.HasSuffix(name, suffix) {
		if rel := strings.TrimSuffix(name, suffix); rel != "" {
			return rel
		}
	}
	return name
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

// BuildRecord validates w against zone and its existing records and returns
// the store.ZoneRecord ready to write. existing is every record already in
// the zone (the full set, not pre-filtered by name); selfID and hasSelf
// identify the record being replaced by a PUT so it is excluded from the
// sibling/RRSet checks below — otherwise replacing a record would always
// conflict with itself.
//
// Check order is part of the contract, not an implementation detail: parse
// (invalid), TTL range (invalid), apex CNAME (conflict), CNAME siblings both
// directions (conflict), RRSet TTL match (conflict). See
// docs/superpowers/specs/2026-08-08-zones-design.md §6.
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

	// Validation is dns.NewRR itself, via the same ToRR the resolver uses to
	// build the RR it serves. One validator, so an accepted record is by
	// construction a servable one — there is no second copy to drift from
	// the parser.
	rr, err := ToRR(RecordFQDN(zone.Name, name), rec)
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
