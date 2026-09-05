package zones

import (
	"fmt"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
)

// Render writes z and recs out as a standard BIND master file: an $ORIGIN
// and $TTL directive, the SOA built from z's stored fields, then one line
// per enabled record. Record names are already relative to the apex
// ("@", "bifrost", "*.nexus") and rdata is already presentation format, so
// both pass through unchanged — this is assembly, not serialisation.
//
// Disabled records are omitted: a master file has no way to represent
// "present but disabled", so writing one out would silently enable it on
// whatever server imports the file.
func Render(z store.Zone, recs []store.ZoneRecord) string {
	var b strings.Builder

	fmt.Fprintf(&b, "$ORIGIN %s.\n", z.Name)
	// $TTL is the default TTL for records whose line omits one (none of
	// ours do, every record carries its own TTL) and, by convention, also
	// what operators expect to see next to the SOA's own TTL. Both are
	// SOATTL, not SOAMinimum: SOAMinimum is the negative-cache TTL carried
	// in the SOA's MINIMUM field, a distinct value (RFC 2308 §5).
	fmt.Fprintf(&b, "$TTL %d\n", z.SOATTL)
	fmt.Fprintf(&b, "@ %d IN SOA %s. %s. (\n", z.SOATTL, z.SOANS, z.SOAMbox)
	fmt.Fprintf(&b, "\t%d ; serial\n", z.SOASerial)
	fmt.Fprintf(&b, "\t%d ; refresh\n", z.SOARefresh)
	fmt.Fprintf(&b, "\t%d ; retry\n", z.SOARetry)
	fmt.Fprintf(&b, "\t%d ; expire\n", z.SOAExpire)
	fmt.Fprintf(&b, "\t%d ) ; minimum\n", z.SOAMinimum)

	// A stub's out-of-zone glue is the one stored name that is not relative to
	// the apex, and writing it out under $ORIGIN would address a name the
	// delegation does not name. Every other zone type — and every other name a
	// stub holds — is relative and passes through unchanged. See
	// stubAbsoluteNames for why the name alone cannot decide this.
	absolute := stubAbsoluteNames(z, recs)

	for _, r := range recs {
		if !r.Enabled {
			continue
		}
		name := r.Name
		if absolute[normalizeName(name)] {
			// The trailing dot is the whole of the fix: it is what makes a
			// master file read the owner as the name it already is rather
			// than as a name under the origin.
			name = normalizeName(name) + "."
		}
		fmt.Fprintf(&b, "%s %d IN %s %s\n", name, r.TTL, r.Type, r.RData)
	}

	return b.String()
}
