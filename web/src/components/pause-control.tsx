import { useEffect, useState } from "react";
import type { LucideIcon } from "lucide-react";
import { ChevronDown, CircleAlert, Pause, Play, Timer } from "lucide-react";
import { toast } from "sonner";
import {
  Button,
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
  /** 0 = global (every group) — the shell Header's instance. Task 10 passes
   * a real group id to reuse this same control per-group. */
  groupId?: number;
}

/**
 * Blocking pause/resume: one button that *is* the state readout and the
 * action, rather than a status dot sitting next to a separate button. The
 * sidebar footer owns the app's only status dot (resolver health — see
 * sidebar-nav.tsx); a second identical-looking dot for a different thing
 * next to it read as duplication.
 *
 * The wording is about *blocking* specifically, never a bare "Pause":
 * dnsaur also serves local DNS and will grow zones, so "paused" on its own
 * is genuinely ambiguous about what stopped.
 *
 *     [⏸ Blocking active ▾]   [▶ Paused · 4:32 ▾]   [⚠ Status unavailable ▾]
 *
 * GET /blocking polls every 30s (see use-blocking.ts); between polls, a
 * local 1s ticker keeps the countdown itself smooth without hammering the
 * network — it only runs while actually paused.
 */
export function PauseControl({ groupId = 0 }: PauseControlProps) {
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
  // confident "Blocking active" — that's the one state this button can't
  // honestly claim without having read it. It stays operable either way:
  // pause/resume are still worth attempting, and the mutations carry their
  // own error toasts.
  const { icon: StateIcon, label, tone } = triggerView(status, isPaused, remainingMs);
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
        render={
          <Button type="button" variant="outline" size="sm" disabled={busy} className={tone} />
        }
      >
        <StateIcon />
        <span className="tabular-nums">{label}</span>
        {/* The visible text is the state; the accessible name still has to
            say what activating the control does. */}
        <span className="sr-only">, open blocking controls</span>
        <ChevronDown className="opacity-60" />
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

interface TriggerView {
  icon: LucideIcon;
  label: string;
  /** Extra button classes — amber while paused, since a pause is temporary
   * and about to end (the tone the old StatusIndicator's "fixing" carried). */
  tone?: string;
}

function triggerView(
  status: ReturnType<typeof useBlockingStatus>,
  isPaused: boolean,
  remainingMs: number,
): TriggerView {
  if (status.isPending) return { icon: Pause, label: "Checking…", tone: "text-muted-foreground" };
  if (status.isError)
    return { icon: CircleAlert, label: "Status unavailable", tone: "text-muted-foreground" };
  if (isPaused)
    return {
      icon: Play,
      label: `Paused · ${formatCountdown(remainingMs)}`,
      tone: "border-warning/40 bg-warning/10 text-warning-foreground hover:bg-warning/20 dark:bg-warning/20",
    };
  return { icon: Pause, label: "Blocking active" };
}
