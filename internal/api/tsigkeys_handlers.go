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
// (zones_handlers.go) works around -- so the same extra checks apply here:
// no whitespace or path characters, and no empty label (e.g. "e412..in").
func normalizeTSIGName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || strings.ContainsAny(name, " \t\r\n/\\") {
		return "", false
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			return "", false
		}
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

func (s *Server) handleTSIGKeysList(w http.ResponseWriter, r *http.Request) {
	keys, err := s.deps.Store.TSIGKeys().List(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
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
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleTSIGKeyCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[tsigKeyWrite](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	name, code, msg, ok := validateTSIGKeyWrite(body)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	id, err := s.deps.Store.TSIGKeys().Create(r.Context(), store.TSIGKey{
		Name:      name,
		Algorithm: body.Algorithm,
		Secret:    body.Secret,
		CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		storeErrDup(w, err, "a TSIG key with that name already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleTSIGKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[tsigKeyWrite](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	name, code, msg, ok := validateTSIGKeyWrite(body)
	if !ok {
		errJSON(w, code, msg)
		return
	}
	err = s.deps.Store.TSIGKeys().Update(r.Context(), store.TSIGKey{
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
