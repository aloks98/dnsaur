/**
 * The one line a synced screen carries on a replica (spec §7): whose
 * configuration this is.
 *
 * It sits in the section header, immediately left of the disabled action it
 * explains, and it states the fact and stops. *Why* the controls beside it
 * are dead, and how to take them back, is docs/dashboard.md's job — putting
 * that sentence on every one of six screens would be six copies of an
 * explanation an operator reads once.
 *
 * Muted text, not a warning strip. Being a replica is the configuration
 * working as intended; the page-wide strip is kept for the two sync facts
 * that are actually wrong (lib/serving.ts's syncBanners), and a screen that
 * warned about the normal case would teach an operator to skip it.
 *
 * `<output>` rather than a `role="alert"` banner: this is standing state for
 * as long as the instance follows a peer, not an event to interrupt for. It
 * names no peer — the top bar's role chip carries which main, once, for the
 * whole app.
 */
export function ManagedNotice() {
  return (
    <output className="text-[11.5px] leading-none text-muted-foreground">
      Managed by the main
    </output>
  );
}
