package zones

// SerialNewer reports whether serial a is strictly newer than serial b under
// RFC 1982 §3.2's circular arithmetic.
//
// A DNS serial is a uint32 that wraps, so ordinary `>` is wrong at exactly
// one place and right everywhere else. §3.2 defines s1 to be greater than s2
// when the forward distance from s2 to s1 is less than half the space; in
// unsigned arithmetic that whole definition collapses to the subtraction
// below, which wraps for free.
//
// **The comparison is not total, and that is the RFC's own doing.** §3.2
// leaves two serials exactly 2^31 apart with no defined ordering — the
// forward and backward distances are equal, so neither is "closer". This
// returns false for that pair, in both directions.
//
// The policy is a choice between two unequal mistakes rather than a
// preference. A transfer that does not happen is recovered by the next
// refresh timer, at the cost of some staleness. A transfer that should not
// have happened is a full AXFR of someone else's zone — and on the outbound
// side, an undefined pair resolving to "newer" would mean every target is
// notified on every pass, with nothing to stop it, because the comparison
// that decides "there is news" would never come out false.
func SerialNewer(a, b uint32) bool {
	// Wraps by definition of uint32 subtraction: for a=0, b=4294967295 this
	// is 1, which is what makes 0 the successor of the maximum.
	d := a - b
	// d == 0 is equality, d == 1<<31 is §3.2's undefined pair, and everything
	// above it is a backward distance. Only the open interval is "newer".
	return d != 0 && d < 1<<31
}
