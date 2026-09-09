package zones_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

func TestParsePrimaries(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"192.168.150.5", []string{"192.168.150.5:53"}, false},
		{"192.168.150.5:5353", []string{"192.168.150.5:5353"}, false},
		{"192.168.150.5, 192.168.150.6:5353", []string{"192.168.150.5:53", "192.168.150.6:5353"}, false},
		// A secondary with no primary can never transfer, so an empty list is
		// a configuration error rather than a zone that quietly never updates.
		{"", nil, true},
		{"not a host", nil, true},
	} {
		got, err := zones.ParsePrimaries(t.Context(), nil, tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParsePrimaries(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
		}
		var gotStr []string
		for _, ap := range got {
			gotStr = append(gotStr, ap.String())
		}
		if !slices.Equal(gotStr, tc.want) {
			t.Fatalf("ParsePrimaries(%q) = %v, want %v", tc.in, gotStr, tc.want)
		}
	}
}

// An IPv6 primary is written bare when it carries no port and bracketed when
// it does — the same rule every other host:port field in DNS follows, and the
// reason net.SplitHostPort's "too many colons" has to be treated as the
// no-port form rather than as a parse failure.
func TestParsePrimariesAcceptsIPv6(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2001:db8::1", "[2001:db8::1]:53"},
		{"[2001:db8::1]:5353", "[2001:db8::1]:5353"},
	} {
		got, err := zones.ParsePrimaries(t.Context(), nil, tc.in)
		if err != nil {
			t.Fatalf("ParsePrimaries(%q): %v", tc.in, err)
		}
		if len(got) != 1 || got[0].String() != tc.want {
			t.Fatalf("ParsePrimaries(%q) = %v, want [%s]", tc.in, got, tc.want)
		}
	}
}

// The requirement the whole format exists for: a primary named by hostname is
// accepted and *stored* without being resolved, so it survives its address
// changing. ValidatePrimaries is what the API calls at write time, and it must
// not touch the network to say yes.
func TestValidatePrimariesAcceptsAHostnameWithoutResolvingIt(t *testing.T) {
	// .invalid is reserved by RFC 6761 and resolves nowhere, so a validator
	// that quietly resolved would have to reject this.
	for _, in := range []string{"ns1.nowhere.invalid", "ns1.nowhere.invalid:5353", "ns1.nowhere.invalid, 192.168.150.5"} {
		if err := zones.ValidatePrimaries(in); err != nil {
			t.Fatalf("ValidatePrimaries(%q) = %v, want nil", in, err)
		}
	}
	for _, in := range []string{"", "   ", ",,", "not a host", "192.168.150.5:0", "192.168.150.5:70000", "192.168.150.5:http"} {
		if err := zones.ValidatePrimaries(in); err == nil {
			t.Fatalf("ValidatePrimaries(%q) = nil, want an error", in)
		}
	}
}

// FormatPrimaries is the inverse: what a caller writes back after resolving,
// in the form ParsePrimaries reads.
func TestFormatPrimariesRoundTrips(t *testing.T) {
	// The separator is ", " — the one FormatForwardTo, FormatNotifyTo and
	// FormatACL all write, and one the reader trims back off.
	const in = "192.168.150.5:53, [2001:db8::1]:5353"
	aps, err := zones.ParsePrimaries(t.Context(), nil, in)
	if err != nil {
		t.Fatalf("ParsePrimaries(%q): %v", in, err)
	}
	if got := zones.FormatPrimaries(aps); got != in {
		t.Fatalf("FormatPrimaries(ParsePrimaries(%q)) = %q, want %q", in, got, in)
	}
	if got := zones.FormatPrimaries(nil); got != "" {
		t.Fatalf("FormatPrimaries(nil) = %q, want empty", got)
	}
}

// mockNameserver runs a real DNS server on 127.0.0.1:0 and returns its
// address — the same shape internal/upstream's tests use. It is what makes
// the resolution half of ParsePrimaries testable without depending on the
// machine's own resolver or on anything outside the process.
func mockNameserver(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, ln := listenBothProtocols(t)
	u := &dns.Server{PacketConn: pc, Handler: handler}
	s := &dns.Server{Listener: ln, Handler: handler}
	go func() { _ = u.ActivateAndServe() }()
	go func() { _ = s.ActivateAndServe() }()
	t.Cleanup(func() { _ = u.Shutdown(); _ = s.Shutdown() })
	return pc.LocalAddr().String()
}

// resolverAt is a net.Resolver that asks addr and nothing else: PreferGo
// takes Go's own resolver rather than cgo, and Dial ignores the system's
// nameserver list entirely.
func resolverAt(addr string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

// oneHost answers A and AAAA for name and NXDOMAIN for everything else.
func oneHost(name, v4, v6 string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		r.Authoritative = true
		q := m.Question[0]
		if !strings.EqualFold(q.Name, name) {
			r.Rcode = dns.RcodeNameError
			_ = w.WriteMsg(r)
			return
		}
		switch q.Qtype {
		case dns.TypeA:
			rr, _ := dns.NewRR(name + " 300 IN A " + v4)
			r.Answer = []dns.RR{rr}
		case dns.TypeAAAA:
			rr, _ := dns.NewRR(name + " 300 IN AAAA " + v6)
			r.Answer = []dns.RR{rr}
		}
		_ = w.WriteMsg(r)
	}
}

// The half of the format that only exists at transfer time: a hostname is
// resolved on every call, which is what lets a primary follow its address
// rather than the one it had when the zone was created. Every address the
// name has is returned — a list of primaries exists so one being down is
// survivable, and that does not stop applying inside a single name.
func TestParsePrimariesResolvesAHostname(t *testing.T) {
	res := resolverAt(mockNameserver(t, oneHost("primary.test.", "10.0.0.1", "2001:db8::1")))

	got, err := zones.ParsePrimaries(t.Context(), res, "primary.test.:5353")
	if err != nil {
		t.Fatalf("ParsePrimaries: %v", err)
	}
	var gotStr []string
	for _, ap := range got {
		gotStr = append(gotStr, ap.String())
	}
	sort.Strings(gotStr)
	// "10.0.0.1:5353", not "[::ffff:10.0.0.1]:5353": LookupNetIP hands back
	// a v4 address in its 4-in-6 form, and an unmapped one is what dials and
	// what prints in a log line an operator has to read.
	want := []string{"10.0.0.1:5353", "[2001:db8::1]:5353"}
	if !slices.Equal(gotStr, want) {
		t.Fatalf("ParsePrimaries = %v, want %v", gotStr, want)
	}

	// And the default port applies to a resolved name exactly as it does to
	// a literal.
	got, err = zones.ParsePrimaries(t.Context(), res, "primary.test.")
	if err != nil {
		t.Fatalf("ParsePrimaries without a port: %v", err)
	}
	for _, ap := range got {
		if ap.Port() != 53 {
			t.Fatalf("port = %d, want 53", ap.Port())
		}
	}
}

// A list with nothing resolvable in it is a transfer-time failure, and the
// error has to name every primary that failed — the whole point of the list is
// that some of them can.
func TestParsePrimariesReportsWhichHostFailedToResolve(t *testing.T) {
	res := resolverAt(mockNameserver(t, oneHost("primary.test.", "10.0.0.1", "2001:db8::1")))
	_, err := zones.ParsePrimaries(t.Context(), res, "gone.test., also-gone.test.")
	if err == nil {
		t.Fatal("ParsePrimaries = nil error for a list where no name resolves")
	}
	for _, want := range []string{"gone.test.", "also-gone.test."} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q; it must name the primary %q that failed", err, want)
		}
	}
}

// One entry that will not resolve must not take the rest of the list with it.
// A list is written with more than one entry precisely so one of them being
// unusable is survivable, and an all-or-nothing resolution turned a resolver
// outage into "the literal beside it is never dialled and every NOTIFY for the
// zone is REFUSED".
func TestParsePrimariesSkipsAnEntryThatWillNotResolve(t *testing.T) {
	res := resolverAt(mockNameserver(t, oneHost("primary.test.", "10.0.0.1", "2001:db8::1")))

	got, err := zones.ParsePrimaries(t.Context(), res, "gone.test., 10.0.0.5")
	if err != nil {
		t.Fatalf("ParsePrimaries: %v", err)
	}
	var gotStr []string
	for _, ap := range got {
		gotStr = append(gotStr, ap.String())
	}
	if want := []string{"10.0.0.5:53"}; !slices.Equal(gotStr, want) {
		t.Fatalf("ParsePrimaries = %v, want %v: the literal beside an unresolvable name must still be dialled", gotStr, want)
	}
}

// An IP literal must never reach the resolver: a transfer to an address must
// not depend on DNS working. The resolver here fails every dial, so the only
// way this passes is by not consulting it.
func TestParsePrimariesDoesNotResolveALiteral(t *testing.T) {
	dead := &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("the resolver must not be consulted for a literal")
		},
	}
	got, err := zones.ParsePrimaries(t.Context(), dead, "192.168.150.5:5353")
	if err != nil {
		t.Fatalf("ParsePrimaries: %v", err)
	}
	if len(got) != 1 || got[0].String() != "192.168.150.5:5353" {
		t.Fatalf("ParsePrimaries = %v, want [192.168.150.5:5353]", got)
	}
}

// Unmap is not decoration. LookupNetIP hands back whatever form the answer
// carried: an A comes back as a 4-byte address, but an AAAA holding
// ::ffff:10.0.0.2 comes back mapped, and netip keeps it that way. Dialling
// and logging both want the dotted quad — an operator reading
// "[::ffff:10.0.0.2]:53" in a transfer failure has to decode it before they
// can compare it to what they configured.
//
// Deliberately an AAAA: with only A records in the fixture this test passes
// with .Unmap() deleted, which is exactly the hole it was written to close.
func TestParsePrimariesUnmapsA4in6Address(t *testing.T) {
	res := resolverAt(mockNameserver(t, oneHost("mapped.test.", "10.0.0.1", "::ffff:10.0.0.2")))
	got, err := zones.ParsePrimaries(t.Context(), res, "mapped.test.")
	if err != nil {
		t.Fatalf("ParsePrimaries: %v", err)
	}
	var gotStr []string
	for _, ap := range got {
		gotStr = append(gotStr, ap.String())
	}
	sort.Strings(gotStr)
	want := []string{"10.0.0.1:53", "10.0.0.2:53"}
	if !slices.Equal(gotStr, want) {
		t.Fatalf("ParsePrimaries = %v, want %v — the AAAA's 4-in-6 form must be unmapped", gotStr, want)
	}
}
