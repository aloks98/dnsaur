package store

import (
	"context"
	"strings"
	"testing"
)

// TestDeriveListName pins the naming rule, including the two real URLs the
// feature exists for. A derived name has to be deterministic and short enough
// for a table cell — a dropdown of 90-character GitHub URLs is what it
// replaces — and it must distinguish lists that share a host, a repo and even
// a filename, which is why the GitHub owner and the path below the ref are
// what get used rather than host + last segment.
func TestDeriveListName(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		// The two the owner actually subscribes to.
		{"https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt", "hagezi wildcard/pro.txt"},
		{"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts", "StevenBlack hosts"},
		// Same repo, same filename, different list — the case host + last
		// segment would collapse into one indistinguishable label.
		{"https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt", "hagezi hosts/pro.txt"},
		{"https://github.com/hagezi/dns-blocklists/blob/main/wildcard/pro.txt", "hagezi wildcard/pro.txt"},
		// Generic hosts: host plus the final segment.
		{"https://example.com/hosts", "example.com hosts"},
		{"https://www.example.com/lists/ads.txt", "example.com ads.txt"},
		{"https://blocklist.example.org/", "blocklist.example.org"},
		{"https://blocklist.example.org", "blocklist.example.org"},
		// A GitHub URL too short for the owner/repo/ref shape must not
		// panic its way through a slice bound.
		{"https://raw.githubusercontent.com/hagezi", "raw.githubusercontent.com hagezi"},
		{"https://raw.githubusercontent.com/hagezi/dns-blocklists/main", "raw.githubusercontent.com main"},
		// Never empty, whatever it is handed.
		{"", "unnamed list"},
		{"::not a url::", "::not a url::"},
	} {
		if got := DeriveListName(tc.url); got != tc.want {
			t.Errorf("DeriveListName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestDeriveListNameIsBounded(t *testing.T) {
	got := DeriveListName("https://example.com/" + strings.Repeat("verylongsegment", 20))
	if len([]rune(got)) > maxDerivedNameLen {
		t.Fatalf("derived name is %d runes: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated name must show it was cut: %q", got)
	}
}

// TestListNameStoredAndRenamed: a list must never reach the UI without a
// label, whether the admin supplied one, left it blank, or subscribed before
// the column existed. Blank on rename means "back to the default", not "no
// name" — an empty string would leave menus and toasts with nothing to print.
func TestListNameStoredAndRenamed(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		url := testGroupName("https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts")
		explicit, err := s.Filters().AddList(ctx, List{URL: url + "/a", Name: "  Household baseline  ", Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		derived, err := s.Filters().AddList(ctx, List{URL: "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts" + testGroupName(""), Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		byID := func() map[int64]List {
			t.Helper()
			ls, err := s.Filters().Lists(ctx)
			if err != nil {
				t.Fatal(err)
			}
			m := map[int64]List{}
			for _, l := range ls {
				m[l.ID] = l
			}
			return m
		}

		got := byID()
		// AddList trims but otherwise keeps what the admin typed.
		if got[explicit].Name != "Household baseline" {
			t.Fatalf("explicit name = %q", got[explicit].Name)
		}
		if !strings.HasPrefix(got[derived].Name, "StevenBlack hosts") {
			t.Fatalf("derived name = %q, want the StevenBlack derivation", got[derived].Name)
		}

		if err := s.Filters().RenameList(ctx, explicit, "Kids blocklist"); err != nil {
			t.Fatal(err)
		}
		if n := byID()[explicit].Name; n != "Kids blocklist" {
			t.Fatalf("after rename = %q", n)
		}
		// Blank resets to the derived default rather than blanking the label.
		if err := s.Filters().RenameList(ctx, explicit, "   "); err != nil {
			t.Fatal(err)
		}
		if n := byID()[explicit].Name; n == "" || n != DeriveListName(url+"/a") {
			t.Fatalf("blank rename = %q, want the derived default", n)
		}
		if err := s.Filters().RenameList(ctx, 999999, "nope"); err == nil {
			t.Fatal("renaming a missing list must not silently succeed")
		}
	})
}
