package upstream

import "github.com/miekg/dns"

// paddingBlock is RFC 8467 §4.1's recommended client block size: every
// query is rounded up to a multiple of this, so its length says nothing
// about the name inside it.
const paddingBlock = 128

// padQuery pads m to a multiple of block octets using the EDNS(0) Padding
// option (RFC 7830, code 12).
//
// Only the encrypted exchangers call it. Padding a plaintext query hides
// nothing — the name is right there — and only wastes bytes.
//
// Idempotent: an existing padding option is reused and re-sized rather than
// added to, so a query padded before a failed attempt and retried on a
// fresh connection is padded once.
func padQuery(m *dns.Msg, block int) error {
	opt := m.IsEdns0()
	if opt == nil {
		// Padding has nowhere to live without an OPT record. 1232 is the
		// advertised UDP size used everywhere else in dnsaur; over a stream
		// transport it is inert, and the reply comes back over the same
		// stream regardless.
		m.SetEdns0(1232, false)
		opt = m.IsEdns0()
	}
	var pad *dns.EDNS0_PADDING
	for _, o := range opt.Option {
		if p, ok := o.(*dns.EDNS0_PADDING); ok {
			pad = p
			break
		}
	}
	if pad == nil {
		pad = &dns.EDNS0_PADDING{}
		opt.Option = append(opt.Option, pad)
	}
	// Measured with the option present but empty, so the four bytes of
	// option header are already counted and the padding added below is the
	// only thing that still has to fit.
	pad.Padding = nil
	wire, err := m.Pack()
	if err != nil {
		return err
	}
	if n := (block - len(wire)%block) % block; n > 0 {
		pad.Padding = make([]byte, n)
	}
	return nil
}

// stripPadding removes the Padding option from m's OPT record.
//
// The OPT record itself is kept even when padding was its only option: it
// also carries the DO bit, the extended rcode bits and the advertised UDP
// size, and dropping it to save eleven bytes would discard those. Called on
// every encrypted reply before it is returned, so neither the cache nor the
// client ever sees padding that meant something only on the TLS hop.
func stripPadding(m *dns.Msg) {
	opt := m.IsEdns0()
	if opt == nil {
		return
	}
	kept := opt.Option[:0]
	for _, o := range opt.Option {
		if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
			continue
		}
		kept = append(kept, o)
	}
	opt.Option = kept
}
