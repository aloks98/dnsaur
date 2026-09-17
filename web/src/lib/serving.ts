import type {
  CertificateStatus,
  DHCPStatus,
  ProtocolStatus,
  ResolverStatus,
  SyncStatus,
} from "../api/types";
import { formatDuration } from "./format";

/**
 * A protocol's reality, reduced to the three states spec §8 and the
 * Protocols artboard both read off an intent/reality pair:
 *
 * - `off` — the setting itself is false. Reality is moot.
 * - `listening` — enabled, and the socket is actually open.
 * - `failed` — enabled, but the bind didn't happen (a privileged port
 *   already taken, a certificate that stopped reading) — see
 *   ProtocolStatus.error for why.
 *
 * Shared between protocols-field.tsx's per-row line and serving-banners.tsx's
 * "enabled but not listening" banner, so the two never drift on what
 * counts as broken.
 */
type ServingState = "off" | "listening" | "failed";

export function servingState(status: ProtocolStatus): ServingState {
  if (!status.enabled) return "off";
  return status.listening ? "listening" : "failed";
}

/**
 * Whether anything in the resolver's status is worth watching — the one
 * predicate the status poll and the shell banners both read.
 *
 * It lives beside `servingState` for the same reason `servingState` exists:
 * the poll used to be gated on `encryption_downgraded` alone, and when this
 * milestone added two more facts to the same endpoint nobody widened it, so
 * a bind failure that later cleared left its banner up until the operator
 * navigated away and back. One predicate is what keeps "we show a banner
 * for this" and "we keep asking about this" from drifting apart again.
 *
 * `undefined` is not "wrong": a status that has not loaded, or would not
 * load, is unknown, and the retry policy for that belongs to the query, not
 * here.
 */
export function somethingIsWrong(status: ResolverStatus | undefined): boolean {
  if (!status) return false;
  return (
    status.encryption_downgraded ||
    servingState(status.serving.dot) === "failed" ||
    servingState(status.serving.doh) === "failed" ||
    (status.certificate?.expiring_soon ?? false) ||
    syncTrouble(status.sync) ||
    dhcpTrouble(status.dhcp)
  );
}

/**
 * The DHCP facts worth asking again about, and exactly the three the strip
 * shows — one predicate, so "we show a banner for this" and "we keep asking
 * about this" cannot drift apart (the mistake this file's header describes).
 *
 * All three end without anyone doing anything: an engine that was restarted
 * answers again, a partner that was rebooted comes back, and a refused
 * configuration clears the moment a render is accepted — including the
 * render a *scope edit* performs, which is what makes polling worth the
 * request rather than leaving a stale line up until the operator navigates
 * away and back.
 */
function dhcpTrouble(dhcp: DHCPStatus | undefined): boolean {
  return dhcpBanners(dhcp).length > 0;
}

/**
 * DHCP's failures, as the lines the shell shows (spec §8.4).
 *
 * Only failures, on the same rule as syncBanners above: DHCP being off is
 * not a fault and gets no line, and neither does a healthy pair. A box with
 * no engine (`enabled: false`) is the whole of what most instances are, and
 * a strip that warned about that would be a strip nobody reads.
 *
 * `config rejected` carries the engine's own words rather than a
 * translation: the message is Kea's refusal of a configuration dnsaur
 * built, and paraphrasing it would leave the operator matching an
 * approximation against the engine's log.
 *
 * The rejection and the unreachable engine are one line, not two, because
 * the server already ranks them — a configuration the engine would not take
 * outranks an engine that is not there, since it is the one an operator has
 * to act on and is still true when the engine comes back.
 */
export function dhcpBanners(dhcp: DHCPStatus | undefined, engineLineOnPage = false): string[] {
  if (!dhcp?.enabled) return [];
  const lines: string[] = [];
  // The Scopes page carries the engine's own status line, with Apply again
  // beside it: the strip saying the same words above it is two warnings
  // for one fact. The partner line stays, since the engine line does not
  // say that.
  if (!engineLineOnPage) {
    if (dhcp.engine === "unreachable") lines.push("DHCP engine unreachable");
    if (dhcp.engine === "config rejected") {
      lines.push(`DHCP config rejected: ${dhcp.message ?? ""}`);
    }
  }
  if (dhcp.ha?.communication_interrupted ?? false) lines.push("DHCP partner unreachable");
  return lines;
}

/**
 * The sync facts worth asking again about: a `last_error`, and a replica
 * that has gone quiet. A replica's two end without anyone doing anything —
 * the next successful pull, the next check-in — which is what makes asking
 * again worth the request. A main's `last_error` is an unreadable
 * `sync.replicas` and waits on the operator, but it is watched all the same:
 * the poll is what takes the line down once the row is repaired.
 *
 * `plain_http` is neither watched here nor shown as a banner: it is a
 * reading of the peer URL the operator typed, so nothing but an edit to
 * that URL can change the answer. It is stated once, in the Sync band
 * beside the peer it describes (see components/sync-field.tsx).
 */
function syncTrouble(sync: SyncStatus | undefined): boolean {
  if (!sync) return false;
  return Boolean(sync.last_error) || (sync.replicas ?? []).some((r) => r.stale);
}

/**
 * Sync's failures, as the lines the shell shows (spec §8) — the one
 * definition of what is on screen, so a caller cannot invent another line
 * or spell one of these differently.
 *
 * Only failures. Being a replica is a *state*, and it reads as the top
 * bar's role chip and one muted line per synced screen; a plaintext peer is
 * a standing property of the peer URL and reads in the Sync band beside it.
 * A page-wide strip that carried those too would warn about the normal
 * case, which is how an operator learns to skip the strip.
 *
 * Two of the three clear on their own — the next pull that succeeds empties
 * a replica's `last_error`, and a replica that checks in stops being stale —
 * which is exactly what makes `syncTrouble` worth polling for. A main's
 * `last_error` is the exception: `sync.replicas` is unreadable until
 * somebody repairs the row, so that line stands until they do.
 *
 * Every line states the fact and stops. A replica that is merely *behind*
 * gets no line at all: that is what a pull interval looks like from the
 * outside, and the Sync band's "applied n of m" already says it on the one
 * screen where the number is worth reading.
 */
export function syncBanners(sync: SyncStatus | undefined, now: number = Date.now()): string[] {
  if (!sync) return [];
  const lines: string[] = [];
  // The same field, two failures. On a replica it is the last pull; on a
  // main it is the registry row that could not be read, and there is no
  // pull to have failed.
  if (sync.last_error)
    lines.push(
      sync.role === "replica" ? `Last pull failed: ${sync.last_error}` : `Sync: ${sync.last_error}`,
    );
  for (const replica of sync.replicas ?? []) {
    if (!replica.stale) continue;
    // How long it has been quiet, not when it was last heard: an operator
    // should not have to subtract a wall-clock stamp from now.
    lines.push(
      `Replica ${replica.instance_id} not seen for ${formatDuration(now - replica.last_seen)}`,
    );
  }
  return lines;
}

const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
] as const;

/**
 * "14 Nov 2026" — the exact shape the Protocols artboard's certificate line
 * and the certificate-expiring shell banner both use.
 *
 * Built from UTC calendar fields, not the browser's local timezone: the
 * server's `not_after` is an instant, and reading it through whichever
 * timezone a viewer happens to be in would make two operators watching the
 * same server disagree on the date printed — and would make this
 * untestable without pinning the test runner's TZ. `toLocaleDateString` was
 * also ruled out for the same determinism reason: its short-month spelling
 * of September varies by locale/ICU data (`Sep` vs `Sept`), which the
 * artboard's exact string does not allow for.
 */
export function formatCertDate(date: Date): string {
  return `${date.getUTCDate()} ${MONTHS[date.getUTCMonth()]} ${date.getUTCFullYear()}`;
}

/** Whole days remaining until `notAfter`, floored at 0 so a certificate
 * that has already lapsed reads as "0 days" rather than a negative number.
 *
 * Floor, not round: "whole days remaining" is what the caller renders, and
 * rounding turns 36 hours into "2 days" — a day of grace the operator does
 * not have. A certificate 12 hours from lapsing reads "0 days", which is
 * the honest number. */
export function daysUntil(notAfter: Date, now: Date = new Date()): number {
  return Math.max(0, Math.floor((notAfter.getTime() - now.getTime()) / 86_400_000));
}

/** "Expires in 9 days — 17 Sep 2026", the shell banner and (with a
 * different lead-in) the certificate line's shape for an expiring
 * certificate — built once here so the two copies of "N days — DATE" can't
 * drift apart.
 *
 * `expired` is separate from `days === 0`, and both callers need it: the
 * server reports a lapsed certificate as ok=true with expiring_soon=true
 * (internal/app/serve.go's CertExpiry), so without this the screen said
 * "expires in 0 days" about a certificate that expired last week. */
export function expiringSoonDetail(
  certificate: CertificateStatus,
  now: Date = new Date(),
): { days: number; date: string; expired: boolean } {
  const notAfter = new Date(certificate.not_after);
  return {
    days: daysUntil(notAfter, now),
    date: formatCertDate(notAfter),
    expired: notAfter.getTime() <= now.getTime(),
  };
}
