package api

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

func (s *Server) tsigKeysRoutes() {
	s.route("GET /api/v1/tsig-keys", s.requireAuth(s.handleTSIGKeysList))
	s.route("POST /api/v1/tsig-keys", s.requireAuth(s.handleTSIGKeyCreate))
	s.route("GET /api/v1/tsig-keys/{id}", s.requireAuth(s.handleTSIGKeyGet))
	s.route("PUT /api/v1/tsig-keys/{id}", s.requireAuth(s.handleTSIGKeyUpdate))
	s.route("DELETE /api/v1/tsig-keys/{id}", s.requireAuth(s.handleTSIGKeyDelete))
}

// tsigAlgorithms is the set of algorithms miekg/dns can actually sign and
// verify with -- the cases tsigHMACProvider.Generate/Verify switch on
// (tsig.go:18). dns.HmacMD5 ("hmac-md5.sig-alg.reg.int.") is a real RFC 8945
// registry name and is still exported by the library for compatibility, but
// it was removed from that switch -- Generate falls through the default case
// and returns ErrKeyAlg for it. A key created with it would parse and store
// fine and then fail on the very first transfer, so it is rejected here
// instead, at write time.
var tsigAlgorithms = map[string]bool{
	dns.HmacSHA1:   true,
	dns.HmacSHA224: true,
	dns.HmacSHA256: true,
	dns.HmacSHA384: true,
	dns.HmacSHA512: true,
}

// normalizeTSIGName canonicalises a TSIG key's owner name (RFC 8945 §4.2) to
// the form store.TSIGKeyStore.ByName looks keys up by: lowercase and fully
// qualified, via dns.CanonicalName. dns.IsDomainName alone would accept it --
// it documents itself as "extremely liberal", the same gap normalizeZoneName
// (zones_handlers.go) works around -- so the same label rules apply here,
// through the same validator. A key name is written into allow_transfer and
// notify_to as `key:<name>` in a comma-separated list, so a name carrying
// punctuation would not survive being read back out of one either.
func normalizeTSIGName(raw string) (string, bool) {
	name := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	if !validDomainLabels(name, false) {
		return "", false
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return "", false
	}
	return dns.CanonicalName(name), true
}

// tsigKeyWrite is the request body shared by create (POST) and replace
// (PUT): a TSIG key has no field that is optional or server-generated apart
// from id and created_at, so there is no separate partial-patch shape.
type tsigKeyWrite struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	// Secret is base64, exactly as it would be pasted into a peer's config
	// (BIND's key{} clause, Technitium's zone transfer settings). Validated
	// here because that is the same encoding tsigHMACProvider.Generate
	// decodes it with at sign time -- an unusable secret must fail at write,
	// not on the first transfer.
	Secret string `json:"secret"`
}

// validateTSIGKeyWrite normalises and validates body, shared by create and
// update. On success it returns the canonical name ready to store.
func validateTSIGKeyWrite(body tsigKeyWrite) (name string, code int, msg string, ok bool) {
	name, ok = normalizeTSIGName(body.Name)
	if !ok {
		return "", http.StatusBadRequest, "name must be a valid domain name", false
	}
	if !tsigAlgorithms[body.Algorithm] {
		return "", http.StatusBadRequest,
			"algorithm must be one of hmac-sha1., hmac-sha224., hmac-sha256., hmac-sha384., hmac-sha512.", false
	}
	if _, err := base64.StdEncoding.DecodeString(body.Secret); err != nil {
		return "", http.StatusBadRequest, "secret must be base64-encoded", false
	}
	return name, 0, "", true
}

// tsigKeyView is a TSIG key as a read answers with it. It exists for the
// one field the answer depends on the caller for.
//
// Scope enforcement is method-based (see Server.requireAuth): a read token
// may call any GET, and a TSIG secret is the only credential this API
// returns in the clear on one. So a token minted so a dashboard or a
// monitoring script could look around also handed over every signing key
// the server holds, which is not what "read-only" means to the person who
// minted it.
//
// The secret is still returned to a session and to a write-scoped token,
// because it has to be: the same value must be configured on the peer
// (BIND's key{} clause, Technitium's transfer settings), so it has to be
// readable whenever that peer needs re-pairing. secret_redacted is always
// present, never omitted when false, so a client can tell "no secret was
// set" from "you may not see it" without guessing from an empty string.
type tsigKeyView struct {
	store.TSIGKey
	SecretRedacted bool `json:"secret_redacted"`
}

// tsigKeyFor renders one key for the caller behind r.
func tsigKeyFor(r *http.Request, k store.TSIGKey) tsigKeyView {
	if tokenFrom(r).Scope != "read" {
		return tsigKeyView{TSIGKey: k}
	}
	k.Secret = ""
	return tsigKeyView{TSIGKey: k, SecretRedacted: true}
}

func (s *Server) handleTSIGKeysList(w http.ResponseWriter, r *http.Request) {
	keys, err := s.deps.Store.TSIGKeys().List(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	out := make([]tsigKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, tsigKeyFor(r, k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTSIGKeyGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	k, found, err := s.deps.Store.TSIGKeys().Get(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	if !found {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, tsigKeyFor(r, k))
}

func (s *Server) handleTSIGKeyCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[tsigKeyWrite](w, r)
	if !ok {
		return
	}
	name, code, msg, ok := validateTSIGKeyWrite(body)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	key := store.TSIGKey{
		Name:      name,
		Algorithm: body.Algorithm,
		Secret:    body.Secret,
		CreatedAt: time.Now().UnixMilli(),
	}
	id, err := s.deps.Store.TSIGKeys().Create(r.Context(), key)
	if err != nil {
		storeErrDup(w, err, "a TSIG key with that name already exists")
		return
	}
	key.ID = id
	created(w, resourceURL("tsig-keys", id), tsigKeyFor(r, key))
}

func (s *Server) handleTSIGKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, ok := decodeOr400[tsigKeyWrite](w, r)
	if !ok {
		return
	}
	name, code, msg, ok := validateTSIGKeyWrite(body)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	// A rename is refused with 409 "resource in use" while a zone still
	// names this key in allow_transfer or notify_to — the same guard that
	// refuses the delete, since renaming a key referenced by name breaks the
	// reference exactly as removing it does. Enforced in the UPDATE itself;
	// see tsigKeyStore.Update.
	err := s.deps.Store.TSIGKeys().Update(r.Context(), store.TSIGKey{
		ID:        id,
		Name:      name,
		Algorithm: body.Algorithm,
		Secret:    body.Secret,
	})
	if err != nil {
		storeErrDup(w, err, "a TSIG key with that name already exists")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTSIGKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.TSIGKeys().Delete(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
