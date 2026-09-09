package filter

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"golang.org/x/net/idna"
)

type ParseResult struct {
	Block   []string
	Allow   []string
	Skipped int
}

// maxLineBytes bounds one line of a list. Nothing legitimate comes close;
// the limit exists so a file with no newlines in it cannot be read into
// memory whole.
const maxLineBytes = 1 << 20

func ParseList(r io.Reader) (ParseResult, error) {
	var res ParseResult
	br := bufio.NewReaderSize(r, maxLineBytes)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// One absurd line must not cost the other million entries, which
			// is what aborting the read here used to do. Drop it and
			// resynchronise on the next newline.
			res.Skipped++
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			line = nil
		}
		if len(line) > 0 {
			res.parseLine(string(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			return res, err
		}
	}
}

// bom is the UTF-8 byte order mark. It is not whitespace, so left in place
// it swallows the first line of every list exported from an editor that
// writes one.
const bom = "\uFEFF"

func (res *ParseResult) parseLine(raw string) {
	line := strings.TrimSpace(strings.TrimPrefix(raw, bom))
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
		return
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

// RulePattern normalises a manual (non-regex) rule pattern into the form the
// trie stores and a query is matched against, or reports that it is not one.
// It is validDomain's rules minus the "must contain a dot" one, so
// `localhost` is a usable rule, plus wildcardDomain's leading `*.`: stored
// verbatim, `*.doubleclick.net` becomes a label `*` that no query can ever
// carry, which is a rule that silently does nothing.
func RulePattern(s string) (string, bool) {
	return validName(strings.TrimPrefix(strings.TrimSpace(s), "*."))
}

func validDomain(s string) (string, bool) {
	s, ok := validName(s)
	if !ok || !strings.Contains(s, ".") {
		return "", false
	}
	return s, true
}

// validName holds the label rules shared by list entries and manual rules.
//
// Unicode is converted to punycode rather than rejected: a query arrives on
// the wire already encoded, so a list naming `пример.рф` in Unicode was
// being skipped by the ASCII charset below while the name it meant was never
// blocked. idna.ToASCII is the encoder only — it leaves an ASCII name
// (underscores included, which real hosts files use) exactly as it is, so
// the charset check still has the final say.
func validName(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if a, err := idna.ToASCII(s); err == nil {
		s = a
	}
	if s == "" || len(s) > 253 {
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
