package filter

import (
	"bufio"
	"io"
	"strings"
)

type ParseResult struct {
	Block   []string
	Allow   []string
	Skipped int
}

func ParseList(r io.Reader) (ParseResult, error) {
	var res ParseResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case strings.HasPrefix(line, "@@||"):
			if d, ok := abpDomain(line[4:]); ok {
				res.Allow = append(res.Allow, d)
			} else {
				res.Skipped++
			}
		case strings.HasPrefix(line, "||"):
			if d, ok := abpDomain(line[2:]); ok {
				res.Block = append(res.Block, d)
			} else {
				res.Skipped++
			}
		default:
			fields := strings.Fields(line)
			switch {
			case len(fields) == 2 && (fields[0] == "0.0.0.0" || fields[0] == "127.0.0.1" || fields[0] == "::" || fields[0] == "::1"):
				if d, ok := wildcardDomain(fields[1]); ok {
					res.Block = append(res.Block, d)
				} else {
					res.Skipped++
				}
			case len(fields) == 1:
				if d, ok := wildcardDomain(fields[0]); ok {
					res.Block = append(res.Block, d)
				} else {
					res.Skipped++
				}
			default:
				res.Skipped++
			}
		}
	}
	return res, sc.Err()
}

// wildcardDomain accepts an optional leading `*.` label — the format of
// hagezi's mainstream wildcard/* lists and of dnsmasq-style configs — and
// returns the domain it covers. Without it, every line of a 4.5 MB wildcard
// blocklist is rejected by validDomain's charset and the list silently
// yields zero entries.
//
// Stripping the star is the correct mapping for this engine, not a
// convenient shortcut. DomainSet.Match (domainset.go) walks whole labels
// from the TLD inward and reports a hit on any stored ancestor, so a stored
// `analytics.example.com` already blocks `x.analytics.example.com` and
// everything below it — subdomain coverage is what the trie does by default,
// which is exactly what `*.` asks for.
//
// The one deliberate difference: this also blocks the apex
// (`analytics.example.com` itself), which strict AdGuard `*.x` syntax
// excludes. For a blocklist that is the safer reading — blocking every
// subdomain of a tracker while leaving the tracker's own name resolvable is
// not what anyone subscribing to these lists intends — and it matches
// dnsmasq's `address=/analytics.example.com/`, which these same files are
// routinely fed to.
//
// Only a *leading* `*.` is accepted. A star anywhere else (`ads.*.com`,
// `*ads.com`) is untouched here and still rejected by validDomain's charset,
// and abpDomain keeps its own guard against `/ ^ $ * |` inside ABP rules.
func wildcardDomain(s string) (string, bool) {
	return validDomain(strings.TrimPrefix(s, "*."))
}

func abpDomain(s string) (string, bool) {
	s = strings.TrimSuffix(s, "^")
	if strings.ContainsAny(s, "/^$*|") {
		return "", false // path/regex ABP rules unsupported
	}
	return validDomain(s)
}

func validDomain(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return "", false
	}
	for _, lbl := range strings.Split(s, ".") {
		if lbl == "" || len(lbl) > 63 {
			return "", false
		}
		for _, r := range lbl {
			if r != '-' && r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return "", false
			}
		}
	}
	return s, true
}
