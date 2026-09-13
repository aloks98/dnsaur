package api

// PeerURLValidator is the settings grammar for sync.peer_url, exported for
// the agreement test in package api_test (peerurl_agreement_test.go).
//
// That test imports internal/confsync, which is an import this package
// cannot make: confsync imports internal/api for api.Replica, api.PairResult
// and the rest, so the dependency only runs one way. An external test
// package is the one place both sides can be named at once.
var PeerURLValidator = peerURL
