import { useEffect, useMemo, useRef, useState } from "react";
import { Outlet, useLocation } from "react-router";
import { useResolverStatus } from "../hooks/use-settings";
import { statusFacts } from "../lib/serving";
import { CommandPalette } from "./command-palette";
import { ErrorBoundary } from "./error-boundary";
import { TopNav } from "./top-nav";

// Toaster lives at the App level (not here) — unauthenticated screens like
// Setup and Login need toasts too, and mounting it here as well as there
// would double every toast.
export function AppShell() {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const { pathname } = useLocation();

  return (
    // h-screen, not min-h-screen: the full-bleed pages size a table to
    // "whatever is left of the window", and flex-1 can only resolve that
    // against a parent with a definite height. With min-h-screen the chain
    // is open-ended, which is why the query log had to hardcode a scroll
    // height and then stopped short of the bottom of the page.
    <div className="flex h-screen flex-col overflow-hidden bg-background">
      <TopNav onOpenCommandPalette={() => setPaletteOpen(true)} />
      {/* Facts about the running server, not about any one screen, used to
          stack here as a warning bar each: three of them ate the top of
          every page and the operator stopped reading them. They are now one
          cell in the top bar and a panel under it (top-nav.tsx's
          StatusCell). What stays in the shell is the part that has nowhere
          to be drawn — the announcement. */}
      <StatusAnnouncer />
      {/* No gutter, and no scrolling here. Every screen is a full-bleed
          grid of hairline-separated bands whose rules have to meet the
          viewport edges rather than float inside a 24px frame, and each
          owns its own scrolling — the thing that should scroll is one pane,
          not the chrome.
          This used to branch per route while the screens were rebuilt one
          at a time; once Account was the last one left, the branch always
          took the same side.
          `flex flex-col` is what lets a page claim the remaining height, and
          min-h-0 lets it actually shrink — without it a long table forces
          <main> past the viewport and the internal scroller never engages. */}
      <main className="flex min-h-0 flex-1 flex-col overflow-hidden">
        {/* Keyed on the path: this shell is the persistent layout element,
            so one instance of the boundary outlives every sibling route
            change. Without the key, a page that throws once leaves
            "Something went wrong" pinned in the content area while the top
            nav happily changes the URL underneath it — every link looks
            broken until a reload. */}
        <ErrorBoundary key={pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>
      {/* Always mounted, open or not: it owns the ⌘K / Ctrl+K listener. */}
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </div>
  );
}

/**
 * How a screen reader learns that something went wrong, now that the facts
 * live behind a cell nobody is looking at.
 *
 * Every stacked bar used to be `role="alert"`, which announced all nine of
 * them assertively and interrupted whatever was being read. Two regions
 * instead: arriving amber facts are polite — they are worth knowing and
 * worth finishing your sentence first — and the one red fact (every query
 * going out in the clear) keeps `role="alert"`, because it is the fact that
 * earns an interruption.
 *
 * Only *arrivals*. A fact clearing announces nothing: nobody needs telling
 * that a thing they may never have heard about has stopped being true.
 * Everything already true when the page loads counts as one arrival — that
 * is the load rather than a notification, and it is the only moment a
 * reader who cannot see the cell would otherwise never learn there is
 * anything in it.
 *
 * In the shell rather than beside the cell, so it is mounted exactly once
 * and stays mounted while the routed page under it changes. It reads the
 * same query the cell does — react-query dedupes, so this is a
 * subscription, not a second request — rather than sharing state with it:
 * the cell's own bookkeeping (first-seen order, the NEW tag) is about the
 * panel, and this is about the announcement.
 */
function StatusAnnouncer() {
  const status = useResolverStatus();
  // Memoised on the query's own data so the array's identity moves only
  // when the server's answer does: the effect below keys off it, and a new
  // array every render would re-run on every unrelated render in the shell.
  const facts = useMemo(() => statusFacts(status.data), [status.data]);
  const announced = useRef(new Set<string>());
  const [polite, setPolite] = useState(NOTHING_SAID);
  const [urgent, setUrgent] = useState(NOTHING_SAID);

  useEffect(() => {
    const known = announced.current;
    announced.current = new Set(facts.map((fact) => fact.id));
    const arrived = facts.filter((fact) => !known.has(fact.id));
    setPolite((said) => spoken(said, say(arrived, "amber"), holds(facts, "amber")));
    setUrgent((said) => spoken(said, say(arrived, "red"), holds(facts, "red")));
  }, [facts]);

  return (
    <>
      {/* Mounted whether or not it has anything to say: a polite region has
          to exist before its content changes, or the first change is the
          region appearing and nothing is read. The text hangs off a keyed
          node *inside* it — see `spoken` for why the region itself cannot
          be the thing that gets replaced. */}
      <span className="sr-only" aria-live="polite" aria-atomic="true">
        <span key={polite.say}>{polite.text}</span>
      </span>
      {/* The assertive one is mounted only while it has something to say —
          appearing with content is how an alert announces, and an empty
          `role="alert"` sitting in the tree on every clear screen is a
          landmark that reports nothing. */}
      {urgent.text !== "" && (
        <span key={urgent.say} className="sr-only" role="alert">
          {urgent.text}
        </span>
      )}
    </>
  );
}

/** What a region is currently saying, and how many times it has been asked
 * to say something. */
interface Said {
  text: string;
  say: number;
}

const NOTHING_SAID: Said = { text: "", say: 0 };

/**
 * What a region should hold next: what has just arrived, nothing at all
 * once the facts of that tone have cleared, or what it already had.
 *
 * `say` counts announcements rather than describing one. It is the key of
 * the node holding the text, because the same fault can clear and come back
 * with the same words — and re-rendering an identical string changes no
 * DOM, so a screen reader has nothing to notice and the second arrival goes
 * unannounced. A new key replaces the node instead. On the polite region
 * the key is on a *child*, not on the region: a live region that is itself
 * replaced is a new region appearing with content already in it, which is
 * the one shape that reliably announces nothing.
 *
 * Emptying is not an announcement — a region that goes blank reads nothing
 * — but it has to happen, or a fault that ended stays in the accessible
 * tree for anyone who navigates into it.
 */
function spoken(said: Said, arrived: string, stillTrue: boolean): Said {
  if (arrived !== "") return { text: arrived, say: said.say + 1 };
  if (!stillTrue && said.text !== "") return { text: "", say: said.say + 1 };
  return said;
}

/** The facts of one tone that arrived together, as one sentence — several
 * can land in a single poll, and one region cannot say them in turn. */
function say(arrived: { tone: string; text: string }[], tone: string): string {
  return arrived
    .filter((fact) => fact.tone === tone)
    .map((fact) => fact.text)
    .join(". ");
}

/** Whether anything of this tone is still true, which is what decides
 * whether its region still has anything to hold. */
function holds(facts: { tone: string }[], tone: string): boolean {
  return facts.some((fact) => fact.tone === tone);
}
