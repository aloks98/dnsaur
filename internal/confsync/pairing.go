package confsync

import (
	"crypto/rand"
	"strings"
	"time"
)

const (
	// pairingAlphabet is 32 glyphs with no ambiguous pair in it: no I or 1,
	// no O or 0, no U next to V by accident. The operator reads a code off
	// one screen and types it into another, and a code they mistype is five
	// attempts from voiding itself (§9).
	pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// pairingCodeLen is eight glyphs of that alphabet: 40 bits, which is
	// only safe because of pairingTTL and pairingMaxAttempts.
	pairingCodeLen = 8
	// pairingTTL is how long a minted code lives (§6).
	pairingTTL = 10 * time.Minute
	// pairingMaxAttempts is how many wrong codes void the live one (§9).
	pairingMaxAttempts = 5
)

// pairingState is the live code, as `sync.pairing` holds it: never the code
// itself, only what is needed to recognise it, time it out and count the
// guesses against it.
type pairingState struct {
	Hash      string `json:"hash"`
	ExpiresAt int64  `json:"expires_at"`
	Attempts  int    `json:"attempts"`
}

// newPairingCode mints a code in the shape the main shows it, "XXXX-XXXX".
// The dash is grouping only; normalizePairingCode takes it back off.
func newPairingCode() (string, error) {
	raw := make([]byte, pairingCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i == pairingCodeLen/2 {
			b.WriteByte('-')
		}
		// 256 is a whole number of alphabets, so the remainder favours no
		// glyph over another.
		b.WriteByte(pairingAlphabet[int(v)%len(pairingAlphabet)])
	}
	return b.String(), nil
}

// normalizePairingCode is what both halves of the comparison run through, so
// the case and the grouping dash are the operator's to get wrong.
func normalizePairingCode(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", ""))
}
