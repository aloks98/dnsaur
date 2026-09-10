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
import {
  useBlockingStatus,
  usePauseBlocking,
  useResumeBlocking,
  type BlockingStatus,
} from "../hooks/use-blocking";
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
  /** A client row's alternative to `groupId`: this one device, not its
   * group. Never both — the server refuses a pause that names two scopes. */
  clientId?: number;
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
 * action, rather than a status dot sitting next to a separate button. It
 * sits directly beside the app's only other status cell (resolver health —
 * see top-nav.tsx); a second identical-looking indicator for a different
 * thing next to it read as duplication.
 *
 * The wording is about *blocking* specifically, never a bare "Pause":
 * dnsaur also serves local DNS and will grow zones, so "paused" on its own
 * is genuinely ambiguous about what stopped.
 *
 *     [⏸ BLOCKING ACTIVE ▾]  [▶ PAUSED · 4:32 ▾]  [⚠ STATUS UNAVAILABLE ▾]
 *        filled, primary          filled, warning        flat, destructive
 *
 * In the chrome the two states we've actually read from GET /blocking fill
 * the cell; the two we haven't (checking, unreachable) stay flat text. That
 * asymmetry is the point — see `triggerView` below.
 *
 * Uppercase is CSS, not the strings: `text-transform` keeps the accessible
 * name and the DOM text as sentence case, which is what screen readers and
 * assertions both want, while the chrome still reads as chrome.
 *
 * GET /blocking polls every 30s (see use-blocking.ts); between polls, a
 * local 1s ticker keeps the countdown itself smooth without hammering the
 * network — it only runs while actually paused.
 */
export function PauseControl({ groupId = 0, clientId = 0, variant = "button" }: PauseControlProps) {
  const status = useBlockingStatus({ groupId, clientId });
  const pauseBlocking = usePauseBlocking();
  const resumeBlocking = useResumeBlocking();

  const pausedUntil = status.data?.paused_until ?? 0;

  // The clock is state, never a Date.now() read while rendering: render has
  // to be pure, and one that reads the clock can print two different seconds
  // for the same frame.
  const [now, setNow] = useState(() => Date.now());
  // …floored at the moment this answer arrived. Between pauses no ticker
  // runs, so `now` is whatever the last one left behind — which for a pause
  // that starts minutes later would show a countdown minutes too long until
  // the first tick corrected it. dataUpdatedAt is when the server said this,
  // so the pause is measured from there and the ticker takes over from the
  // second onwards. It used to be an effect that set the clock the instant
  // it saw a pause, which is a render the state was already able to describe.
  const asOf = Math.max(now, status.dataUpdatedAt);
  const isPaused = pausedUntil > asOf;

  useEffect(() => {
    if (!isPaused) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [isPaused]);

  const remainingMs = Math.max(0, pausedUntil - asOf);
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
  // Having read the state once is what counts, not whether the *latest* poll
  // succeeded: a 30s poll that blips must not un-grey Resume under a pause
  // this control already knows about.
  const stateKnown = status.data !== undefined;
  // Resume only ever clears this control's own scope. When the countdown
  // belongs to a wider one — a client showing its group's pause, a group
  // showing the global one — there is nothing here to resume, so the menu
  // says where the pause is from and greys the action out rather than
  // offering one that would change nothing. A pause reported without a
  // scope is not treated as inherited: greying out the one action this
  // control has, on a half-understood answer, is the worse guess.
  const ownScope = clientId ? "client" : groupId ? "group" : "global";
  const inherited = isPaused && status.data?.scope !== undefined && status.data.scope !== ownScope;

  function onPause(minutes: number) {
    pauseBlocking.mutate(
      { groupId, clientId, minutes },
      {
        onSuccess: () =>
          toast.success(`Blocking paused for ${minutes} minute${minutes === 1 ? "" : "s"}`),
        onError: () => toast.error("Couldn't pause blocking — try again"),
      },
    );
  }

  function onResume() {
    resumeBlocking.mutate(
      { groupId, clientId },
      {
        onSuccess: () => toast.success("Blocking resumed"),
        onError: () => toast.error("Couldn't resume blocking — try again"),
      },
    );
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
            {menuLabel(isPaused, inherited, status.data?.scope)}
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
        <DropdownMenuItem
          disabled={busy || (stateKnown && (!isPaused || inherited))}
          onClick={onResume}
        >
          <Play />
          Resume blocking
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The menu's heading: what is going on, and — when the pause came from a
 * wider scope than this control covers — where it came from, which is the
 * only place that fact has to be said. */
function menuLabel(isPaused: boolean, inherited: boolean, scope: BlockingStatus["scope"]) {
  if (!isPaused) return "Pause blocking";
  if (!inherited) return "Blocking is paused";
  return scope === "global" ? "Paused for everyone" : "Paused for this group";
}

/**
 * The top bar's row-1 cell skin: square edges, one hairline on the left,
 * uppercase mono at `text-xs`/`tracking-wider` — the same pair the resolver
 * readout beside it uses, one notch tighter than the nav cells' tracking so
 * a whole phrase ("Status unavailable") still fits the row. Not a Button:
 * the chrome is a strip of divided cells, and a rounded outlined control
 * dropped into it looks like something that fell in from another screen.
 *
 * The surface is left to `chromeTone`, because the *known* states fill this
 * cell (see below) and the unknown ones deliberately don't.
 */
const CHROME_CELL =
  "flex h-full shrink-0 items-center gap-1.5 whitespace-nowrap border-l border-border " +
  "px-4 font-mono text-xs font-medium tracking-wider uppercase transition-colors " +
  "disabled:opacity-60 " +
  "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

interface TriggerView {
  icon: LucideIcon;
  label: string;
  /** Extra classes for the `"button"` variant — amber while paused, since a
   * pause is temporary and about to end (the tone the old StatusIndicator's
   * "fixing" carried). */
  buttonTone?: string;
  /**
   * The same four states in the chrome cell's vocabulary. The two states we
   * have actually read fill the cell; the two we haven't don't — which is
   * what keeps "I couldn't reach GET /blocking" from ever looking like the
   * confident, filled "BLOCKING ACTIVE".
   */
  chromeTone: string;
}

/**
 * The paused fill's ink.
 *
 * The design asks for `var(--on-warning)`, which doesn't exist. rnui's
 * nearest token is `--warning-foreground`, but that is amber *text for a
 * normal surface*, not ink for an amber fill: on `--warning` it measures
 * 2.10:1 in light and 1.41:1 in dark. `--warning-solid-foreground` is the
 * app token for exactly this — a near-black that clears AA on the solid
 * amber in both modes (see styles/dnsaur-theme.css) — and it's the same
 * ink the query log's RESUME TAIL cell wears, so the two amber fills in
 * the shell agree.
 */
const ON_WARNING = "text-warning-solid-foreground";

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
      // Unfilled: there is nothing to be confident about yet.
      chromeTone: "text-muted-foreground hover:bg-accent/60",
    };
  // Only when there is nothing to show. A failed *refetch* still has the
  // last good `paused_until`, and replacing a live countdown with a
  // destructive "Status unavailable" over one missed poll is the failure
  // mode the data layer's rule exists to prevent (web/README.md).
  if (status.isError && status.data === undefined)
    return {
      icon: CircleAlert,
      label: "Status unavailable",
      buttonTone: "text-muted-foreground",
      // Loud enough not to be skimmed past in a one-row bar, and — by not
      // being a fill at all — impossible to mistake for the filled cell
      // that means "blocking is on".
      chromeTone: "text-destructive hover:bg-accent/60",
    };
  if (isPaused)
    return {
      icon: Play,
      label: `Paused · ${formatCountdown(remainingMs)}`,
      buttonTone:
        "border-warning/40 bg-warning/10 text-warning-foreground hover:bg-warning/20 dark:bg-warning/20",
      chromeTone: `bg-warning ${ON_WARNING} hover:bg-warning/90`,
    };
  return {
    icon: Pause,
    label: "Blocking active",
    chromeTone: "bg-primary text-primary-foreground hover:bg-primary/90",
  };
}
