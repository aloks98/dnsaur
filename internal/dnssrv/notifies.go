package dnssrv

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// NotifyTimeout bounds answering one NOTIFY. It is much smaller than
// TransferTimeout because the reply is one message and goes out *before* any
// work is done: RFC 1996 §4.7 has the responder enter its refresh state, and
// §3.6 has the sender retransmitting until it gets a response, so a responder
// that waited for a transfer would earn a second NOTIFY for the transfer
// already in flight. The probe and the transfer take their own background
// context — see zones.NotifyServer.
const NotifyTimeout = 10 * time.Second

// Notifies answers NOTIFY, which the pipeline must not.
//
// The justification is deliberately weaker than Transfers'. A transfer
// *cannot* be a Handler — Handler returns one *Response and a transfer is a
// sequence of messages — while a NOTIFY reply is a single message and would
// fit it mechanically. It still must not go through the chain: qlog's
// per-query accounting, the filter, the cache and the forwarder have no
// meaning for it, and before this interface existed the consequence was
// concrete. A NOTIFY for an apex this server does not hold fell through every
// middleware to the terminal forwarder and was sent upstream, so dnsaur asked
// its own upstream resolver an SOA question on behalf of a peer that was
// trying to notify it.
//
// key and tsigErr are what Server.RequireTSIG concluded about this message,
// passed rather than recomputed, for the reason Transfers documents: two
// callers of one check are two chances to disagree, and here the disagreement
// would be a NOTIFY acted on without the signature the zone requires.
//
// Implementations own the whole reply and are responsible for their own
// rcodes; nothing downstream inspects what they wrote. m carries exactly one
// question — miekg's DefaultMsgAcceptFunc rejects any other QDCOUNT with
// FORMERR before serve runs — of whatever qtype the sender chose, which the
// implementation is expected to check.
type Notifies interface {
	ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error)
}

// WithNotifies attaches the handler NOTIFY is routed to. Without it the
// branch is not taken and a NOTIFY falls through the pipeline as any other
// query would, which is what every server built before Milestone D4 did.
func WithNotifies(n Notifies) Option {
	return func(s *Server) { s.notifies = n }
}

// isNotify reports whether m is a NOTIFY: an opcode, not a qtype, which is
// exactly why isTransferQuery does not catch it and why this is a second
// branch rather than another case in that one.
//
// The question count is not checked here. miekg rejects any message whose
// header QDCOUNT is not 1 with FORMERR of its own accord
// (DefaultMsgAcceptFunc, acceptfunc.go:44, applied at server.go:639-660), and
// dnsaur does not replace that accept function — the same reasoning
// isTransferQuery records. Zero questions remains reachable by the one shape
// that comment describes, so ServeNotify checks for itself rather than
// assuming a question exists.
func isNotify(m *dns.Msg) bool {
	return m.Opcode == dns.OpcodeNotify
}
