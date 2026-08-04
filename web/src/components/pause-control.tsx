import { useEffect, useState } from "react";
import { Pause, Play, Timer } from "lucide-react";
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
  StatusIndicator,
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
 * Blocking pause/resume. Follows the app's existing split between *state*
 * and *action* rather than baking both into one button: a StatusIndicator
 * dot (the same primitive the sidebar's resolver status and the query
 * log's live/paused readout use — see sidebar-nav.tsx, pages/queries.tsx)
 * carries the live state, amber/pulsing ("fixing") while paused since a
 * pause is inherently temporary and about to end, next to a small,
 * always-labeled "Pause" menu button — the same pattern as the query log's
 * "Filter" button, whose label never changes even when filters are active.
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
    <div className="flex items-center gap-2">
      <StatusIndicator
        state={isPaused ? "fixing" : "active"}
        label={isPaused ? `Paused · ${formatCountdown(remainingMs)}` : "Blocking active"}
        size="sm"
        labelClassName="tabular-nums"
      />
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={busy}
              aria-label="Pause blocking"
            />
          }
        >
          <Pause />
          Pause
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
            <DropdownMenuItem
              key={opt.minutes}
              disabled={busy}
              onClick={() => onPause(opt.minutes)}
            >
              <Timer />
              {opt.label}
            </DropdownMenuItem>
          ))}
          <DropdownMenuSeparator />
          <DropdownMenuItem disabled={busy || !isPaused} onClick={onResume}>
            <Play />
            Resume
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}
