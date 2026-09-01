package zones_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// D2's client (Transferrer, transfer.go) and D3's server (TransferServer,
// transferserver.go) were each written against the RFCs, not against each
// other. This file is the one place both halves are ours at once: a dnsaur
// primary (xfrFixture, transferserver_test.go) serving to a dnsaur secondary
// (transferFixture, transfer_test.go). A disagreement between the two — a
// TSIG signature one generates and the other refuses, an rdata spelling that
// survives the wire differently than either side expects — has nowhere else
// to hide, because everywhere else in this milestone one end is the library.
//
// The primary and secondary zones deliberately share a name: transferApex
// ("xfer.e412.in", transfer_test.go), since newTransferFixture hardcodes it
// as the secondary's own. Reusing it here, rather than inventing a third
// apex, is what lets newXFRFixture (the primary) and newTransferFixture (the
// secondary) be pointed at each other with nothing else to reconcile.

// loopbackRecords is half a dozen record types whose presentation format is
// easy to get subtly wrong across a wire round trip: A, AAAA, MX, TXT,
// CNAME, SRV. Every rdata below is already in the canonical presentation
// form miekg's own String() produces, because the secondary stores whatever
// RDataOf derives from the RR it unpacked off the wire (Transferrer.build) —
// a value typed here in a non-canonical spelling would make the record-set
// comparison fail for a reason that has nothing to do with what this test is
// checking. The primary itself never runs that normalisation: newXFRFixture
// writes rows straight to the store, unlike the API's BuildRecord.
//
// disabled is the record TestAZoneServedOverAXFRExcludesDisabledRecords is
// about: the snapshot the primary serves from already dropped it (NewZone),
// so it is not present in xfrRecords()'s sense of "what a querier sees"
// either.
func loopbackRecords() []store.ZoneRecord {
	return []store.ZoneRecord{
		{Name: "@", Type: "NS", TTL: 300, RData: "ns1." + transferApex + ".", Enabled: true},
		{Name: "ns1", Type: "A", TTL: 300, RData: "10.20.0.1", Enabled: true},
		{Name: "bifrost", Type: "A", TTL: 300, RData: "10.20.0.2", Enabled: true},
		{Name: "bifrost", Type: "AAAA", TTL: 300, RData: "2001:db8::2", Enabled: true},
		{Name: "@", Type: "MX", TTL: 300, RData: "10 mail." + transferApex + ".", Enabled: true},
		{Name: "mail", Type: "A", TTL: 300, RData: "10.20.0.3", Enabled: true},
		{Name: "@", Type: "TXT", TTL: 300, RData: `"v=spf1 -all"`, Enabled: true},
		{Name: "www", Type: "CNAME", TTL: 300, RData: "bifrost." + transferApex + ".", Enabled: true},
		{Name: "_sip._tcp", Type: "SRV", TTL: 300, RData: "10 20 5060 bifrost." + transferApex + ".", Enabled: true},
		{Name: "disabled", Type: "A", TTL: 300, RData: "10.20.0.9", Enabled: false},
	}
}

// loopbackBigRecordCount is a zone that cannot be carried in one message,
// let alone one envelope: 2,000 A records under 20-character labels, roughly
// 44 uncompressed bytes each, so about 88 KiB against a 16 KiB envelope
// target and the 65535 a single DNS message can hold at all. Sized to match
// transferserver_test.go's bigZoneRecordCount, which is the multi-envelope
// shape the miekg-client tests use.
const loopbackBigRecordCount = 2000

// loopbackBigRecords is that zone, under transferApex so both fixtures agree
// on the name the way loopbackRecords does.
func loopbackBigRecords(n int) []store.ZoneRecord {
	recs := make([]store.ZoneRecord, 0, n+1)
	recs = append(recs, store.ZoneRecord{
		Name: "@", Type: "NS", TTL: 300, RData: "ns1." + transferApex + ".", Enabled: true,
	})
	for i := range n {
		recs = append(recs, store.ZoneRecord{
			Name: fmt.Sprintf("host%04d-of-big-zone", i), Type: "A", TTL: 300,
			RData: fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff), Enabled: true,
		})
	}
	return recs
}

// loopbackPrimaryZone is the primary side of the pair: same apex the
// secondary fixture hard-codes, an ACL, and a serial distinct from both 1
// (newTransferFixture's initial placeholder) and 3 (transferserver_test.go's
// xfrZone), so a passing assertion on SOA serial cannot be an accident of
// two fixtures agreeing by coincidence.
func loopbackPrimaryZone(acl string) store.Zone {
	return store.Zone{
		Name: transferApex, Type: "primary", Enabled: true, AllowTransfer: acl,
		SOANS: "ns1." + transferApex, SOAMbox: "hostmaster." + transferApex,
		SOASerial: 777, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	}
}

// recordFingerprint is what "the same record" means for the comparison
// below: owner name, type, TTL and rdata — exactly the four things the task
// asks the loopback test to prove survive the round trip. ID and ZoneID are
// deliberately excluded: they are storage identifiers with no counterpart on
// the wire.
type recordFingerprint struct {
	name, typ string
	ttl       uint32
	rdata     string
}

// recordFingerprints counts each enabled record's fingerprint, so a record
// duplicated or dropped in transit is caught as well as one whose rdata
// merely changed shape.
func recordFingerprints(recs []store.ZoneRecord) map[recordFingerprint]int {
	out := make(map[recordFingerprint]int, len(recs))
	for _, r := range recs {
		if !r.Enabled {
			continue
		}
		out[recordFingerprint{name: strings.ToLower(r.Name), typ: strings.ToUpper(r.Type), ttl: r.TTL, rdata: r.RData}]++
	}
	return out
}

// assertRecordSetMatchesLoopback fails unless got is exactly
// loopbackRecords()'s enabled records, name+type+ttl+rdata, no more and no
// fewer.
func assertRecordSetMatchesLoopback(t *testing.T, got []store.ZoneRecord) {
	t.Helper()
	gotSet := recordFingerprints(got)
	wantSet := recordFingerprints(loopbackRecords())

	for k, n := range wantSet {
		if gotSet[k] != n {
			t.Errorf("record %+v: got %d, want %d", k, gotSet[k], n)
		}
	}
	for k, n := range gotSet {
		if wantSet[k] != n {
			t.Errorf("record %+v arrived on the secondary but is not in the primary's zone: %d", k, n)
		}
	}
}

// loopbackKey writes one TSIG key — same name, algorithm and secret — into
// both stores, the way an operator pairs a primary and a secondary in real
// configuration. Returns the id each store gave it, since the two are
// independent sqlite databases and there is no reason their autoincrement
// counters agree.
func loopbackKey(t *testing.T, primary, secondary store.Store, name string) (primaryID, secondaryID int64) {
	t.Helper()
	ctx := context.Background()
	secret := base64.StdEncoding.EncodeToString([]byte("a-loopback-transfer-secret-of-length"))
	k := store.TSIGKey{
		Name: dns.CanonicalName(name), Algorithm: dns.HmacSHA256, Secret: secret,
		CreatedAt: time.Now().UnixMilli(),
	}
	pid, err := primary.TSIGKeys().Create(ctx, k)
	if err != nil {
		t.Fatalf("primary TSIGKeys().Create: %v", err)
	}
	sid, err := secondary.TSIGKeys().Create(ctx, k)
	if err != nil {
		t.Fatalf("secondary TSIGKeys().Create: %v", err)
	}
	return pid, sid
}

func TestADnsaurSecondaryTransfersFromADnsaurPrimary(t *testing.T) {
	primary := newXFRFixture(t, loopbackPrimaryZone("127.0.0.0/8"), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)

	res, err := sec.transferrer().Transfer(context.Background(), sec.zone(t))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Serial != 777 {
		t.Errorf("serial = %d, want the primary's 777", res.Serial)
	}

	z := sec.zone(t)
	if z.SOASerial != 777 {
		t.Errorf("zone row's soa_serial = %d, want the primary's 777", z.SOASerial)
	}
	if z.RefreshedAt == 0 {
		t.Error("refreshed_at was not stamped")
	}
	if z.ExpiresAt == 0 {
		t.Error("expires_at was not stamped")
	}

	got, err := sec.st.Zones().Records(context.Background(), sec.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	assertRecordSetMatchesLoopback(t, got)

	// A zone that installed correctly but is not being served is a zone that
	// failed: the secondary's own resolver has to actually answer, not just
	// hold the right rows.
	m := sec.ask(t, "bifrost."+transferApex, dns.TypeA)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) == 0 {
		t.Fatalf("secondary did not answer bifrost.%s after transfer: rcode=%s answers=%v",
			transferApex, dns.RcodeToString[m.Rcode], m.Answer)
	}
	a, ok := m.Answer[0].(*dns.A)
	if !ok || a.A.String() != "10.20.0.2" {
		t.Fatalf("answer = %v, want the A record transferred from the primary", m.Answer)
	}
}

func TestTheLoopbackTransferIsSignedEndToEnd(t *testing.T) {
	keyName := "ns2." + transferApex
	// allow_transfer names only the key: an unsigned transfer has nothing to
	// match and would be REFUSED (TestAXFRWithAKeyEntryRequiresASignature
	// covers that refusal on its own), so a transfer that completes here can
	// only have done so signed.
	primary := newXFRFixture(t, loopbackPrimaryZone("key:"+dns.CanonicalName(keyName)), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)

	_, secKeyID := loopbackKey(t, primary.st, sec.st, keyName)
	z := sec.zone(t)
	z.TSIGKeyID = secKeyID
	if err := sec.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	res, err := sec.transferrer().Transfer(context.Background(), sec.zone(t))
	if err != nil {
		t.Fatalf("signed loopback transfer: %v", err)
	}
	if res.Serial != 777 {
		t.Errorf("serial = %d, want the primary's 777", res.Serial)
	}

	got, err := sec.st.Zones().Records(context.Background(), sec.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	assertRecordSetMatchesLoopback(t, got)

	m := sec.ask(t, "bifrost."+transferApex, dns.TypeA)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) == 0 {
		t.Fatalf("secondary did not answer bifrost.%s after a signed transfer: rcode=%s",
			transferApex, dns.RcodeToString[m.Rcode])
	}
}

func TestASignedLoopbackTransferCrossesEnvelopeBoundaries(t *testing.T) {
	// The loopback tests above transfer ten records, which is one envelope:
	// both halves are ours, but the seam D3 actually invented — a stream of
	// messages whose TSIG MAC chains from one to the next, timers-only after
	// the first (RFC 8945 §5.3.1) — never comes up. On the wire that seam is
	// otherwise verified only against miekg's own client, and the chaining is
	// the part where two implementations most easily disagree while each
	// looks right on its own.
	//
	// So: a zone too big for one message, signed. There is no envelope count
	// to assert on — Transferrer reports records, not messages — and none is
	// needed. A stream that tried one message could not be packed at all, and
	// a MAC that broke at the second would fail verification inside the
	// client, which is the D2 half of the pair doing the asserting.
	keyName := "ns2." + transferApex
	primary := newXFRFixture(t,
		loopbackPrimaryZone("key:"+dns.CanonicalName(keyName)),
		loopbackBigRecords(loopbackBigRecordCount))
	sec := newTransferFixture(t, primary.addr, 0)

	_, secKeyID := loopbackKey(t, primary.st, sec.st, keyName)
	z := sec.zone(t)
	z.TSIGKeyID = secKeyID
	if err := sec.st.Zones().UpdateZone(context.Background(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	res, err := sec.transferrer().Transfer(context.Background(), sec.zone(t))
	if err != nil {
		t.Fatalf("signed multi-envelope loopback transfer: %v", err)
	}
	// The NS plus every A record, and not one fewer: an envelope silently
	// dropped between the two halves would land here.
	if want := loopbackBigRecordCount + 1; res.Records != want {
		t.Errorf("installed %d records, want %d", res.Records, want)
	}

	got, err := sec.st.Zones().Records(context.Background(), sec.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if want := loopbackBigRecordCount + 1; len(got) != want {
		t.Fatalf("the secondary holds %d records, want %d", len(got), want)
	}

	// One record from each end of the stream, so a transfer truncated at an
	// envelope boundary cannot pass by holding the first envelope alone.
	for _, name := range []string{"host0000-of-big-zone", fmt.Sprintf("host%04d-of-big-zone", loopbackBigRecordCount-1)} {
		m := sec.ask(t, name+"."+transferApex, dns.TypeA)
		if m.Rcode != dns.RcodeSuccess || len(m.Answer) == 0 {
			t.Fatalf("secondary did not answer %s after the transfer: rcode=%s", name, dns.RcodeToString[m.Rcode])
		}
	}
}

func TestAZoneServedOverAXFRExcludesDisabledRecords(t *testing.T) {
	primary := newXFRFixture(t, loopbackPrimaryZone("127.0.0.0/8"), loopbackRecords())
	sec := newTransferFixture(t, primary.addr, 0)

	if _, err := sec.transferrer().Transfer(context.Background(), sec.zone(t)); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	got, err := sec.st.Zones().Records(context.Background(), sec.zoneID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	for _, r := range got {
		if strings.EqualFold(r.Name, "disabled") {
			t.Fatalf("a disabled record from the primary reached the secondary: %+v", r)
		}
	}

	// The same thing a querier sees: the name does not exist at all, not
	// merely "no A record here".
	m := sec.ask(t, "disabled."+transferApex, dns.TypeA)
	if m.Rcode != dns.RcodeNameError {
		t.Errorf("rcode = %s, want NXDOMAIN: a disabled record must not exist on the secondary either", dns.RcodeToString[m.Rcode])
	}
}
