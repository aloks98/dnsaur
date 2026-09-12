import { useRef, useState } from "react";
import { Download, TriangleAlert, Upload, X } from "lucide-react";
import { toast } from "sonner";
import {
  Button,
  cn,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "@e412/rnui-react";
import type { Zone } from "../../api/types";
import {
  useExportZoneFile,
  useImportZoneFile,
  zoneFileErrors,
  type ZoneFileDiff,
  type ZoneRecordChange,
} from "../../hooks/use-zones";
import { useManagedBy } from "../../hooks/use-sync";
import { formatBytes } from "../../lib/format";

/** The most rows any one diff group lists before the rest collapse into a
 * "+ N more" line — the artboard's own truncation. A whole-zone replace can
 * delete hundreds of records, and a dialog that scrolls for a minute to show
 * them makes the counts strip above it the only thing anyone actually reads.
 * The point of the list is to recognise *what kind* of thing is going, which
 * a handful of examples does. */
const MAX_DIFF_ROWS = 5;

/** What the dialog's header band needs to name the file, known synchronously
 * from the File itself — before its bytes have been read, which is what lets
 * the dialog open the instant a file is chosen. */
interface FileMeta {
  name: string;
  size: number;
}

/** The file the admin picked, held as text because that's what gets posted:
 * the endpoint takes the master file in a JSON field, not a multipart upload,
 * so the bytes are read once here and sent twice — dry run, then commit. */
interface ChosenFile extends FileMeta {
  content: string;
}

/**
 * Idle is "no dialog"; choosing a file is what opens it.
 *
 * `checking` exists because the dry run is not instant — the server re-parses
 * and re-diffs the entire file, seconds of work on a large zone. Waiting for
 * it before opening anything left the page looking frozen for that whole
 * time. Only `diff` carries the file's content, because Apply is the one
 * thing that needs to send the bytes again.
 *
 * Every request-bearing phase carries the `token` of the request it is
 * waiting on. Closing the dialog is not a cancel — nothing here can recall a
 * request the server is already parsing — so a result can still arrive for a
 * dialog nobody has open, and applying it would reopen the dialog on its own
 * seconds after Cancel. The token is what a landing result is checked
 * against; a stale one is dropped.
 */
type ImportPhase =
  | { kind: "idle" }
  | { kind: "checking"; file: FileMeta; token: number }
  | { kind: "diff"; file: ChosenFile; diff: ZoneFileDiff }
  | { kind: "rejected"; file: FileMeta; errors: string[] };

/** Per-bucket presentation. Spelled out per tone rather than composed from a
 * token name because Tailwind resolves class names statically — a
 * `text-${tone}-foreground` would compile to nothing. */
const DIFF_TONES = {
  add: {
    label: "Added",
    sign: "+",
    text: "text-success-foreground",
    mark: "shadow-[inset_3px_0_0_var(--success)]",
  },
  change: {
    label: "Changed",
    sign: "~",
    text: "text-warning-foreground",
    mark: "shadow-[inset_3px_0_0_var(--warning)]",
  },
  delete: {
    label: "Deleted",
    // U+2212 MINUS SIGN, not a hyphen: it sits at the same width and height
    // as the "+" above it, which a hyphen does not, and these three counts
    // are read as a column.
    sign: "−",
    text: "text-destructive-foreground",
    mark: "shadow-[inset_3px_0_0_var(--destructive)]",
  },
} as const;

/**
 * What actually moved on a changed record.
 *
 * The server pairs `from` and `to` by name, type *and* rdata together, so
 * those three are equal by construction and rendering "rdata → rdata" would
 * print the same value twice on every row. What a change can carry is a new
 * TTL or a new enabled state, so that is what gets shown.
 */
function changeTransition(change: ZoneRecordChange): string {
  const moved: string[] = [];
  if (change.from.ttl !== change.to.ttl) moved.push(`ttl ${change.from.ttl} → ${change.to.ttl}`);
  if (change.from.enabled !== change.to.enabled) {
    moved.push(change.to.enabled ? "disabled → enabled" : "enabled → disabled");
  }
  return moved.join(" · ");
}

/** One line of the diff: the record, plus what moved if anything did. */
interface DiffRow {
  key: string;
  name: string;
  type: string;
  rdata: string;
  transition: string;
}

function DiffGroup({
  tone,
  rows,
  total,
}: {
  tone: keyof typeof DIFF_TONES;
  rows: DiffRow[];
  total: number;
}) {
  if (total === 0) return null;
  const { label, sign, text, mark } = DIFF_TONES[tone];
  const hidden = total - rows.length;
  return (
    <div>
      <div
        className={cn(
          "sticky top-0 flex items-center gap-2.5 border-b border-border-muted bg-muted px-4 py-1.5",
          mark,
        )}
      >
        <span
          className={cn("font-mono text-[9.5px] font-semibold tracking-[0.14em] uppercase", text)}
        >
          {label}
        </span>
        <span className="font-mono text-[11px] text-muted-foreground">
          {total} {total === 1 ? "record" : "records"}
        </span>
      </div>
      {rows.map((row) => (
        <div
          key={row.key}
          className="grid grid-cols-[16px_196px_64px_1fr] items-baseline gap-3 border-b border-border-muted px-4 py-[5px]"
        >
          <span className={cn("font-mono text-[12.5px] font-semibold", text)}>{sign}</span>
          <span className="truncate font-mono text-[12.5px]" title={row.name}>
            {row.name}
          </span>
          <span className="font-mono text-[11.5px] text-muted-foreground">{row.type}</span>
          <span className="truncate font-mono text-[12.5px]" title={row.rdata}>
            <span className="text-muted-foreground">{row.rdata}</span>
            {row.transition && <span className="text-foreground"> {row.transition}</span>}
          </span>
        </div>
      ))}
      {hidden > 0 && (
        <div className="border-b border-border-muted py-1.5 pr-4 pl-11 font-mono text-[11px] text-muted-foreground">
          + {hidden} more
        </div>
      )}
    </div>
  );
}

/**
 * The import dialog's footer, shared by the dry run's wait and the diff.
 *
 * One component rather than two copies so the two states cannot drift apart
 * — they are meant to be the same bar with a different label on the primary
 * action, which is also what stops anything shifting under the pointer when
 * the diff arrives. The warning is stated in both: it is equally true of the
 * file being checked as of the diff already on screen.
 *
 * `onApply` is optional because the pending state has nothing to apply yet;
 * that button is disabled there, so it can carry no handler at all.
 */
function ImportFooter({
  applyLabel,
  applyDisabled,
  onApply,
  onCancel,
}: {
  applyLabel: string;
  applyDisabled: boolean;
  onApply?: () => void;
  onCancel: () => void;
}) {
  return (
    <div className="flex shrink-0 items-center gap-3 border-t border-border px-4 py-3">
      <span className="flex items-center gap-1.5 text-[12.5px] text-destructive-foreground">
        <TriangleAlert className="size-3.5 shrink-0" aria-hidden="true" />
        Anything not in the file is deleted.
      </span>
      <div className="ml-auto flex items-center gap-2">
        <Button type="button" size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          onClick={onApply}
          disabled={applyDisabled}
        >
          {applyLabel}
        </Button>
      </div>
    </div>
  );
}

/**
 * Export and import for one zone: the two header buttons, the file input
 * behind Import, and the dialog that makes a destructive replace legible
 * before it happens.
 *
 * Import is a whole-zone replace — anything the zone has that the file
 * doesn't is deleted — so choosing a file never writes. It posts
 * `dry_run: true`, shows the diff that comes back, and only the explicit
 * Apply posts again with `dry_run: false`. The second post sends the same
 * bytes rather than a diff to replay, because the endpoint has no handle for
 * one: the server re-reads and re-diffs the file at apply time. A zone that
 * changed in between therefore applies against its current state, not the
 * state that was previewed — the preview is advisory, which is inherent to
 * the shipped API rather than a choice made here.
 *
 * `zone` rather than an id alone because the export filename falls back to
 * the zone's own name when a response arrives without a Content-Disposition.
 */
export function ZoneFileActions({ zone }: { zone: Zone }) {
  const [phase, setPhase] = useState<ImportPhase>({ kind: "idle" });
  const fileInput = useRef<HTMLInputElement>(null);
  const exportFile = useExportZoneFile();
  const importFile = useImportZoneFile();
  const managedBy = useManagedBy();
  /**
   * Which request the dialog is currently interested in. Bumped when one is
   * started and again when the dialog closes, so a result that lands after
   * Cancel finds its token superseded and returns without touching the
   * phase — see ImportPhase. A ref rather than state because the mutation
   * callbacks below close over it, and what they need is the value at the
   * moment they run, not the one their render captured.
   */
  const requestToken = useRef(0);

  function closeDialog() {
    // Nothing recalls a request already on the wire, so the outstanding one
    // is disowned rather than cancelled.
    requestToken.current += 1;
    setPhase({ kind: "idle" });
  }

  /**
   * A primary is the only zone whose records are authored here, so it is the
   * only one an import means anything for. Gated on that *positive* set
   * rather than on the set the server 409s, which keeps the two independent:
   * recordWriteRefusal covers `internal`, `secondary` and `stub`, so every
   * type this hides the control from is also refused server-side, and the
   * gate stays right if that set changes again.
   *
   * The stub case was added late (it had been accepted, then thrown away by
   * the next fetch's whole-set replace — and worse, read back by
   * StubUpstreams into the routing table in between). Do not infer from that
   * history that this gate is load-bearing for correctness: it is not, and
   * must not become the only thing standing between a write and the store.
   *
   * Export is offered for a stub, a secondary and a built-in alike: reading
   * is never refused. The exception is a forwarder, which holds nothing to
   * render into a file — see the header's own gate.
   */
  // Import rewrites the whole record set, so on a replica it is the main's
  // — while export stays offered, because reading is never refused.
  const canImport = zone.type === "primary" && managedBy === "";

  // A dry run or a commit is actually on the wire. This is the window in
  // which a second file selection would buy a second full parse-and-diff of
  // the whole file — the most expensive request this page makes.
  //
  // Deliberately not `phase.kind !== "idle"`: the rejected state has to keep
  // accepting a file, because "Choose file" is how that state is escaped.
  const requestInFlight = phase.kind === "checking" || importFile.isPending;

  function onExport() {
    exportFile.mutate(
      { id: zone.id, name: zone.name },
      { onError: () => toast.error(`Couldn't export ${zone.name}`) },
    );
  }

  function onFileChosen(event: React.ChangeEvent<HTMLInputElement>) {
    // The guard belongs here, at the point the work is actually started, not
    // only on the Import button: the button is the visible affordance, but
    // the input is what fires the request, and it can be reached without the
    // button — programmatically, or if the dialog's focus handling ever
    // changes. The `disabled` attribute below is the affordance; this is the
    // backstop that holds when something dispatches the event anyway.
    if (requestInFlight) return;

    const picked = event.target.files?.[0];
    // Re-choosing the same file has to re-fire this, and a file input whose
    // value still holds that path won't emit `change` again. Cleared here,
    // before any await, so it happens whether or not the read succeeds.
    event.target.value = "";
    if (!picked) return;

    // Opened here, before the read and before the request, so the wait is
    // visible for all of it. Name and size come off the File itself, so the
    // header band is complete from the first frame.
    const meta: FileMeta = { name: picked.name, size: picked.size };
    const token = ++requestToken.current;
    setPhase({ kind: "checking", file: meta, token });

    void picked.text().then(
      (content) => {
        if (requestToken.current !== token) return;
        importFile.mutate(
          { id: zone.id, content, dryRun: true },
          {
            onSuccess: (diff) => {
              if (requestToken.current !== token) return;
              setPhase({ kind: "diff", file: { ...meta, content }, diff });
            },
            onError: (err) => {
              if (requestToken.current !== token) return;
              setPhase({ kind: "rejected", file: meta, errors: zoneFileErrors(err) });
            },
          },
        );
      },
      () => {
        if (requestToken.current !== token) return;
        setPhase({ kind: "idle" });
        toast.error(`Couldn't read ${picked.name}`);
      },
    );
  }

  function onApply() {
    if (phase.kind !== "diff") return;
    const { file } = phase;
    const token = ++requestToken.current;
    importFile.mutate(
      { id: zone.id, content: file.content, dryRun: false },
      {
        // The toast lands whatever the dialog is doing: unlike the dry run,
        // this one wrote to the zone, and that is worth saying even if the
        // dialog it was started from has since been closed.
        onSuccess: (diff) => {
          toast.success(
            `Zone replaced — ${diff.add.length} added, ${diff.change.length} changed, ${diff.delete.length} deleted.`,
          );
          if (requestToken.current !== token) return;
          setPhase({ kind: "idle" });
        },
        // A file that passed the dry run can still be refused now: the zone
        // may have moved underneath it. The same rejected view says so.
        onError: (err) => {
          if (requestToken.current !== token) return;
          setPhase({ kind: "rejected", file, errors: zoneFileErrors(err) });
        },
      },
    );
  }

  const applying = importFile.isPending && phase.kind === "diff";

  // A forwarder holds no records at all — it claims a suffix and routes it —
  // so the file it would export is an SOA and nothing else. The endpoint
  // renders it happily; the button is omitted because the download is
  // meaningless, not because the server refuses it.
  const canExport = zone.type !== "forwarder";

  return (
    <>
      {canExport && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={onExport}
          disabled={exportFile.isPending}
        >
          <Download />
          Export
        </Button>
      )}

      {canImport && (
        <>
          {/* Disabled for as long as the dialog is up, which now starts the
              moment a file is chosen. Without it a second click during the
              dry run buys a second full parse-and-diff of the whole file —
              the most expensive request this page can make. */}
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => fileInput.current?.click()}
            disabled={phase.kind !== "idle"}
          >
            <Upload />
            Import
          </Button>
          {/* Visually hidden rather than `display:none`: the button above is
              what's seen and clicked, but the input stays a real, focusable,
              labelled control so it is reachable without a pointer. */}
          <input
            ref={fileInput}
            type="file"
            accept=".zone,text/dns,text/plain"
            aria-label="Zone file"
            className="sr-only"
            disabled={requestInFlight}
            onChange={onFileChosen}
          />
        </>
      )}

      <Dialog
        open={phase.kind !== "idle"}
        onOpenChange={(open) => {
          if (!open) closeDialog();
        }}
      >
        {/*
         * The artboard's panel: 900px, inset 40px from every edge, one 1px
         * border and a drop shadow.
         *
         * Three of these classes exist to undo an rnui base class rather
         * than to state something new, because `cn()`'s tailwind-merge only
         * drops a base class when the override lands in the *same* scope:
         *
         * - `sm:max-w-*` — the base ships `sm:max-w-sm`, which lives in the
         *   `sm:` variant scope and so survives an unprefixed
         *   `max-w-[…]`. Left alone it caps this panel at 384px on any
         *   viewport ≥640px, which is a third of its designed width and
         *   collapses the whole diff layout. Pinned by an e2e width
         *   assertion (e2e/smoke.spec.ts) — jsdom cannot see it, since the
         *   DOM is identical either way and only the cascade differs.
         * - `ring-0` — the base pairs `ring-1 ring-foreground/10` with its
         *   own border; `border` is a different scope, so both would draw
         *   and the panel would carry 2px of edge where the design has 1.
         * - `rounded-none` — belt and braces. `--radius: 0` already makes
         *   the base `rounded-xl` resolve flat, but stating it here means
         *   this panel keeps hard corners even if that token ever moves.
         */}
        <DialogContent
          showCloseButton={false}
          className="flex max-h-[calc(100%-5rem)] w-[900px] max-w-[calc(100%-5rem)] flex-col gap-0 rounded-none border border-border bg-card p-0 ring-0 shadow-[0_18px_50px_rgba(0,0,0,0.34)] sm:max-w-[calc(100%-5rem)]"
        >
          {phase.kind !== "idle" && (
            <>
              <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-3">
                <DialogTitle className="text-sm font-semibold">Import zone file</DialogTitle>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {phase.file.name} · {formatBytes(phase.file.size)}
                </span>
                <DialogClose
                  render={
                    <Button
                      type="button"
                      size="icon-sm"
                      variant="ghost"
                      aria-label="Close"
                      className="ml-auto shrink-0"
                    />
                  }
                >
                  <X />
                </DialogClose>
              </div>
              <DialogDescription className="sr-only">
                Review what this file would change before applying it.
              </DialogDescription>
            </>
          )}

          {/* The dry run's wait. The artboard has no state for it — its
              `applying` covers the commit only — so this borrows that state's
              language rather than inventing a second one: the dialog stays
              open, and the primary action sits disabled with an ellipsis
              label. Keeping the same footer also means nothing jumps when the
              diff arrives and the label becomes "Apply". */}
          {phase.kind === "checking" && (
            <>
              <div className="min-h-0 flex-1 overflow-y-auto">
                {/* <output> rather than a <p role="status">: it carries that
                    role implicitly, so the wait is announced rather than
                    passing silently for anyone not watching the dialog.
                    `block` because it is inline by default. */}
                <output className="block p-6 text-center text-sm text-muted-foreground">
                  Checking what this file would change…
                </output>
              </div>
              <ImportFooter applyLabel="Checking…" applyDisabled onCancel={closeDialog} />
            </>
          )}

          {phase.kind === "diff" && (
            <>
              <div className="flex shrink-0 items-center gap-3.5 border-b border-border px-4 py-2.5 font-mono text-xs">
                <span className="text-success-foreground">+{phase.diff.add.length} added</span>
                <span className="text-warning-foreground">~{phase.diff.change.length} changed</span>
                <span className="text-destructive-foreground">
                  −{phase.diff.delete.length} deleted
                </span>
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto">
                {/* Re-importing a file that was just exported is the ordinary
                    way to reach this, and three empty groups under three
                    zeroes would read as a failure to load rather than as the
                    answer "nothing would change". */}
                {phase.diff.add.length === 0 &&
                  phase.diff.change.length === 0 &&
                  phase.diff.delete.length === 0 && (
                    <p className="p-6 text-center text-sm text-muted-foreground">
                      This file matches the zone. Applying it would change nothing.
                    </p>
                  )}
                <DiffGroup
                  tone="add"
                  total={phase.diff.add.length}
                  rows={phase.diff.add.slice(0, MAX_DIFF_ROWS).map((r, i) => ({
                    key: `add-${i}`,
                    name: r.name,
                    type: r.type,
                    rdata: r.rdata,
                    transition: "",
                  }))}
                />
                <DiffGroup
                  tone="change"
                  total={phase.diff.change.length}
                  rows={phase.diff.change.slice(0, MAX_DIFF_ROWS).map((c, i) => ({
                    key: `change-${i}`,
                    name: c.to.name,
                    type: c.to.type,
                    rdata: c.to.rdata,
                    transition: changeTransition(c),
                  }))}
                />
                <DiffGroup
                  tone="delete"
                  total={phase.diff.delete.length}
                  rows={phase.diff.delete.slice(0, MAX_DIFF_ROWS).map((r, i) => ({
                    key: `delete-${i}`,
                    name: r.name,
                    type: r.type,
                    rdata: r.rdata,
                    transition: "",
                  }))}
                />
              </div>
              <ImportFooter
                applyLabel={applying ? "Applying…" : "Apply"}
                applyDisabled={applying}
                onApply={onApply}
                onCancel={closeDialog}
              />
            </>
          )}

          {phase.kind === "rejected" && (
            <>
              <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2.5 shadow-[inset_3px_0_0_var(--destructive)]">
                <span className="text-[12.5px] font-medium text-destructive-foreground">
                  File rejected — nothing was written.
                </span>
                <span className="font-mono text-[11.5px] text-muted-foreground">
                  {phase.errors.length} {phase.errors.length === 1 ? "problem" : "problems"}
                </span>
              </div>
              {/* Rendered exactly as they arrived, one per line. The server
                  already phrases these — most name a line or the offending
                  record, a file-wide problem names neither — so there is no
                  prefix worth parsing and rewriting them would only lose
                  what they say. */}
              <div className="min-h-0 flex-1 overflow-y-auto">
                {phase.errors.map((message, i) => (
                  <p
                    key={`${i}-${message}`}
                    className="border-b border-border-muted px-4 py-[5px] font-mono text-[12.5px] leading-[1.35] text-destructive-foreground"
                  >
                    {message}
                  </p>
                ))}
              </div>
              <div className="flex shrink-0 items-center justify-end gap-2 border-t border-border px-4 py-3">
                <Button type="button" size="sm" variant="ghost" onClick={closeDialog}>
                  Close
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => fileInput.current?.click()}
                >
                  Choose file
                </Button>
              </div>
            </>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}
