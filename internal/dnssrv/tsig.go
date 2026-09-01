package dnssrv

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// TSIGKeys is the one lookup a signed message needs: the key whose owner name
// the message arrived under. store.TSIGKeyStore satisfies it; the interface is
// narrowed to a single method so the provider cannot reach the write side.
type TSIGKeys interface {
	ByName(ctx context.Context, name string) (store.TSIGKey, bool, error)
}

// tsigLookupTimeout bounds the store read a signed message triggers. Generate
// and Verify are called from the connection goroutine and carry no context of
// their own, so a store that has stopped answering would otherwise pin that
// goroutine for as long as it stays stuck.
const tsigLookupTimeout = 2 * time.Second

var (
	// ErrTSIGUnsigned means the message carried no TSIG at all. It is a
	// distinct outcome from a bad signature because miekg reports both as a
	// nil TsigStatus -- see RequireTSIG.
	ErrTSIGUnsigned = errors.New("dnssrv: message carries no TSIG")

	// ErrTSIGUnavailable means nothing verified this message: either the
	// server was built without a key store, or the Request was not produced by
	// Server.serve. Reported as a failure rather than a pass, because "no one
	// checked" and "checked and fine" must never look alike to a caller.
	ErrTSIGUnavailable = errors.New("dnssrv: no TSIG provider attached, nothing was verified")
)

// tsigProvider implements dns.TsigProvider against the key store.
//
// It is deliberately uncached, and the alternative was considered rather than
// skipped. A cache would save one indexed read on a UNIQUE column per signed
// message -- less work than the cache lookup and upstream forward an ordinary
// unsigned query already costs -- in exchange for a window in which a key the
// operator has just deleted still authenticates. A stale credential is the bad
// direction to be wrong in, and reading through is also the whole reason this
// is a provider and not dns.Server.TsigSecret: a key created through the API
// works on the next packet, with no restart and no invalidation protocol to
// get right. If a signed-query workload ever makes this measurable, a cache
// can go behind this same interface then, on evidence.
type tsigProvider struct{ keys TSIGKeys }

// NewTSIGProvider returns the store-backed dns.TsigProvider this package
// verifies incoming messages with, for a caller that has to sign outgoing
// ones. It is the same object with the same lookup: Generate is what signs a
// request, and RFC 8945 gives it no direction — dns.Server and dns.Transfer
// take the same interface.
//
// It is exported for internal/zones, whose AXFR client (Milestone D2) signs
// a transfer request under the key its zone names. Signing there with its own
// HMAC code would be a second implementation of the one thing in this
// codebase that must not have two: an outgoing signature this server would
// not itself accept is a bug nothing in either half's tests would catch.
//
// A nil keys returns nil, so a caller with no key store gets "no provider"
// rather than one that panics on first use — the same treatment WithTSIGKeys
// gives it.
func NewTSIGProvider(keys TSIGKeys) dns.TsigProvider {
	if keys == nil {
		return nil
	}
	return &tsigProvider{keys: keys}
}

// key resolves the TSIG RR's owner name to a stored key. An unknown name gives
// dns.ErrSecret, matching what miekg's own tsigSecretProvider returns for a
// name it does not hold (tsig.go:82). A store that failed gives that failure
// wrapped: it must never collapse into "no such key", and it must never
// collapse into success.
func (p *tsigProvider) key(name string) (store.TSIGKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tsigLookupTimeout)
	defer cancel()
	// Names are stored canonical (lowercase, trailing dot); what arrives on
	// the wire need not be, and RFC 8945 §4.2 makes key names case-insensitive.
	k, ok, err := p.keys.ByName(ctx, dns.CanonicalName(name))
	if err != nil {
		return store.TSIGKey{}, fmt.Errorf("tsig: looking up key %q: %w", name, err)
	}
	if !ok {
		return store.TSIGKey{}, dns.ErrSecret
	}
	return k, nil
}

// tsigHMAC maps a canonical algorithm name onto its keyed hash. This mirrors
// the switch in miekg's unexported tsigHMACProvider.Generate (tsig.go:42-57)
// case for case, including which algorithms are absent: dns.HmacMD5 is still
// exported by the library for compatibility but was removed from that switch,
// so it falls through to ErrKeyAlg here exactly as it does there.
func tsigHMAC(alg string, secret []byte) (hash.Hash, error) {
	switch alg {
	case dns.HmacSHA1:
		return hmac.New(sha1.New, secret), nil
	case dns.HmacSHA224:
		return hmac.New(sha256.New224, secret), nil
	case dns.HmacSHA256:
		return hmac.New(sha256.New, secret), nil
	case dns.HmacSHA384:
		return hmac.New(sha512.New384, secret), nil
	case dns.HmacSHA512:
		return hmac.New(sha512.New, secret), nil
	}
	return nil, dns.ErrKeyAlg
}

// Generate computes the MAC over msg for the key named by t, mirroring
// tsigHMACProvider.Generate (tsig.go:37-60): base64-decode the secret, pick
// the hash from the algorithm in the TSIG RR, HMAC the caller's byte slice
// whole, and return the full untruncated sum. msg is the buffer miekg has
// already assembled per RFC 8945 §4.3.3 (request MAC, message, TSIG
// variables); nothing here reinterprets it.
func (p *tsigProvider) Generate(msg []byte, t *dns.TSIG) ([]byte, error) {
	k, err := p.key(t.Hdr.Name)
	if err != nil {
		return nil, err
	}
	alg := dns.CanonicalName(t.Algorithm)
	// A key is the triple (name, algorithm, secret), so the algorithm the
	// message names has to be the one the key was created with. miekg's
	// providers cannot check this -- a map of name to secret has no algorithm
	// to check against -- but this one has the stored key in hand, and a
	// sender that disagrees with the operator's configuration is a mismatch to
	// reject, not to sign around. It is not a weakening of the mirrored
	// computation: the MAC below is still the algorithm the message asked for.
	if alg != dns.CanonicalName(k.Algorithm) {
		return nil, dns.ErrKeyAlg
	}
	rawsecret, err := base64.StdEncoding.DecodeString(k.Secret)
	if err != nil {
		return nil, err
	}
	h, err := tsigHMAC(alg, rawsecret)
	if err != nil {
		return nil, err
	}
	_, _ = h.Write(msg) // hash.Hash.Write never errors
	return h.Sum(nil), nil
}

// Verify checks the MAC on t against one computed over msg, mirroring
// tsigHMACProvider.Verify (tsig.go:62-76).
//
// The time window is not checked here and must not be: miekg checks the fudge
// itself, after this returns and deliberately not before (tsig.go:243-252),
// because doing it first is what CVE-2017-3142/3143 was.
func (p *tsigProvider) Verify(msg []byte, t *dns.TSIG) error {
	b, err := p.Generate(msg, t)
	if err != nil {
		return err
	}
	mac, err := hex.DecodeString(t.MAC)
	if err != nil {
		return err
	}
	// hmac.Equal, never == and never bytes.Equal. Both operands are derived
	// from a message an unauthenticated peer chose, and a comparison that
	// returns early on the first differing byte leaks, through timing, how
	// much of a guessed MAC was right -- which is a forgery oracle, one byte
	// at a time. hmac.Equal is subtle.ConstantTimeCompare underneath and also
	// returns false for a length mismatch.
	if !hmac.Equal(b, mac) {
		return dns.ErrSig
	}
	return nil
}

// RequireTSIG reports the key name a message was signed with, or an error
// saying why the message cannot be treated as authenticated. Callers that need
// authentication -- zone transfers, and anything else RFC 8945 gates -- must
// call this and act on the error. Skipping it does not mean "no TSIG
// enforcement"; it means the path is open to anyone who can reach the port,
// because verification here is automatic but non-enforcing: miekg records an
// outcome before the handler runs (server.go:673) and then serves the message
// either way.
//
// Checking w.TsigStatus() alone is not enough, and the gap is the whole reason
// this helper exists. miekg sets that status to nil unconditionally and only
// replaces it when the message actually carried a TSIG and a provider was
// attached, so nil means "not rejected" rather than "authenticated" -- an
// unsigned message, and a signed message arriving at a server with no
// provider, both read as success. Hence the two checks before the status.
func (s *Server) RequireTSIG(w dns.ResponseWriter, m *dns.Msg) (string, error) {
	if s.tsig == nil {
		return "", ErrTSIGUnavailable
	}
	t := m.IsTsig()
	if t == nil {
		return "", ErrTSIGUnsigned
	}
	if err := w.TsigStatus(); err != nil {
		return "", err
	}
	return dns.CanonicalName(t.Hdr.Name), nil
}

// tsigState is the answer Server.serve got from RequireTSIG for a request,
// carried on the Request so pipeline handlers can ask the same question
// without a dns.ResponseWriter. A nil *tsigState means nobody asked.
type tsigState struct {
	key string
	err error
}

// RequireTSIG is the pipeline-side form of (*Server).RequireTSIG and returns
// exactly what the server computed for this message. Everything said there
// about acting on the error applies here unchanged.
//
// A Request that did not come from Server.serve -- a hand-built one in a test,
// say -- reports ErrTSIGUnavailable rather than an empty key and no error, so
// the zero value fails closed.
func (r *Request) RequireTSIG() (string, error) {
	if r.tsig == nil {
		return "", ErrTSIGUnavailable
	}
	return r.tsig.key, r.tsig.err
}

// ReplyTSIG builds the stub TSIG that makes miekg sign a reply. It is a stub
// on purpose: WriteMsg passes it to TsigGenerateWithProvider, which fills in
// the MAC (server.go:753-761). RFC 8945 §5.3 answers a signed request under
// the same key and algorithm, and the fudge is echoed so a peer configured
// with a wide window is not narrowed to ours.
//
// The caller appends the returned record to the reply's Extra section. It is
// returned rather than attached because a UDP reply has to be trimmed to size
// *before* the signature goes on — see FitUDPReply.
//
// It is exported for the zone-transfer handler, which signs a reply this
// package never sees: every message of a transfer stream carries one of these
// (RFC 8945 §5.3.1). That handler used to build its own, which was the same
// fudge default and the same canonicalisation written twice with nothing
// keeping the two in step.
//
// TimeSigned is time.Now and deliberately not any injected clock: the peer
// checks it against its own inside the fudge window, so a test clock here
// would produce signatures a real peer rejects.
func ReplyTSIG(reply *dns.Msg, keyName string, req *dns.TSIG) *dns.TSIG {
	fudge := req.Fudge
	if fudge == 0 {
		fudge = 300 // RFC 8945 §5.2.3's usual default, and miekg's.
	}
	return &dns.TSIG{
		Hdr:        dns.RR_Header{Name: keyName, Rrtype: dns.TypeTSIG, Class: dns.ClassANY, Ttl: 0},
		Algorithm:  dns.CanonicalName(req.Algorithm),
		TimeSigned: uint64(time.Now().Unix()),
		Fudge:      fudge,
		OrigId:     reply.Id,
	}
}

// maxTSIGMACLen is the largest MAC any algorithm above produces (SHA-512, 64
// bytes). ReplyTSIG's stub carries no MAC yet, so reserving room for a signed
// reply has to allow for one.
const maxTSIGMACLen = 64
