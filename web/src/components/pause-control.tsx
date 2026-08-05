import { useEffect, useState } from "react";
import type { LucideIcon } from "lucide-react";
import { ChevronDown, CircleAlert, Pause, Play, Timer } from "lucide-react";
import { toast } from "sonner";
import {
  Button,
  cn,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@e412/rnui-react";
import { useBlockingStatus, usePauseBlocking, useResumeBlocking } from "../hooks/use-blocking";
import { formatCountdown } from "../lib/format";

const PAUSE_OPTIONS = [
  { minutes: 5, label: "Pause 5 minutes" },
  { minutes: 30, label: "Pause 30 minutes" },
  { minutes: 60, label: "Pause 60 minutes" },
] as const;

interface PauseControlProps {
  /** 0 = global (every group) — the shell top bar's instance. The Filtering
   * page's group rows pass a real group id to reuse this same control. */
  groupId?: number;
  /**
   * `"chrome"` is the top bar's row-1 cell: a square-edged, hairline-divided
   * strip of uppercase mono, not a button with a border and a radius.
   * `"button"` (the default) is what the per-group rows on Filtering use,
   * where it sits among other buttons inside a card.
   */
  variant?: "button" | "chrome";
}

/**
 * Blocking pause/resume: one control that *is* the state readout and the
 * action, rather than a status dot sitting next to a separate button. The
 * shell's second row owns the app's only other status cell (resolver health
 * — see top-nav.tsx); a second identical-looking indicator for a different
 * thing next to it read as duplication.
 *
 * The wording is about *blocking* specifically, never a bare "Pause":
 * dnsaur also serves local DNS and will grow zones, so "paused" on its own
 * is genuinely ambiguous about what stopped.
 *
 *     [⏸ BLOCKING ACTIVE ▾]  [▶ PAUSED · 4:32 ▾]  [⚠ STATUS UNAVAILABLE ▾]
 *
 * Uppercase is CSS, not the strings: `text-transform` keeps the accessible
 * name and the DOM text as sentence case, which is what screen readers and
 * assertions both want, while the chrome still reads as chrome.
 *
 * GET /blocking polls every 30s (see use-blocking.ts); between polls, a
 * local 1s ticker keeps the countdown itself smooth without hammering the
 * network — it only runs while actually paused.
 */
export function PauseControl({ groupId = 0, variant = "button" }: PauseControlProps) {
  const status = useBlockingStatus(groupId);
  const pauseBlocking = usePauseBlocking();
  const resumeBlocking = useResumeBlocking();

  const pausedUntil = status.data?.paused_until ?? 0;
  const isPaused = pausedUntil > Date.now();

  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!isPaused) return;
    // Sync immediately (not just on the first tick) so the countdown is
    // accurate the instant a pause takes effect, not up to a second stale.
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [isPaused]);

  const remainingMs = Math.max(0, pausedUntil - now);
  const busy = pauseBlocking.isPending || resumeBlocking.isPending;

  // A failed or not-yet-answered GET /blocking must not render as a
  // confident "Blocking active" — that's the one state this control can't
  // honestly claim without having read it. It stays operable either way:
  // pause/resume are still worth attempting, and the mutations carry their
  // own error toasts.
  const {
    icon: StateIcon,
    label,
    buttonTone,
    chromeTone,
  } = triggerView(status, isPaused, remainingMs);
  const stateKnown = status.isSuccess;

  function onPause(minutes: number) {
    pauseBlocking.mutate(
      { groupId, minutes },
      {
        onSuccess: () =>
          toast.success(`Blocking paused for ${minutes} minute${minutes === 1 ? "" : "s"}`),
        onError: () => toast.error("Couldn't pause blocking — try again"),
      },
    );
  }

  function onResume() {
    resumeBlocking.mutate(groupId, {
      onSuccess: () => toast.success("Blocking resumed"),
      onError: () => toast.error("Couldn't resume blocking — try again"),
    });
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={busy}
        className={variant === "chrome" ? cn(CHROME_CELL, chromeTone) : undefined}
        render={
          variant === "chrome" ? undefined : (
            <Button type="button" variant="outline" size="sm" className={buttonTone} />
          )
        }
      >
        <StateIcon className={variant === "chrome" ? "size-3" : undefined} />
        <span className="tabular-nums">{label}</span>
        {/* The visible text is the state; the accessible name still has to
            say what activating the control does. */}
        <span className="sr-only">, open blocking controls</span>
        <ChevronDown className={variant === "chrome" ? "size-3 opacity-60" : "opacity-60"} />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {/* Menu.GroupLabel (rnui's DropdownMenuLabel) requires a
            Menu.Group ancestor even for a single ungrouped label. */}
        <DropdownMenuGroup>
          <DropdownMenuLabel>
            {isPaused ? "Blocking is paused" : "Pause blocking"}
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        {PAUSE_OPTIONS.map((opt) => (
          <DropdownMenuItem key={opt.minutes} disabled={busy} onClick={() => onPause(opt.minutes)}>
            <Timer />
            {opt.label}
          </DropdownMenuItem>
        ))}
        <DropdownMenuSeparator />
        {/* Only greyed out when we *know* there is nothing to resume. */}
        <DropdownMenuItem disabled={busy || (stateKnown && !isPaused)} onClick={onResume}>
          <Play />
          Resume blocking
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * The top bar's row-1 cell skin: square edges, one hairline on the left,
 * uppercase mono at the design's 11px/500/.06em. Not a Button — the chrome
 * is a strip of divided cells, and a rounded outlined control dropped into
 * it looks like something that fell in from another screen.
 */
const CHROME_CELL =
  "flex h-full shrink-0 items-center gap-1.5 whitespace-nowrap border-l border-border " +
  "px-[15px] font-mono text-[11px] font-medium tracking-[0.06em] uppercase transition-colors " +
  "hover:bg-accent/60 disabled:opacity-60 " +
  "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

interface TriggerView {
  icon: LucideIcon;
  label: string;
  /** Extra classes for the `"button"` variant — amber while paused, since a
   * pause is temporary and about to end (the tone the old StatusIndicator's
   * "fixing" carried). */
  buttonTone?: string;
  /** The same four states in the chrome cell's flat, text-only vocabulary. */
  chromeTone: string;
}

function triggerView(
  status: ReturnType<typeof useBlockingStatus>,
  isPaused: boolean,
  remainingMs: number,
): TriggerView {
  if (status.isPending)
    return {
      icon: Pause,
      label: "Checking…",
      buttonTone: "text-muted-foreground",
      chromeTone: "text-muted-foreground",
    };
  if (status.isError)
    return {
      icon: CircleAlert,
      label: "Status unavailable",
      buttonTone: "text-muted-foreground",
      // Loud enough not to be skimmed past in a 42px bar, and impossible to
      // read as a confident "blocking is on".
      chromeTone: "text-destructive",
    };
  if (isPaused)
    return {
      icon: Play,
      label: `Paused · ${formatCountdown(remainingMs)}`,
      buttonTone:
        "border-warning/40 bg-warning/10 text-warning-foreground hover:bg-warning/20 dark:bg-warning/20",
      chromeTone: "bg-warning/10 text-warning-foreground dark:bg-warning/20",
    };
  return { icon: Pause, label: "Blocking active", chromeTone: "text-primary" };
}
