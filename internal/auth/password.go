package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
	// argonSlots is how many argon2id computations may run at once across
	// the whole process. Each one allocates argonMemory (64 MiB) for the
	// duration of the pass, and the two endpoints that trigger one —
	// POST /setup and POST /auth/login — are unauthenticated, so without a
	// ceiling a burst of N concurrent requests allocates N × 64 MiB before
	// any of them finish. Four is deliberately small: the per-IP limiter in
	// front of those endpoints keeps the queue behind this gate short, and
	// argon2id is meant to be slow, so queueing is the correct response to
	// a flood rather than a symptom.
	argonSlots = 4
)

// gate bounds how many goroutines run f at the same time. Its own type
// rather than a bare channel so the bound has a name and a test, and so
// every argon2 call site acquires and releases it the same way.
type gate chan struct{}

func (g gate) do(f func()) {
	g <- struct{}{}
	defer func() { <-g }()
	f()
}

// argon2Gate is the process-wide bound described on argonSlots. It fronts
// every argon2id computation in this package; nothing else calls
// argon2.IDKey.
var argon2Gate = make(gate, argonSlots)

func argon2Key(pw string, salt []byte, t, m uint32, p uint8, keyLen uint32) []byte {
	var key []byte
	argon2Gate.do(func() {
		key = argon2.IDKey([]byte(pw), salt, t, m, p, keyLen)
	})
	return key
}

func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2Key(pw, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("malformed hash")
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2Key(pw, salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
