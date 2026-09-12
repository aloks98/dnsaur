import { WarningStrip } from "./warning-strip";

/**
 * The one line at the top of a synced screen on a replica (spec §7): whose
 * configuration this is.
 *
 * It states the fact and stops. *Why* the controls beneath it are dead, and
 * how to take them back, is docs/dashboard.md's job — putting that sentence
 * on every one of six screens would be six copies of an explanation an
 * operator reads once.
 *
 * `<output>` rather than a `role="alert"` banner: this is standing state for
 * as long as the instance follows a peer, not an event to interrupt for.
 * The shell's sync banners (serving-banners.tsx) are the ones that alert,
 * and only when something is actually wrong.
 */
export function ManagedNotice({ peer }: { peer: string }) {
  return (
    <WarningStrip
      as="output"
      size="roomy"
      className="block shrink-0 border-b border-border font-mono text-[12.5px] text-warning-foreground"
    >
      Managed by {peer}
    </WarningStrip>
  );
}
