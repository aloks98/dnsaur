package records

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type snapshot struct {
	byName map[string][]store.LocalRecord // lowercase name -> records (wildcards keyed as "*.parent")
}

type Resolver struct {
	rs   store.RecordStore
	snap atomic.Pointer[snapshot]
}

func NewResolver(rs store.RecordStore) *Resolver {
	r := &Resolver{rs: rs}
	r.snap.Store(&snapshot{byName: map[string][]store.LocalRecord{}})
	return r
}

func (r *Resolver) Reload(ctx context.Context) error {
	all, err := r.rs.All(ctx)
	if err != nil {
		return err
	}
	s := &snapshot{byName: map[string][]store.LocalRecord{}}
	for _, rec := range all {
		k := strings.ToLower(strings.TrimSuffix(rec.Name, "."))
		s.byName[k] = append(s.byName[k], rec)
	}
	r.snap.Store(s)
	return nil
}

// lookup finds records for name: exact first, then wildcard walking up.
func (s *snapshot) lookup(name string) ([]store.LocalRecord, bool) {
	if recs, ok := s.byName[name]; ok {
		return recs, true
	}
	labels := strings.Split(name, ".")
	for i := 1; i < len(labels); i++ {
		if recs, ok := s.byName["*."+strings.Join(labels[i:], ".")]; ok {
			return recs, true
		}
	}
	return nil, false
}

func toRR(qname string, rec store.LocalRecord) (dns.RR, error) {
	hdr := dns.RR_Header{Name: dns.Fqdn(qname), Class: dns.ClassINET, Ttl: rec.TTL}
	switch strings.ToUpper(rec.Type) {
	case "A":
		ip := net.ParseIP(rec.Value)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("record %d: invalid IPv4 %q", rec.ID, rec.Value)
		}
		hdr.Rrtype = dns.TypeA
		return &dns.A{Hdr: hdr, A: ip.To4()}, nil
	case "AAAA":
		ip := net.ParseIP(rec.Value)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("record %d: invalid IPv6 %q", rec.ID, rec.Value)
		}
		hdr.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: hdr, AAAA: ip}, nil
	case "CNAME":
		hdr.Rrtype = dns.TypeCNAME
		return &dns.CNAME{Hdr: hdr, Target: dns.Fqdn(strings.ToLower(rec.Value))}, nil
	case "TXT":
		hdr.Rrtype = dns.TypeTXT
		return &dns.TXT{Hdr: hdr, Txt: []string{rec.Value}}, nil
	}
	return nil, fmt.Errorf("record %d: unsupported type %q", rec.ID, rec.Type)
}

func (r *Resolver) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			s := r.snap.Load()
			qname := req.QName()
			qtypeStr := dns.TypeToString[req.QType()]
			if _, ok := s.lookup(qname); !ok {
				return next.ServeDNS(ctx, req)
			}
			m := new(dns.Msg)
			m.SetReply(req.Msg)
			m.Authoritative = true
			name := qname
			for hop := 0; hop < 8; hop++ {
				cur, ok := s.lookup(name)
				if !ok {
					// target is remote: resolve via the rest of the pipeline and merge
					sub := new(dns.Msg)
					sub.SetQuestion(dns.Fqdn(name), req.QType())
					subResp, err := next.ServeDNS(ctx, &dnssrv.Request{Msg: sub, ClientIP: req.ClientIP, Client: req.Client})
					if err != nil {
						m.Rcode = dns.RcodeServerFailure
					} else if subResp.Msg != nil {
						m.Answer = append(m.Answer, subResp.Msg.Answer...)
						if subResp.Msg.Rcode != dns.RcodeSuccess {
							m.Rcode = subResp.Msg.Rcode
						}
					}
					break
				}
				var cname *store.LocalRecord
				matched := false
				for i := range cur {
					rec := cur[i]
					if strings.EqualFold(rec.Type, qtypeStr) {
						if rr, err := toRR(name, rec); err == nil {
							m.Answer = append(m.Answer, rr)
							matched = true
						}
					}
					if strings.EqualFold(rec.Type, "CNAME") {
						cname = &cur[i]
					}
				}
				if matched || cname == nil || req.QType() == dns.TypeCNAME {
					break
				}
				if rr, err := toRR(name, *cname); err == nil {
					m.Answer = append(m.Answer, rr)
				}
				name = strings.ToLower(strings.TrimSuffix(cname.Value, "."))
			}
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionLocal}, nil
		})
	}
}
