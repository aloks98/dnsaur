package store

import (
	"net/url"
	"strings"
)

// maxDerivedNameLen keeps a derived name inside a table cell. Real list URLs
// are long (a 4.5 MB blocklist lives six path segments deep) and the name
// exists precisely so the UI stops rendering those.
const maxDerivedNameLen = 60

// DeriveListName invents a readable label for a filter list from its URL, for
// when the admin didn't supply one. A list must always have something human
// to show — the whole point of List.Name is that the raw URL stops being the
// identifier in menus, toasts and confirmation dialogs — so this never
// returns "".
//
// The rule, in order:
//
//  1. GitHub raw/blob URLs have a fixed, published shape:
//     /<owner>/<repo>/<ref>/<path…>. The owner and the path below the ref are
//     the two parts that actually distinguish one list from another — every
//     hagezi list shares a host, a repo and often a filename — so those are
//     what get used:
//     raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt
//     → "hagezi wildcard/pro.txt"
//     raw.githubusercontent.com/StevenBlack/hosts/master/hosts
//     → "StevenBlack hosts"
//     The branch is dropped deliberately: "main" tells an admin nothing.
//  2. Anything else: host (minus a leading www.) plus the final path segment.
//     example.com/hosts → "example.com hosts"
//  3. No path at all, or an unparseable URL: the host, or the raw string.
//
// It is deterministic and derived, never authoritative — the Add-list dialog
// shows it as a placeholder rather than a prefilled value, and the UI keeps
// the URL on screen underneath, so it is always checkable against the source.
func DeriveListName(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return truncateName(strings.TrimSpace(rawURL))
	}
	host := strings.TrimPrefix(u.Host, "www.")
	segs := make([]string, 0, 8)
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return truncateName(host)
	}

	// GitHub's raw and blob paths are /<owner>/<repo>/<ref>/<path…>; "blob"
	// URLs carry an extra literal segment before the ref.
	if host == "raw.githubusercontent.com" || host == "github.com" {
		// skip owner/repo/ref, and github.com's extra blob|raw segment.
		skip := 3
		if host == "github.com" && len(segs) > 2 && (segs[2] == "blob" || segs[2] == "raw") {
			skip = 4
		}
		// Bounds-checked before slicing: a truncated GitHub URL
		// (/owner, /owner/repo/ref) is a real thing a user can paste, and
		// it must fall through to the generic rule, not panic.
		if len(segs) > skip {
			return truncateName(segs[0] + " " + strings.Join(segs[skip:], "/"))
		}
	}
	return truncateName(host + " " + segs[len(segs)-1])
}

// truncateName keeps a derived name inside its cell without producing a
// misleading fragment: an ellipsis marks that something was cut.
func truncateName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "unnamed list"
	}
	if len([]rune(s)) <= maxDerivedNameLen {
		return s
	}
	return string([]rune(s)[:maxDerivedNameLen-1]) + "…"
}
