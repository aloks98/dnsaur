package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestTSIGKeyRejectsAnUnsupportedAlgorithm(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-md5.sig-alg.reg.int.","secret":"c2VjcmV0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400 -- MD5 was removed from the library and cannot sign", rec.Code, rec.Body)
	}
}

func TestTSIGKeyRejectsANonBase64Secret(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha256.","secret":"not base64!!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 -- an unusable secret must fail at write, not at the first transfer", rec.Code)
	}
}

// The name is how a signed message finds its key, so "XFER.E412.IN" and
// "xfer.e412.in." must be the same key rather than two.
func TestTSIGKeyNameIsCanonicalised(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"XFER.E412.IN","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	rec := srv.do(t, "GET", "/api/v1/tsig-keys", "")
	if !strings.Contains(rec.Body.String(), `"xfer.e412.in."`) {
		t.Fatalf("name not canonicalised: %s", rec.Body)
	}
}

// TestTSIGKeyLifecycle drives every route this task adds — POST, GET (list
// and by id), PUT, DELETE — through the real Server.Handler(), checking that
// the secret is returned on read (the decision the brief documents) and that
// a create-then-fetch round trip preserves every field.
func TestTSIGKeyLifecycle(t *testing.T) {
	srv := newTestServer(t)

	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha256.","secret":"c2VjcmV0LXNlY3JldC1zZWNyZXQ="}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	id := int64(created["id"].(float64))

	// GET by id returns the secret in the clear -- it has to be, or the peer
	// (BIND/Technitium) can never be configured with a matching key.
	rec = srv.do(t, "GET", "/api/v1/tsig-keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, k := range list {
		if k["name"] == "xfer.e412.in." {
			found = true
			if k["secret"] != "c2VjcmV0LXNlY3JldC1zZWNyZXQ=" {
				t.Fatalf("secret not returned on list: %v", k)
			}
			if k["algorithm"] != "hmac-sha256." {
				t.Fatalf("algorithm mismatch: %v", k)
			}
		}
	}
	if !found {
		t.Fatalf("created key not in list: %v", list)
	}

	rec = srv.do(t, "GET", "/api/v1/tsig-keys/"+itoa(id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	if got["secret"] != "c2VjcmV0LXNlY3JldC1zZWNyZXQ=" {
		t.Fatalf("secret not returned on get: %v", got)
	}

	// PUT replaces the key: a new algorithm and secret, same name.
	rec = srv.do(t, "PUT", "/api/v1/tsig-keys/"+itoa(id),
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha512.","secret":"bmV3LXNlY3JldC1uZXctc2VjcmV0"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	rec = srv.do(t, "GET", "/api/v1/tsig-keys/"+itoa(id), "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get after update: %v", err)
	}
	if got["algorithm"] != "hmac-sha512." || got["secret"] != "bmV3LXNlY3JldC1uZXctc2VjcmV0" {
		t.Fatalf("update did not take: %v", got)
	}

	rec = srv.do(t, "DELETE", "/api/v1/tsig-keys/"+itoa(id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = srv.do(t, "GET", "/api/v1/tsig-keys/"+itoa(id), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d %s", rec.Code, rec.Body)
	}
}

func TestTSIGKeyCreateRejectsInvalidName(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"not a domain","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s; want 400", rec.Code, rec.Body)
	}
}

func TestTSIGKeyDuplicateNameConflicts(t *testing.T) {
	srv := newTestServer(t)
	if rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body)
	}
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"xfer.e412.in.","algorithm":"hmac-sha256.","secret":"b3RoZXI="}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s; want 409", rec.Code, rec.Body)
	}
}

func TestTSIGKeyGetMissingIs404(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.do(t, "GET", "/api/v1/tsig-keys/999999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

// createTSIGKey creates a key through the real POST handler and returns its
// id, for tests whose subject is something that references a key rather than
// the key CRUD itself.
func createTSIGKey(t *testing.T, srv *zoneTestServer, name string) int64 {
	t.Helper()
	rec := srv.do(t, "POST", "/api/v1/tsig-keys",
		`{"name":"`+name+`","algorithm":"hmac-sha256.","secret":"c2VjcmV0LXNlY3JldC1zZWNyZXQ="}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key %q: %d %s", name, rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	return created.ID
}

// The guard D1 deferred: a key a zone depends on must not vanish silently.
// Deleting it would leave the secondary unable to authenticate its transfers
// with nothing on the zone to say why.
func TestDeletingATSIGKeyInUseIsRefused(t *testing.T) {
	srv := newTestServer(t)
	keyID := createTSIGKey(t, srv, "xfer.e412.in.")
	if rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","type":"secondary","primaries":"192.168.150.5","tsig_key_id":`+itoa(keyID)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("create zone: %d %s", rec.Code, rec.Body)
	}

	rec := srv.do(t, "DELETE", "/api/v1/tsig-keys/"+itoa(keyID), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 — deleting it would leave the zone unable to authenticate", rec.Code)
	}
	if _, found, err := srv.store.TSIGKeys().Get(t.Context(), keyID); err != nil || !found {
		t.Fatalf("key gone after a refused delete: found=%v err=%v", found, err)
	}
}

// The other half: the guard must not turn every delete into a 409. A key no
// zone references still goes.
func TestDeletingAnUnusedTSIGKeySucceeds(t *testing.T) {
	srv := newTestServer(t)
	used := createTSIGKey(t, srv, "used.e412.in.")
	unused := createTSIGKey(t, srv, "unused.e412.in.")
	if rec := srv.do(t, "POST", "/api/v1/zones",
		`{"name":"e412.in","type":"secondary","primaries":"192.168.150.5","tsig_key_id":`+itoa(used)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("create zone: %d %s", rec.Code, rec.Body)
	}
	if rec := srv.do(t, "DELETE", "/api/v1/tsig-keys/"+itoa(unused), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", rec.Code)
	}
}
