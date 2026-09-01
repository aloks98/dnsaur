package dnssrv

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// TransferTimeout bounds one outbound zone transfer. It replaces the
// pipeline's 5-second handler context, which would abort a large transfer
// mid-zone — the first of the two consequences §9.1 of the zones design
// measured before this existed.
const TransferTimeout = 2 * time.Minute

// Transfers answers AXFR and IXFR, which the pipeline cannot: Handler returns
// exactly one *Response and serve writes exactly one message, while a transfer
// is a sequence of messages on one connection. So this takes the raw
// dns.ResponseWriter and owns the whole reply.
//
// key and tsigErr are what Server.RequireTSIG concluded about this message,
// passed rather than recomputed: two callers of one check are two chances for
// them to disagree, and this is the path where the disagreement would be a
// transfer served to an unauthenticated peer.
//
// Implementations must write exactly one reply on the refusal paths and are
// responsible for their own rcodes; nothing downstream inspects what they
// wrote. q carries exactly one question, of qtype AXFR or IXFR — see
// isTransferQuery, and the library gate above it.
type Transfers interface {
	ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error)
}

// WithTransfers attaches the handler AXFR and IXFR are routed to. Without it
// the branch is not taken at all and a transfer query falls through the
// pipeline as any other query would, which is what every server built before
// Milestone D3 did.
func WithTransfers(t Transfers) Option {
	return func(s *Server) { s.transfers = t }
}

// isTransferQuery reports whether m is the kind of query the intercept owns:
// exactly one question, because a transfer names one zone, asking for AXFR or
// IXFR.
//
// The one-question half is not what keeps a malformed transfer out of the
// pipeline, and it is worth knowing which layer does. miekg rejects any
// message whose header QDCOUNT is not 1 with FORMERR of its own accord —
// DefaultMsgAcceptFunc (acceptfunc.go:44), applied at server.go:639-660,
// before Server.serve is ever called — and dnsaur does not replace that
// accept function. So a two-question AXFR is answered FORMERR, which is
// §9.5.5's own rcode for it, without reaching either branch here, and the
// question count is 0 or 1 by the time this runs.
//
// Zero is reachable, by exactly one shape: a message whose header claims one
// question and whose body is empty, which the accept function passes and
// unpack hands back with no Question at all (msg.go:830-835). It has no qtype
// to route on, so it goes to the pipeline, where QName and QType are written
// for it (pipeline.go:40-52). ServeTransfer's own question check is therefore
// a guard for a direct caller — the interface is exported — rather than a row
// a peer can reach. transfers_test.go pins both.
func isTransferQuery(m *dns.Msg) bool {
	if len(m.Question) != 1 {
		return false
	}
	switch m.Question[0].Qtype {
	case dns.TypeAXFR, dns.TypeIXFR:
		return true
	}
	return false
}
