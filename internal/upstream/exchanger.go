package upstream

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// exchanger sends one query to one upstream and returns one reply.
//
// Implementations own their transport completely: dialing, connection
// reuse, TLS, and any rewriting the transport requires — 0x20 case
// randomization on plaintext, RFC 8467 padding on the encrypted ones,
// DNS-over-HTTPS's zero message ID. Above this line the Forwarder measures
// latency, counts failures and picks upstreams, and none of that knows or
// cares which transport it got.
type exchanger interface {
	Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
	// Close releases any connections held. Called when a Forwarder is
	// replaced or shut down; safe to call more than once.
	Close() error
}

// plainExchanger is DNS over UDP with a TCP retry on truncation — the only
// transport dnsaur had before this milestone, moved here unchanged.
type plainExchanger struct {
	addr     string
	udp, tcp *dns.Client
}

func newPlainExchanger(addr string, timeout time.Duration) *plainExchanger {
	return &plainExchanger{
		addr: addr,
		udp:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:  &dns.Client{Net: "tcp", Timeout: timeout},
	}
}

// Exchange sends m with 0x20 case randomization and a TCP retry on
// truncation.
//
// 0x20 raises the difficulty of off-path cache poisoning, which is a threat
// specific to unauthenticated plaintext UDP. It is deliberately absent from
// the encrypted exchangers: inside a verified TLS channel there is no
// off-path attacker to defend against, and resolvers that normalize case
// would turn the check into failures against an upstream that is working.
func (e *plainExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	orig := m.Question[0].Name
	rnd := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	m.Question[0].Name = scramble(strings.ToLower(orig), rnd)
	r, _, err := e.udp.ExchangeContext(ctx, m, e.addr)
	if err == nil && r.Truncated {
		r, _, err = e.tcp.ExchangeContext(ctx, m, e.addr)
	}
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("upstream %s: no reply", e.addr)
	}
	if len(r.Question) != 1 || r.Question[0].Name != m.Question[0].Name {
		return nil, fmt.Errorf("upstream %s: 0x20 case check failed", e.addr)
	}
	// Restore the original case everywhere it echoes.
	r.Question[0].Name = orig
	for _, sec := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range sec {
			if strings.EqualFold(rr.Header().Name, orig) {
				rr.Header().Name = orig
			}
		}
	}
	return r, nil
}

// Close is a no-op: dns.Client dials per exchange and holds nothing.
func (e *plainExchanger) Close() error { return nil }

func scramble(name string, rnd *rand.Rand) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'a' && c <= 'z' && rnd.IntN(2) == 1 {
			b[i] = c - 32
		}
	}
	return string(b)
}
