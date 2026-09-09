package dnssrv

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// PipelineTimeout bounds one ordinary query's trip through the middleware
// chain, from the listener handing it over to the terminal forwarder's
// answer coming back.
//
// It is the number TransferTimeout and NotifyTimeout are each defined
// against — both exist because a transfer and a NOTIFY need a deadline of
// their own rather than this one — and it was a bare literal repeated at
// every listener while its two siblings were named constants beside it.
const PipelineTimeout = 5 * time.Second

type Decision string

const (
	DecisionBlocked       Decision = "blocked"
	DecisionAuthoritative Decision = "authoritative"
	DecisionCached        Decision = "cached"
	DecisionStale         Decision = "stale"
	DecisionForwarded     Decision = "forwarded"
	DecisionError         Decision = "error"
)

type ClientInfo struct {
	ID        int64
	Name      string
	GroupID   int64
	GroupName string
}

type Request struct {
	Msg      *dns.Msg
	ClientIP netip.Addr
	Client   ClientInfo
	// tsig is what TSIG verification concluded about this message, filled in
	// by Server.serve. Unexported and read through RequireTSIG so a handler
	// cannot mistake "nobody checked" for "checked and fine"; see tsig.go.
	tsig *tsigState
}

func (r *Request) QName() string {
	if len(r.Msg.Question) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(r.Msg.Question[0].Name, "."))
}

func (r *Request) QType() uint16 {
	if len(r.Msg.Question) == 0 {
		return 0
	}
	return r.Msg.Question[0].Qtype
}

type Response struct {
	Msg      *dns.Msg
	Decision Decision
	Upstream string
	RuleID   int64
	ListID   int64
	// Matched is the rule pattern or list entry that fired, for a blocked
	// response. The ids alone say which row decided, not what in it
	// matched, which is the one thing "why was this blocked?" needs — an
	// entry inside a million-line list is not findable from its list id.
	Matched string
}

type Handler interface {
	ServeDNS(ctx context.Context, req *Request) (*Response, error)
}

type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

func (f HandlerFunc) ServeDNS(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

type Middleware func(next Handler) Handler

func Chain(terminal Handler, mws ...Middleware) Handler {
	h := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func Servfail(req *Request) *Response {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, dns.RcodeServerFailure)
	return &Response{Msg: m, Decision: DecisionError}
}

func Recover() Middleware {
	return func(next Handler) Handler {
		return HandlerFunc(func(ctx context.Context, req *Request) (resp *Response, err error) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in dns pipeline", "qname", req.QName(), "panic", r)
					resp, err = Servfail(req), nil
				}
			}()
			return next.ServeDNS(ctx, req)
		})
	}
}
