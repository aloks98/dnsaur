import type { Zone } from "../api/types";

/**
 * What a secondary zone's transfer state is, derived from the zone row alone.
 *
 * Every fact used here is a column, and that is the point. The scheduler
 * keeps a richer picture in memory — the consecutive failure count, the
 * pending retry back-off, the error text (zones.Refresher.Status) — but all
 * of that is process-local and dies with the process, so a UI built on it
 * would show a zone that had been failing for a week as healthy for up to a
 * retry interval after every restart.
 *
 * `last_error` and `last_attempt` are the durable half of it (see the 0010
 * migration), written together in one statement by the scheduler and cleared
 * on success. So the screens can state both the state *and* the cause, and
 * both are as current after a restart as before one.
 */

/**
 * The scheduler's floor under every interval taken from an SOA — mirrors
 * `minInterval` in internal/zones/refresh.go. A primary can publish a refresh
 * of 0 (or 1) by accident, and this server clamps rather than obeying it, so
 * computing "next refresh" off the raw SOA value would promise a poll that
 * will not happen.
 */
export const MIN_REFRESH_INTERVAL_MS = 60_000;

/**
 * How often the scheduler asks which zones are due — `refreshTick` in
 * internal/zones/refresh.go. It bounds how *late* an attempt can be, which is
 * why it is the grace period below rather than anything chosen for looks: a
 * zone one second past its deadline is not overdue, it is waiting for the
 * next tick.
 */
export const REFRESH_TICK_MS = 30_000;

/** The interval the scheduler will actually use for this zone, clamped. */
export function refreshIntervalMs(zone: Zone): number {
  return Math.max(zone.soa_refresh * 1000, MIN_REFRESH_INTERVAL_MS);
}

/** The retry interval a failing zone is re-attempted on, clamped the same way. */
export function retryIntervalMs(zone: Zone): number {
  return Math.max(zone.soa_retry * 1000, MIN_REFRESH_INTERVAL_MS);
}

/** When the ordinary schedule next comes round for this zone. */
export function nextRefreshAt(zone: Zone): number {
  return zone.refreshed_at + refreshIntervalMs(zone);
}

/**
 * Why the most recent transfer attempt failed, and when it was made — or null
 * when the most recent attempt succeeded, which is what an empty `last_error`
 * means (the scheduler clears it on success, so a recovered zone stops
 * reporting one).
 *
 * The timestamp comes back with the message on purpose. An error with no date
 * is a claim about the present made by an unknown past: "connection refused"
 * means one thing four minutes after the fact and quite another four days
 * after it, and only the second number tells them apart.
 */
export function lastTransferError(zone: Zone): { message: string; at: number } | null {
  if (zone.last_error === "") return null;
  // The two columns are written together in one statement, so ordinarily an
  // error and the success stamp cannot disagree. The exception is a success
  // whose bookkeeping write failed — best-effort, logged, see
  // Refresher.recordAttempt — which can leave an error older than the last
  // success sitting in the column. Ordering them is what stops that one
  // showing as current.
  if (zone.last_attempt < zone.refreshed_at) return null;
  return { message: zone.last_error, at: zone.last_attempt };
}

/**
 * The part of a transfer error worth reading first.
 *
 * `last_error` is one opaque string — whatever the transfer returned — and for
 * a Go error that is a chain of contexts joined by ": ", outermost first. So
 * the last segment is the innermost failure ("connection refused") and
 * everything in front of it is context the band it appears in already carries:
 * the zone's own name and the primary that was tried.
 *
 * This is a *presentation* split of one string, not a parse of it. Nothing
 * here claims to know what the wording means or to have understood the shape
 * of the message: what comes back is a verbatim suffix, and the caller shows
 * the whole message beside it (a second line on the zone detail page, the
 * `title` on the zones list). So a server that rewords or re-wraps its errors
 * tomorrow costs a nicer first line and nothing else — no text is hidden by
 * getting this wrong, which is the only reason a derivation from unstructured
 * text is defensible at all.
 *
 * When there is no ": " to cut on, or nothing on one side of the last one, the
 * whole message comes back unchanged and there is nothing left to show
 * separately.
 */
export function transferErrorLead(message: string): string {
  const cut = message.lastIndexOf(": ");
  if (cut === -1) return message;
  const head = message.slice(0, cut).trim();
  const tail = message.slice(cut + 2).trim();
  // Both halves have to be real for the cut to be worth making: a message that
  // ends in ": " has no failure after it, and one that starts with it has no
  // context in front of it to set aside.
  if (head === "" || tail === "") return message;
  return tail;
}

/**
 * One of five states, in the order they have to be checked.
 *
 * - `never` — no transfer has ever succeeded. Whether one has been *tried* is
 *   a separate question (`last_attempt`), and the failure that answers it
 *   shows up as the cause beside this state rather than as a state of its
 *   own: either way the zone holds nothing and answers nothing.
 * - `expired` — past the SOA expiry with nothing having succeeded since.
 *   Checked before `failing` because it is the stronger claim: a failing zone
 *   is still serving, an expired one is not.
 * - `failing` — the last attempt failed and said why. Still serving the copy
 *   it has.
 * - `overdue` — no failure recorded, but the refresh deadline came and went
 *   and the copy is still the old one. This is the scheduler not having
 *   reached the zone: it is disabled (skipped entirely), or the process has
 *   only just started. Still serving.
 * - `fresh` — refreshed within the schedule.
 *
 * `never` and `expired` are the two in which the zone answers SERVFAIL rather
 * than serving anything; they are exactly the negation of Zone.Serving in
 * internal/zones/answer.go, and are deliberately derived here by the same two
 * conditions in the same order.
 */
export type TransferState = "never" | "expired" | "failing" | "overdue" | "fresh";

/**
 * **A secondary's states, and only a secondary's.** `expired` is the reason
 * this is not the general answer: `expires_at` is written by a transfer and
 * by nothing else, so on any other type it is 0 — and a row retyped from
 * secondary to stub keeps whatever value it had (see
 * internal/zones/refresh.go, which makes the same carve-out for exactly this
 * reason). A stub is *never given* an expiry, deliberately: its NS set is
 * routing information, and an old-but-working nameserver beats a
 * self-inflicted SERVFAIL. Run one through here and it either dates to the
 * epoch or expires on a stamp that stopped meaning anything the moment it
 * stopped being a secondary.
 *
 * A stub's own state is read off `refreshed_at` and `lastTransferError`
 * alone — neither of which involves an expiry — by `pullState` below, which
 * is what the zone screens ask when the type is not known to be a secondary.
 */
export function transferState(zone: Zone, now: number = Date.now()): TransferState {
  if (zone.refreshed_at === 0) return "never";
  if (zone.expires_at !== 0 && now >= zone.expires_at) return "expired";
  if (lastTransferError(zone) !== null) return "failing";
  // A tick of slack: the scheduler asks which zones are due every 30s, so a
  // zone a moment past its deadline is waiting, not failing. Without this the
  // detail page would flicker into "overdue" once per refresh interval on a
  // perfectly healthy zone.
  if (now >= nextRefreshAt(zone) + REFRESH_TICK_MS) return "overdue";
  return "fresh";
}

/**
 * The two types that pull from a master — the server's own `pullsFromAMaster`
 * (internal/api/zones_handlers.go), which is what decides that `primaries` is
 * required and `tsig_key_id` allowed, and what the scheduler polls.
 *
 * It is also the gate on every health treatment the zone screens give a row.
 * A **forwarder is deliberately outside it**: it claims a suffix and sends
 * live queries on, so its upstreams' reachability is a fact about this
 * instant and nothing about it is stored. There is no column to read, and a
 * list that showed one would be inventing it.
 */
export function pullsFromAMaster(type: Zone["type"]): boolean {
  return type === "secondary" || type === "stub";
}

/**
 * The state of a pulled zone, asked of either type that pulls — and the whole
 * reason it exists is that only one of them may be asked through an expiry.
 *
 * A stub is **never** run through `transferState`. `expires_at` is written by
 * a transfer and by nothing else, and a row retyped from secondary to stub
 * keeps every stamp the transfer left (handleZonePatch sets the type on a row
 * read from the store and clears none of them), so an ordinary stub can carry
 * a long-past expiry that stopped meaning anything the moment it stopped
 * being a secondary. Reading it puts a zone that is routing perfectly well on
 * screen as answering nothing.
 *
 * So a stub gets three states off `refreshed_at` and `lastTransferError`
 * alone, neither of which involves an expiry — the same two the zone detail
 * page's MASTER row derives its note from:
 *
 * - `never` — nothing has ever landed. It holds no NS set, and every name
 *   under the suffix it claims answers SERVFAIL. Not idle: an outage.
 * - `failing` — the last fetch failed; the set it already has is still being
 *   routed on.
 * - `fresh` — the set is current.
 *
 * `expired` is not among them by design (§9.11.8: an old-but-working
 * nameserver beats a self-inflicted SERVFAIL), and neither is `overdue` —
 * that state exists to stop a copy of unknown age reading as current, and a
 * stub's row dates its set outright ("Fetched 5d ago") instead of claiming
 * anything about it.
 *
 * Only meaningful for a type `pullsFromAMaster` accepts; callers gate on it.
 */
export function pullState(zone: Zone, now: number = Date.now()): TransferState {
  if (zone.type === "secondary") return transferState(zone, now);
  if (zone.refreshed_at === 0) return "never";
  return lastTransferError(zone) !== null ? "failing" : "fresh";
}

/**
 * Whether this zone can answer from the records it holds — the client-side
 * twin of Zone.Serving (internal/zones/answer.go). Only a secondary can fail
 * it: a primary owns its data outright.
 *
 * This is what makes "Enabled" an honest badge. A secondary that has never
 * transferred, or whose data has expired, is `enabled` in the database and
 * answering SERVFAIL to every query — so a screen that read `enabled` alone
 * would call it healthy at exactly the moment it is serving nothing.
 */
export function isServing(zone: Zone, now: number = Date.now()): boolean {
  // A stub is deliberately not in here beside the secondary, even though one
  // that has never fetched routes nothing and SERVFAILs its whole suffix. It
  // holds no data on loan, so there is no state it can fall out of; what it
  // has instead is a fetch that has not landed yet, which the MASTER row
  // dates and says outright. Zone.Serving on the server draws the line in the
  // same place.
  if (zone.type !== "secondary") return true;
  const state = transferState(zone, now);
  return state !== "never" && state !== "expired";
}

/**
 * The upstreams a forwarder zone names, one per entry.
 *
 * A count, not a parse: `forward_to` is read back in the server's own
 * canonical spelling (`FormatForwardTo` — port always written, ", "-
 * separated), so splitting it is reading a list the server wrote rather than
 * a second implementation of the grammar. The empty string is no upstreams,
 * which is a configuration and not a gap — see Zone.forward_to.
 */
export function forwardTargets(forwardTo: string): string[] {
  return forwardTo
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry !== "");
}
