package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// zoneFileMaxBytes caps an import request body. It matches the cap the
// shared decode helper (server.go) applies to every endpoint, and is not
// raised: import validates each record against every record accepted
// before it, so cost grows with the square of the file, and a megabyte of
// zone file — roughly 35,000 records — already takes seconds. A larger cap
// would not buy a working import of a larger file, it would move the
// failure from an immediate, legible rejection to a request that runs long
// enough to be killed by something else.
const zoneFileMaxBytes = 1 << 20

func (s *Server) zoneFileRoutes() {
	s.route("GET /api/v1/zones/{id}/file", s.requireAuth(s.handleZoneFileExport))
	s.route("POST /api/v1/zones/{id}/file", s.requireAuth(s.handleZoneFileImport))
}

// handleZoneFileExport renders a zone as a standard BIND master file
// (zones.Render, Task 1) and hands it back as a download. Built-in zones
// (RFC 6303) are read-only for writes but not for reads — see the "internal"
// checks in zones_handlers.go and zonerecords_handlers.go, none of which
// apply here.
func (s *Server) handleZoneFileExport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	recs, err := s.deps.Store.Zones().Records(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	body := zones.Render(zone, recs)

	w.Header().Set("Content-Type", "text/dns; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zone"`, zone.Name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// zoneFileImport is the request body for POST /zones/{id}/file.
//
// DryRun is required: a request that omits it is rejected with 400, not
// treated as either value. It is a *bool for the same reason zoneCreate
// and zoneRecordWrite use one — so "not sent" is distinguishable from
// "false" — but the third state means something stronger here than it does
// there. Spec §8 makes the two-step dry run *the* safety mechanism for a
// destructive whole-zone replace, and Go's zero value for a bool is the
// committing one, so a field the client never sent would otherwise be what
// selects the destructive branch. Defaulting to true instead was the
// alternative and is worse: it answers 200 with a diff to a caller that
// asked to commit, which reads as success and leaves the zone unchanged.
// Refusing outright is the only answer that cannot be misread, and
// openapi.yaml already declares the field required.
type zoneFileImport struct {
	Content string `json:"content"`
	DryRun  *bool  `json:"dry_run"`
}

// zoneRecordChange is one record an import replaces in place: same name,
// type and rdata, different TTL or enabled state. Both sides are reported
// because making a destructive replace legible before it happens is the
// dry run's entire job — "3 changed" with no values is not that.
type zoneRecordChange struct {
	From store.ZoneRecord `json:"from"`
	To   store.ZoneRecord `json:"to"`
}

// zoneFileImportResult is what an import answers with, dry run or not: the
// three-way diff between the zone as it is and the file as it would
// replace it, or — when the file is rejected — one message per problem and
// three empty lists.
//
// Every field is initialized non-nil so a diff with nothing in one bucket
// marshals to `[]` rather than `null`, the convention the rest of the API's
// list-shaped responses follow (see clientStore.Groups in store/sql.go).
type zoneFileImportResult struct {
	Add    []store.ZoneRecord `json:"add"`
	Change []zoneRecordChange `json:"change"`
	Delete []store.ZoneRecord `json:"delete"`
	Errors []string           `json:"errors"`
	// Error is a one-line summary present only on a rejection, so this
	// response still satisfies the API-wide rule that an error is a flat
	// {"error": "<message>"} envelope (docs/api.md, "Conventions") — a
	// client with one error handler for every endpoint finds what it
	// expects here too. It cannot replace Errors: spec §8 requires naming
	// every offending line, and one string is not a list.
	Error string `json:"error,omitempty"`
}

// errZoneFileRejected is the summary that accompanies a rejected import.
// Deliberately says nothing about what was wrong — the per-line messages
// in Errors do that, and duplicating the first of them here would read as
// though it were the only one.
const errZoneFileRejected = "the zone file was rejected; nothing was written. See errors for every problem found."

func newZoneFileImportResult() zoneFileImportResult {
	return zoneFileImportResult{
		Add:    []store.ZoneRecord{},
		Change: []zoneRecordChange{},
		Delete: []store.ZoneRecord{},
		Errors: []string{},
	}
}

// recordIdentity is what makes two rows the same record for diffing: the
// RR itself. TTL and Enabled are the two attributes an import can change
// on a record that stays; a different name, type or rdata is a different
// record, and shows up as a delete plus an add.
type recordIdentity struct{ name, recType, rdata string }

// identify keys rec by its canonical RR. The rdata is normalized through
// zones.ToRR — the same round trip zones.Parse's records have been through
// (classify), and, since the rdata-normalisation fix, the one every new
// write is stored in too. It is still done here rather than assumed,
// because the store holds rows written before that fix and no migration
// rewrote them: "ns.e412.in" and "ns.e412.in.", or a bare `hello` and a
// quoted `"hello"`, are one RR spelled two ways, and compared literally
// they would come out as a delete and an add of the same record on every
// single import, churning row ids and burying the one real change in a
// page of noise.
//
// A row ToRR cannot parse falls back to its literal rdata rather than
// failing the import: it can only mean a record already in the store is
// unservable, which is not this request's fault and not something a diff
// can fix. It compares equal to nothing, so the import replaces it.
func identify(zoneName string, rec store.ZoneRecord) recordIdentity {
	rdata := rec.RData
	if rr, err := zones.ToRR(absoluteRecordName(zoneName, rec.Name), rec); err == nil {
		rdata = zones.RDataOf(rr)
	}
	return recordIdentity{name: rec.Name, recType: rec.Type, rdata: rdata}
}

// importRecordError names the record a validation failure is about. Line
// numbers are preferred, but zones.ParsedRecord.Line is 0 for every record
// of a file whose statement count and record count disagree — a $GENERATE
// file by construction — and "line 0" points at nothing. Such a record is
// named by what it actually is instead, which is the only handle its
// author has on it.
func importRecordError(pr zones.ParsedRecord, msg string) string {
	if pr.Line > 0 {
		return fmt.Sprintf("line %d: %s", pr.Line, msg)
	}
	return fmt.Sprintf("%s %s %s: %s", pr.Name, pr.Type, pr.RData, msg)
}

// readZoneFileImport decodes an import request body, distinguishing a body
// that is too large from one that is malformed, and returns the status and
// message to answer with when it is neither.
//
// The shared decode helper cannot make that distinction. It wraps the body
// in an io.LimitReader, which stops at the cap silently rather than
// reporting it, so an over-cap body reaches the JSON decoder as a document
// that simply ends mid-token — indistinguishable there from a genuinely
// truncated one, and answered "invalid json". On every other endpoint the
// body is an object some code generated, where a megabyte means something
// is wrong in a way that message describes well enough. Here it is a file
// the user picked, and telling them a valid zone file is malformed sends
// them hunting for a syntax error that does not exist.
//
// Scoped to this endpoint rather than fixed in decode itself: every
// endpoint in the package shares that helper, and this is the only one
// where the over-cap case is reachable in ordinary use.
func readZoneFileImport(r *http.Request) (zoneFileImport, int, string) {
	// One byte past the cap, which is what makes "at the limit" and "over
	// it" distinguishable at all — and bounds what an oversize request can
	// make this process allocate, the reason decode limits the read in the
	// first place.
	raw, err := io.ReadAll(io.LimitReader(r.Body, zoneFileMaxBytes+1))
	if err != nil {
		return zoneFileImport{}, http.StatusBadRequest, "invalid json"
	}
	if len(raw) > zoneFileMaxBytes {
		// 413 rather than another 400: "too large" and "malformed" are
		// different problems with different fixes, and a client that can
		// only tell them apart by matching on message text cannot really
		// tell them apart.
		return zoneFileImport{}, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large: the limit is %d bytes", zoneFileMaxBytes)
	}

	var body zoneFileImport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return zoneFileImport{}, http.StatusBadRequest, "invalid json"
	}
	return body, 0, ""
}

// handleZoneFileImport replaces a zone's contents with a BIND master file.
//
// A zone file is the zone, not a patch: anything in the zone and not in
// the file is deleted (spec §8, "Import: the file is the zone"). Merge
// semantics would produce a zone that is neither the file nor what was
// there before, with the specific trap that deleting a record from the
// file and re-importing does nothing. Because that is destructive, the
// request carries dry_run: true returns the diff and touches nothing,
// false applies it.
//
// Note that a *disabled* record is deleted like any other absent one. A
// master file cannot express "present but disabled", so export omits such
// rows (zones.Render) and import cannot see them — the two halves agree,
// and the dry run shows them in the delete list before anything happens.
//
// Validation is buildZoneRecord, the same function behind POST
// /zones/{id}/records, run against the file's own accumulating records
// rather than the zone's current ones — the file is what the zone is about
// to be, so that is what an RRSet TTL or CNAME sibling has to be judged
// against. Every failure is collected, not just the first, and any failure
// at all rejects the whole file with no writes: a partial import leaves a
// zone matching neither the file nor any intent, and a report of what was
// skipped is easy to miss.
func (s *Server) handleZoneFileImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// A built-in zone is seeded infrastructure (RFC 6303), not user content —
	// see internal/store/builtins.go. Reads are fine; writes are not, and an
	// import is the largest write there is.
	if zone.Type == "internal" {
		errJSON(w, http.StatusConflict, "built-in zones cannot be changed")
		return
	}
	body, code, msg := readZoneFileImport(r)
	if code != 0 {
		errJSON(w, code, msg)
		return
	}
	// Before anything is parsed, so a caller that forgot the field is told
	// so rather than handed a diff it might read as a completed import.
	if body.DryRun == nil {
		errJSON(w, http.StatusBadRequest, "dry_run is required: true returns the diff, false replaces the zone")
		return
	}

	result := newZoneFileImportResult()

	pz, parseErrs := zones.Parse(body.Content, zone.Name)
	if len(parseErrs) > 0 {
		result.Errors = parseErrs
		result.Error = errZoneFileRejected
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}

	// zones.Parse guarantees a non-nil SOA whenever it returns no errors
	// (see ParsedZone), so the file's SOA is available unchecked from here.
	built := make([]store.ZoneRecord, 0, len(pz.Records))
	for _, pr := range pz.Records {
		rec, _, msg, ok := buildZoneRecord(zone, zoneRecordWrite{
			Name:  pr.Name,
			Type:  pr.Type,
			TTL:   pr.TTL,
			RData: pr.RData,
		}, built, 0, false)
		if !ok {
			// The status code buildZoneRecord picked for a single write (400
			// for a bad value, 409 for a conflict with another record) is
			// dropped on purpose: this is one request carrying a whole zone,
			// its outcome is a list rather than a code, and the list is what
			// the caller acts on.
			result.Errors = append(result.Errors, importRecordError(pr, msg))
			continue
		}
		// Only records that passed accumulate. A record that failed is not
		// in the zone the file describes, so judging later records against
		// it would report conflicts with something that is not going to
		// exist.
		built = append(built, rec)
	}
	if len(result.Errors) > 0 {
		result.Error = errZoneFileRejected
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}

	existing, err := s.deps.Store.Zones().Records(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	result = diffZoneRecords(zone.Name, existing, built, result)

	if *body.DryRun {
		writeJSON(w, http.StatusOK, result)
		return
	}

	if err := s.applyZoneFile(r, zone, pz, result); err != nil {
		// ZoneStore has no transaction, so a storage failure part-way
		// through leaves the zone part-replaced. Reload anyway: whatever did
		// commit is what the resolver must now be serving, and leaving the
		// old snapshot up would answer from records the store no longer has.
		s.reloadZones(r)
		storeErr(w, err)
		return
	}
	s.reloadZones(r)
	writeJSON(w, http.StatusOK, result)
}

// diffZoneRecords fills result's add/change/delete lists with what turning
// existing into want would do.
//
// Records are matched by identity (name, type, rdata) rather than by row
// id — the file carries no ids — and matched one for one, because nothing
// in the schema stops a zone holding two identical rows: zone_records has
// an index on (zone_id, name, type) but no uniqueness constraint anywhere
// (migration 0004). Counting matches rather than testing membership is
// what keeps two copies of a record in the zone and one in the file from
// looking like no change at all.
func diffZoneRecords(zoneName string, existing, want []store.ZoneRecord, result zoneFileImportResult) zoneFileImportResult {
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
			result.Add = append(result.Add, rec)
			continue
		}
		old := pool[0]
		unmatched[key] = pool[1:]
		matched[old.ID] = true

		// The record survives, so it keeps its row id, and its comment: a
		// master file has no way to carry one, so treating the file's
		// silence as "no comment" would delete a note the file could never
		// have preserved in the first place. Enabled comes from the file
		// unconditionally — a record present in a master file is a record
		// that is served (zones.Render omits disabled ones for exactly that
		// reason), so importing a file that lists a currently-disabled
		// record enables it.
		rec.ID = old.ID
		rec.Comment = old.Comment
		if rec.TTL != old.TTL || rec.Enabled != old.Enabled {
			result.Change = append(result.Change, zoneRecordChange{From: old, To: rec})
		}
	}

	// Whatever no record in the file claimed. Tested by row id rather than
	// by re-counting identities, because with duplicate rows the two are
	// not the same question: recounting would name the first row carrying a
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
			result.Delete = append(result.Delete, rec)
		}
	}
	return result
}

// applyZoneFile commits a diff and the file's SOA. It runs deletes first,
// then changes, then adds, so a name that swaps which records live under
// it never has both sets present at once.
//
// It deliberately does NOT call syncPTR. Import writes exactly what the
// file contains and nothing else: a forward-zone import must not silently
// rewrite a reverse zone the user did not name, and a 200-record import
// stays one predictable set of writes instead of a cascade into other
// zones. This is the only exception to §7's "A/AAAA writes maintain the
// matching PTR" rule and spec §8 states it as one — PTRs arrive by
// importing the reverse zone's own file. Do not "fix" this by adding the
// call.
func (s *Server) applyZoneFile(r *http.Request, zone store.Zone, pz zones.ParsedZone, diff zoneFileImportResult) error {
	// The write half of an import must not be abandoned because the client
	// hung up mid-request — same reasoning as reloadZones (zones_handlers.go),
	// and more pressing here, since stopping part-way through leaves the zone
	// neither the file nor what it was.
	ctx := context.WithoutCancel(r.Context())
	zs := s.deps.Store.Zones()

	for _, rec := range diff.Delete {
		if err := zs.DeleteRecord(ctx, rec.ID); err != nil {
			return err
		}
	}
	for _, ch := range diff.Change {
		if err := zs.UpdateRecord(ctx, ch.To); err != nil {
			return err
		}
	}
	for _, rec := range diff.Add {
		if _, err := zs.AddRecord(ctx, rec); err != nil {
			return err
		}
	}

	// The file's SOA becomes the zone's (RFC 1034 §3.6.1) — its NS, mbox,
	// timers and its own TTL — with one exception. Taking the file's serial
	// verbatim can move the zone's serial backwards, and a secondary that
	// has already seen the higher value would then ignore the zone forever
	// after (§4 D), so the serial becomes max(file, current) + 1: derived
	// from the file when the file is ahead, but never lower than what has
	// already been served.
	//
	// That + 1 wraps at 2^32, which is correct rather than a bug to guard:
	// RFC 1982 serial arithmetic makes 0 the successor of 4294967295, so a
	// wrapped serial still compares as newer to every resolver. Clamping
	// instead would stall the zone at its maximum and stop propagating.
	zone.SOANS = strings.TrimSuffix(pz.SOA.Ns, ".")
	zone.SOAMbox = strings.TrimSuffix(pz.SOA.Mbox, ".")
	zone.SOARefresh = pz.SOA.Refresh
	zone.SOARetry = pz.SOA.Retry
	zone.SOAExpire = pz.SOA.Expire
	zone.SOAMinimum = pz.SOA.Minttl
	// Guaranteed non-zero by zones.Parse (soaTTLProblem): a zero here would
	// make every NXDOMAIN this zone hands out uncacheable — see
	// defaultSOATTL in zones_handlers.go.
	zone.SOATTL = pz.SOA.Hdr.Ttl
	zone.SOASerial = max(pz.SOA.Serial, zone.SOASerial) + 1
	zone.ModifiedAt = time.Now().UnixMilli()
	return zs.UpdateZone(ctx, zone)
}
