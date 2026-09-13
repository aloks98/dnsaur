package api_test

import (
	"testing"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/confsync"
)

// The peer URL has two graders and one grammar. PUT /settings judges
// sync.peer_url with api's own validator; the Follow action judges the URL
// the operator typed with confsync.ParsePeerURL, and then writes it to that
// same setting through the store. A value one accepts and the other refuses
// is a box that cannot save what it just followed, or one that followed
// something the settings screen will not show.
//
// They are two copies because they have to be: confsync imports internal/api,
// so api cannot call into confsync. This test is the substitute for sharing
// the code — an external test package can name both — and it is why the
// duplication is safe rather than merely small.
//
// A lone trailing slash is the one input where the two differ in shape:
// ParsePeerURL takes it off, and handleSettingsPut strips it before the
// validator ever sees the value. Both accept it, which is what this asserts.
func TestPeerURLGrammarsAgree(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"", true}, // empty is a main, on both sides
		{"https://main.lan", true},
		{"http://main.lan", true},
		{"https://main.lan:8443", true},
		{"https://10.0.0.5:8443", true},
		{"https://[2001:db8::1]:8443", true},
		{"https://main.lan/", true},
		{"main.lan", false},
		{"//main.lan", false},
		{"ftp://main.lan", false},
		{"https://", false},
		{"https://main.lan/dnsaur", false},
		{"https://main.lan?x=1", false},
		{"https://main.lan#frag", false},
		{"https://admin:pw@main.lan", false},
	} {
		settings := api.PeerURLValidator(tc.in) == nil
		// ParsePeerURL has no "empty is fine" case: it is handed a URL the
		// operator typed into the Follow form, where blank is not a value.
		follow := tc.in == ""
		if !follow {
			_, err := confsync.ParsePeerURL(tc.in)
			follow = err == nil
		}
		if settings != follow {
			t.Errorf("%q: PUT /settings accepts=%v, Follow accepts=%v — one grammar, two graders",
				tc.in, settings, follow)
		}
		if settings != tc.ok {
			t.Errorf("%q: accepted=%v, want %v", tc.in, settings, tc.ok)
		}
	}
}
