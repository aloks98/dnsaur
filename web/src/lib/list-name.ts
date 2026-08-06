/**
 * Client-side mirror of the server's list-name derivation
 * (`store.DeriveListName`, internal/store/listname.go). The server is the
 * authority — it is what actually gets stored — so this exists for exactly
 * one job: showing the admin, live as they type a URL, what the Add-list
 * dialog will name their list if they leave the Name field blank.
 *
 * Kept honest by list-name.test.ts, which asserts the same table of URLs as
 * the Go TestDeriveListName. Change one, change both.
 *
 * The rule, in order:
 *  1. GitHub raw/blob URLs (`/<owner>/<repo>/<ref>/<path…>`) → owner plus the
 *     path below the ref: `hagezi wildcard/pro.txt`, `StevenBlack hosts`.
 *     The branch is dropped — "main" tells an admin nothing — and the owner
 *     is kept because every hagezi list shares a host, a repo and often a
 *     filename.
 *  2. Anything else → host (minus `www.`) plus the final path segment.
 *  3. No path, or an unparseable URL → the host, or the raw string.
 */
const MAX_DERIVED_NAME_LEN = 60;

function truncate(s: string): string {
  const collapsed = s.split(/\s+/).filter(Boolean).join(" ");
  if (!collapsed) return "unnamed list";
  const chars = [...collapsed];
  if (chars.length <= MAX_DERIVED_NAME_LEN) return collapsed;
  return chars.slice(0, MAX_DERIVED_NAME_LEN - 1).join("") + "…";
}

export function deriveListName(rawUrl: string): string {
  const trimmed = rawUrl.trim();
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return truncate(trimmed);
  }
  if (!parsed.host) return truncate(trimmed);

  const host = parsed.host.replace(/^www\./, "");
  const segs = parsed.pathname.split("/").filter(Boolean);
  if (segs.length === 0) return truncate(host);

  if (host === "raw.githubusercontent.com" || host === "github.com") {
    // Skip owner/repo/ref, plus github.com's extra blob|raw segment.
    const skip = host === "github.com" && (segs[2] === "blob" || segs[2] === "raw") ? 4 : 3;
    // Bounds-checked: a truncated GitHub URL is a real thing to paste, and
    // must fall through to the generic rule rather than yield "owner ".
    if (segs.length > skip) return truncate(`${segs[0]} ${segs.slice(skip).join("/")}`);
  }
  return truncate(`${host} ${segs[segs.length - 1]}`);
}
