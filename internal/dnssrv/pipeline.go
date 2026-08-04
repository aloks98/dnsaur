package dnssrv

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

type Decision string

const (
	DecisionAllowed   Decision = "allowed"
	DecisionBlocked   Decision = "blocked"
	DecisionLocal     Decision = "local"
	DecisionCached    Decision = "cached"
	DecisionStale     Decision = "stale"
	DecisionForwarded Decision = "forwarded"
	DecisionError     Decision = "error"
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
