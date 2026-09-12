import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router";
import {
  ArrowDownToLine,
  ChevronRight,
  EllipsisVertical,
  Lock,
  Plus,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, useFormState, useWatch, type Control } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertTitle,
  Badge,
  Button,
  cn,
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Form,
  FormControl,
  FormField,
  FormItem,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { Zone } from "../../api/types";
import {
  useCreateZone,
  useCloneZone,
  useDeleteZone,
  useRecordsFollowTransfers,
  useRefreshZone,
  useUpdateZone,
  useZoneRecords,
  useZones,
} from "../../hooks/use-zones";
import { useTSIGKeys } from "../../hooks/use-tsig-keys";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { relativeTime } from "../../lib/format";
import { dnsNameSchema } from "../../lib/dns-name";
import { RenameDialog } from "../dialogs";
import { lastTransferError, pullsFromAMaster, pullState, transferErrorLead } from "../../lib/zones";
import { ZONE_TYPE_VARIANT } from "./zone-type-variant";

/**
 * The types the create row's select offers, which is exactly the set the API
 * accepts (handleZoneCreate in internal/api/zones_handlers.go — anything else
 * is a 400).
 *
 * Milestone D2 widened this from `primary` alone; D6 finishes it. The rule
 * has never changed: an option appears here when the server will actually
 * build the zone it names. `internal` is the only one left out, and always
 * will be — it is the RFC 6303 set seeded at migration and is refused on
 * create and patch alike.
 *
 * Order matters a little: the two that hold a zone come first, then the two
 * that claim a suffix and route it.
 */
const CREATABLE_TYPES = [
  "primary",
  "secondary",
  "stub",
  "forwarder",
] as const satisfies readonly Zone["type"][];

/** The clone dialog's one field. Same rule the create row applies to the
 * same value, and for the same reason: the server decides, this only saves a
 * round trip that would always 400. */
const cloneSchema = z.object({
  name: dnsNameSchema("Enter a domain name, e.g. example.com"),
});

const addZoneSchema = z
  .object({
    // Both ends of this check are the server's `dns.CanonicalName` — see
    // lib/dns-name.ts.
    name: dnsNameSchema("Enter a domain name, e.g. example.com"),
    type: z.enum(CREATABLE_TYPES),
    /**
     * Comma-separated `host[:port]`. Deliberately free text beyond "not
     * blank": zones.ValidatePrimaries is the real check and it accepts IP
     * literals, bracketed IPv6, and bare hostnames with per-entry ports, so a
     * second implementation of that grammar here would only drift from it.
     * The server's own message lands on the field when it disagrees.
     */
    primaries: z.string(),
    /**
     * Where a forwarder sends the queries it claims — a separate key from
     * `primaries` above, because they are separate columns and the server
     * 400s each on the type the other belongs to. One shared field would be
     * posted under the wrong name half the time.
     *
     * Same free-text reasoning, and one difference that matters: unlike
     * `primaries` this may legitimately be **empty**. A forwarder with no
     * upstreams still claims its suffix and answers SERVFAIL beneath it
     * rather than falling through (§9.11.5), which the server accepts
     * deliberately — so there is no rejection here to mirror.
     */
    forward_to: z.string(),
    /**
     * The chosen key's **id**, as the select's string value; "" is no key.
     *
     * Id, not name — this is the one place the artboard is actively
     * dangerous. It draws this select valued by key name (`xfer.e412.in.`),
     * but the API field is `tsig_key_id`, so a name-valued select needs a
     * name→id lookup at submit time and silently attaches the *wrong key* the
     * moment that lookup misses (two keys, a rename between fetch and
     * submit). Valuing the option with the id is the same fix lib/tsig.ts
     * applies to the algorithm select: carry the API's own value, and let the
     * label be the only thing that is for humans.
     */
    tsig_key_id: z.string(),
  })
  .superRefine((values, ctx) => {
    // The server refuses a secondary or a stub with no primaries, and it is
    // right to: one that names nowhere to pull from can never transfer or
    // fetch, so it would claim its whole suffix and answer SERVFAIL for it
    // forever. Caught here only to save the round trip, and phrased as the
    // thing to type rather than as "required".
    if (pullsFromAMaster(values.type) && values.primaries.trim() === "") {
      ctx.addIssue({
        code: "custom",
        message: "Where to pull from, e.g. 192.168.150.1",
        path: ["primaries"],
      });
    }
  });
// Exported so list.test.tsx can build a standalone `useForm<AddZoneValues>`
// harness for the type-dependent create-row cells below.
export type AddZoneValues = z.infer<typeof addZoneSchema>;
const ADD_ZONE_DEFAULTS: AddZoneValues = {
  name: "",
  type: "primary",
  primaries: "",
  forward_to: "",
  tsig_key_id: "",
};

/** One declaration of the column geometry, shared by the header, the create
 * row and every zone row — matching the artboard's grid exactly.
 *
 * STATUS is 192px rather than the 168px this started at, which is the
 * artboard's own number and the width the column needs now that it holds the
 * warning sub-line as well as the status word (see StatusCell). The 24px
 * comes off NAME, the only flexible column. */
const GRID = "grid grid-cols-[1fr_108px_192px_116px_84px_116px_92px] items-center gap-3.5 px-4";

/** "17 days" — the age inside an expired secondary's sentence ("No transfer
 * for 17 days, past the SOA expiry"), and never the artboard's literal `17d`,
 * which is a mock's number and wrong for every other zone. lib/format.ts's
 * relativeTime abbreviates ("17d ago"); this line spells the unit out, so it
 * gets its own tiny formatter rather than a flag on the shared one. Only
 * ever fed a real refreshed_at (secondary zones only), so it doesn't need
 * relativeTime's "never"/zero-sentinel handling. */
function daysAgo(epochMs: number): string {
  const days = Math.max(0, Math.floor((Date.now() - epochMs) / 86_400_000));
  return `${days} ${days === 1 ? "day" : "days"}`;
}

/**
 * What the STATUS cell says, which for a zone whose contents arrive from
 * somewhere else is a different question from "is it enabled".
 *
 * A secondary holds its primary's data on loan and a stub holds an NS set it
 * fetched. Both **claim their suffix outright**, and both have states in which
 * they hold nothing while `enabled` is still true in the database: a secondary
 * that has never transferred or has expired (Zone.Serving,
 * internal/zones/answer.go), a stub that has never fetched. Every name beneath
 * such a zone gets SERVFAIL and none of them falls through to the default
 * resolvers — for a split-horizon zone, falling through is the public internet
 * answering for an internal name, which is why the claim is kept rather than
 * dropped. So a cell that read `enabled` alone would call a suffix-wide outage
 * healthy, in green. That is the bug this function exists to prevent, and it
 * said "secondary" for a milestone after `stub` learned to pull.
 *
 * The artboard makes this the pulled zone's whole column, and it is right to:
 * the pull state *is* the thing worth knowing about a zone that is a copy.
 *
 * A **forwarder is deliberately not here**: it stores no attempt at all (see
 * pullsFromAMaster), so its "Enabled" means enabled and nothing more — which
 * is what its tooltip says out loud.
 *
 * Disabled is checked first, for every type. A zone the admin switched off is
 * not answering because they switched it off, and the scheduler skips it
 * entirely — reporting its pull state instead would be pointing at a
 * consequence and calling it the cause.
 */
function zoneStatus(zone: Zone): { dot: string; text: string; label: string } {
  const muted = { dot: "bg-muted-foreground", text: "text-muted-foreground" };
  const bad = { dot: "bg-destructive", text: "font-semibold text-destructive-foreground" };
  const warn = { dot: "bg-warning", text: "font-semibold text-warning-foreground" };

  if (!zone.enabled) return { ...muted, label: "Disabled" };
  if (!pullsFromAMaster(zone.type)) return { dot: "bg-success", text: "", label: "Enabled" };

  // A stub fetches an NS set by two ordinary queries; it does not transfer,
  // and it never expires. Every noun below switches on this.
  const fetches = zone.type === "stub";

  switch (pullState(zone)) {
    // One label for both, because it is one fact: the zone holds nothing it
    // may speak for and answers SERVFAIL. *Which* of the two it is belongs in
    // the tooltip, which has room to say it. (`expired` is a secondary's
    // alone — pullState never returns it for a stub, which is the whole
    // reason a stub goes through pullState and not transferState.)
    case "never":
    case "expired":
      return { ...bad, label: "Not answering" };
    // Not "Serving, transfer failing": the tooltip ends in "Still serving the
    // copy from 31h ago.", which says the serving half better than a clause in
    // a status cell can — including *what* is being served and how old it is.
    case "failing":
      return { ...warn, label: fetches ? "Fetch failing" : "Transfer failing" };
    // Overdue keeps its "Serving," because its own sentence does not say it:
    // nothing has transferred, and that sentence is about the schedule rather
    // than about what is being answered. A secondary's alone, like `expired`.
    case "overdue":
      return { ...warn, label: "Serving, transfer overdue" };
    default:
      // Not "Enabled": for a copy, when it was last confirmed current is
      // strictly more than the fact that it is switched on.
      return {
        dot: "bg-success",
        text: "",
        label: `${fetches ? "Fetched" : "Refreshed"} ${relativeTime(zone.refreshed_at)}`,
      };
  }
}

/** A forwarder's status cell says "Enabled" and means it, so this says what
 * it does *not* mean. Its upstreams' reachability is live and nothing about
 * it is stored — there is no column to read and the list would have to invent
 * one. The apostrophe is U+2019, taken from the artboard. */
const FORWARDER_STATUS_TIP = "Upstream health isn’t tracked.";

/**
 * The warning under a pulled zone that is not in good standing, or null when
 * it is — for either type that pulls, with every noun switched on which.
 *
 * The cause comes from the zone row (`last_error`), not from anything this
 * page remembers, which is what lets it be here at all: the scheduler's own
 * record of a failure is process-local, so a row built on that would go blank
 * after a restart for a zone that is still failing.
 *
 * Three separate values rather than one sentence, because they are drawn in
 * three places and two of them have no length bound in common. `when` and
 * `cause` are the status cell's own sub-line: when it was last tried, and
 * what the server said, which is the longest string on the row and the only
 * thing that gives way when it runs out of width. `label` and `note` are the
 * tooltip's —
 * the artboard renders neither inline, and the row is not made to depend on
 * them: it already carries the alarm in weight, colour, dot, edge mark, tint
 * and that sub-line.
 */
/**
 * The pull action's label, which follows the type and nothing else: a stub
 * *fetches* an NS set, a secondary *transfers* a copy. Split out of
 * pullWarning because the menu item is offered on `secondary || stub` while
 * the warning only exists when one of them is in trouble — one source for the
 * verb, so the two can never disagree about what a stub does.
 */
function pullVerbFor(zone: Zone): { retry: string; retrying: string } {
  const fetches = zone.type === "stub";
  return {
    retry: fetches ? "Fetch now" : "Retry transfer",
    retrying: fetches ? "Fetching…" : "Transferring…",
  };
}

function pullWarning(zone: Zone): {
  label: string;
  tone: "bad" | "warn";
  when: string;
  cause: { lead: string; full: string } | null;
  note: string;
  retry: string;
  retrying: string;
} | null {
  if (!pullsFromAMaster(zone.type) || !zone.enabled) return null;
  const fetches = zone.type === "stub";
  const failure = lastTransferError(zone);
  const base = {
    // The attempt slot. `0` is not a date, and dating a failure that never
    // happened is worse than saying there has not been one.
    when: zone.last_attempt === 0 ? "never attempted" : relativeTime(zone.last_attempt),
    cause: failure ? { lead: transferErrorLead(failure.message), full: failure.message } : null,
    ...pullVerbFor(zone),
  };
  // The same age the row shows, in a sentence — so the tooltip never dates an
  // attempt the row does not.
  const tried = zone.last_attempt === 0 ? "" : `Tried ${relativeTime(zone.last_attempt)}. `;

  switch (pullState(zone)) {
    case "never":
      return {
        ...base,
        label: fetches ? "Never fetched" : "Never transferred",
        tone: "bad",
        // The apex, not the primaries: what is being said is which names go
        // unanswered, and the zone's own name is the whole set of them.
        note: `${tried}Answering nothing under ${zone.name}.`,
      };
    case "expired":
      // Secondary-only by construction — see pullState.
      return {
        ...base,
        label: "Expired",
        tone: "bad",
        note: `${tried}No transfer for ${daysAgo(zone.refreshed_at)}, past the SOA expiry. Answering nothing.`,
      };
    case "failing":
      return {
        ...base,
        label: fetches ? "Last fetch" : "Last transfer",
        tone: "warn",
        note: `${tried}Still ${
          fetches ? "routing on the NS set" : "serving the copy"
        } from ${relativeTime(zone.refreshed_at)}.`,
      };
    case "overdue":
      // Secondary-only, and the one state with nothing recorded against it
      // (see lib/zones.ts) — so there is no cause, by definition.
      return {
        ...base,
        label: "Overdue",
        tone: "warn",
        cause: null,
        note: `Nothing has transferred from ${zone.primaries || "unknown"} since ${relativeTime(zone.refreshed_at)}, past the refresh the SOA asks for.`,
      };
    default:
      return null;
  }
}

/**
 * The Records column: a per-zone request rather than a field on Zone itself
 * (the API has no record-count field — see api/types.ts). Same shape as
 * filtering/groups-clients.tsx's GroupListsCell, which reads a per-group
 * query the same way for the same reason: the list endpoint doesn't carry
 * it, and the count is still worth showing without turning this page into a
 * records browser. Grouped with `.toLocaleString()`, unlike Serial beside it
 * — the artboard draws that distinction deliberately (a serial is an opaque
 * counter, not a quantity).
 *
 * **Asked for only once the row is on screen.** Eighty zones meant eighty
 * requests on load, for a number most of them are scrolled past without ever
 * being read — and every record write invalidates the whole `zoneRecords`
 * tree, so each one cost eighty more. Deferring to an IntersectionObserver
 * keeps the column (dropping it to a lighter signal would mean dropping the
 * count, and there is no lighter signal on the row to replace it with) while
 * paying for it only where it is actually read. The real fix is
 * `record_count` on `GET /zones`, which is a server change and not this
 * one's to make.
 *
 * A browser without IntersectionObserver falls back to asking immediately:
 * the count is the point, and "on screen" is a question that environment
 * cannot answer.
 */
function RecordsCell({ zoneId }: { zoneId: number }) {
  const cell = useRef<HTMLSpanElement>(null);
  const [onScreen, setOnScreen] = useState(false);
  useEffect(() => {
    if (onScreen) return;
    const el = cell.current;
    if (!el || typeof IntersectionObserver === "undefined") {
      setOnScreen(true);
      return;
    }
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) setOnScreen(true);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, [onScreen]);

  const records = useZoneRecords(zoneId, { enabled: onScreen });
  return (
    <span ref={cell} className="text-right font-mono text-sm">
      {records.data ? records.data.length.toLocaleString() : "—"}
    </span>
  );
}

/**
 * The one type-dependent field: where this zone's queries go, under whichever
 * name the chosen type calls it.
 *
 * One cell, and one column of the grid, but **two form keys** — and that is
 * the correction the artboard needs. It draws a single `needsPrimaries` field
 * covering secondary, stub *and* forwarder, which would post `primaries` for
 * a forwarder and be refused every time: checkZoneTransferConfig 400s
 * "primaries applies to secondary and stub zones only" on one, and 400s
 * `forward_to` on everything that is not one. They are different columns
 * because they are different things — a master to pull a zone or an NS set
 * from, versus upstreams to send live queries to — so the label, the
 * placeholder and the key all switch together.
 *
 * The artboard puts this in the STATUS column — a column a zone that does not
 * exist yet has nothing to put in anyway — so the row keeps one grid rather
 * than reflowing when the type changes.
 *
 * `type` is a plain prop rather than a `form.watch` of its own, so
 * list.test.tsx can render this cell directly with any type instead of having
 * to drive the select first.
 */
export function UpstreamField({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  const forwarder = type === "forwarder";
  if (!forwarder && !pullsFromAMaster(type)) return <span />;
  return (
    <FormField
      control={control}
      name={forwarder ? "forward_to" : "primaries"}
      render={({ field }) => (
        <FormItem>
          <FormControl>
            <Input
              {...field}
              aria-label={forwarder ? "Forward to" : "Primary servers"}
              placeholder={forwarder ? "10.0.0.1, 10.0.0.2:5353" : "192.168.150.1:53"}
              autoComplete="off"
              className="font-mono"
            />
          </FormControl>
        </FormItem>
      )}
    />
  );
}

/**
 * The TSIG key select, in the SERIAL column and for the two types that pull
 * from a master.
 *
 * A stub signs its SOA/NS queries with the key exactly as a secondary signs
 * its transfer requests — a master that requires TSIG on ordinary queries
 * would otherwise refuse the fetch — and the server allows `tsig_key_id` on
 * precisely those two. A forwarder signs nothing: it sends plain queries, not
 * transfers, and the field is 400ed on it.
 *
 * Options are valued by **id**, labelled by name — see the schema's
 * `tsig_key_id` comment for why the artboard's name-valued select is a
 * silent-wrong-key bug rather than a style choice.
 *
 * Fetches the key list itself, so nothing is requested until one of those two
 * is actually being created. An instance with no keys still gets the select,
 * showing only "No TSIG": an unsigned pull is a legitimate configuration (a
 * master on a trusted LAN), so there is nothing here to guide anyone out of,
 * and a control that vanishes is a control someone goes looking for.
 */
export function TSIGKeyField({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  if (!pullsFromAMaster(type)) return <TSIGKeyFieldPlaceholder />;
  return <TSIGKeySelect control={control} />;
}

/** The SERIAL column's resting content: the same em dash every other
 * server-decided cell in the create row shows. */
function TSIGKeyFieldPlaceholder() {
  return <span className="text-right font-mono text-sm text-muted-foreground">—</span>;
}

function TSIGKeySelect({ control }: { control: Control<AddZoneValues> }) {
  const keys = useTSIGKeys();
  return (
    <FormField
      control={control}
      name="tsig_key_id"
      render={({ field }) => (
        <FormItem>
          <FormControl>
            <NativeSelect {...field} aria-label="TSIG key">
              <NativeSelectOption value="">No TSIG</NativeSelectOption>
              {(keys.data ?? []).map((key) => (
                <NativeSelectOption key={key.id} value={String(key.id)}>
                  {key.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </FormControl>
        </FormItem>
      )}
    />
  );
}

/**
 * The create row's hint line: column 1 for a primary ("SOA defaults are
 * filled in."), columns 3 and 4 under the two fields the other types show —
 * matching the artboard rather than pinning everything to column 1. A field's
 * validation error takes over its own column when there is one.
 *
 * Column 1's hint is a primary's alone. The other three types have no SOA to
 * default: a secondary's and a stub's arrive with the pull and are
 * overwritten by the next one, and a forwarder answers from no records for an
 * SOA to head.
 *
 * **Both errors are read here rather than passed in**, and that is a
 * correction rather than a preference. They used to arrive as props
 * (`nameError`, `primariesError`) chosen by the caller, which was harmless
 * while one field could produce one — and stopped being harmless the moment
 * this cell grew a second upstream field. A caller selecting
 * `errors.primaries` and a cell rendering `<FormField name="primaries">` are
 * two places to make the same mistake, on the one type that has nowhere else
 * to show the error. Asking the form for the error on the field this cell is
 * about to render collapses both into one and cannot disagree with itself.
 *
 * Same note as UpstreamField: `type` is a prop rather than read from the
 * form, so this cell can be rendered directly in a test.
 */
export function CreateRowHint({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  const forwarder = type === "forwarder";
  const pulls = pullsFromAMaster(type);
  // The field whose error this cell shows, which is the same switch
  // UpstreamField makes one row above — deliberately derived from `type`
  // once, so the message and the input it sits under can never name
  // different fields.
  const upstreamField = forwarder ? "forward_to" : "primaries";
  const { errors } = useFormState({ control });
  const nameError = errors.name?.message;
  const upstreamError = errors[upstreamField]?.message;
  return (
    <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
      <span>
        {nameError ? (
          <FormField control={control} name="name" render={() => <FormMessage />} />
        ) : type === "primary" ? (
          <span className="text-muted-foreground">SOA defaults are filled in.</span>
        ) : null}
      </span>
      <span />
      <span>
        {forwarder || pulls ? (
          upstreamError ? (
            // Nothing rejects a forwarder's upstreams *today* — they may
            // legitimately be empty, and no grammar is checked here — so this
            // branch is only ever reached for `primaries` at the moment. It
            // is written for both anyway, because the alternative is a cell
            // that silently drops the first `forward_to` rule anyone adds.
            <FormField control={control} name={upstreamField} render={() => <FormMessage />} />
          ) : (
            <span className="text-muted-foreground">
              {forwarder
                ? "Upstream servers, comma separated."
                : "Primary servers, comma separated."}
            </span>
          )
        ) : null}
      </span>
      <span>{pulls && <span className="text-muted-foreground">Optional.</span>}</span>
      <span />
      <span />
      <span />
    </div>
  );
}

/**
 * The create row: one persistent band, not a dialog — matching the create
 * affordance already used for groups and clients (see
 * filtering/groups-clients.tsx's AddGroupRow). Closes on success rather
 * than resetting and staying open the way the client row does — adding a
 * zone is a one-off act, not "add four in a row" the way DNS records are.
 */
function AddZoneRow({ onClose }: { onClose: () => void }) {
  const createZone = useCreateZone();
  const form = useForm<AddZoneValues>({
    resolver: zodResolver(addZoneSchema),
    defaultValues: ADD_ZONE_DEFAULTS,
  });

  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  const type = useWatch({ control: form.control, name: "type" });

  function onSubmit(values: AddZoneValues) {
    // Every type-dependent field is sent only for the types it applies to and
    // omitted otherwise, never sent empty: the server refuses each of them on
    // a zone that has no use for it, deliberately, rather than storing
    // configuration nothing will read (checkZoneTransferConfig in
    // zones_handlers.go).
    const upstreams = pullsFromAMaster(values.type)
      ? {
          primaries: values.primaries.trim(),
          // "" means an unsigned pull, which is 0 on the wire — and 0 is also
          // what the server reads an omitted field as, so the key is only
          // named when one was actually chosen.
          ...(values.tsig_key_id === "" ? {} : { tsig_key_id: Number(values.tsig_key_id) }),
        }
      : values.type === "forwarder" && values.forward_to.trim() !== ""
        ? { forward_to: values.forward_to.trim() }
        : {};
    createZone.mutate(
      {
        name: values.name.trim(),
        // Omitted at the default: handleZoneCreate already treats a
        // missing type as "primary" (zones_handlers.go), so there is no
        // need to say it explicitly in the common case.
        ...(values.type === "primary" ? {} : { type: values.type }),
        ...upstreams,
      },
      {
        onSuccess: () => {
          toast.success("Zone added");
          onClose();
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the zone"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="add-zone-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Zone name"
                    placeholder="home.lan"
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="type"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  {/* The four types the API creates — see CREATABLE_TYPES. */}
                  <NativeSelect {...field} aria-label="Zone type">
                    {CREATABLE_TYPES.map((t) => (
                      <NativeSelectOption key={t} value={t}>
                        {t}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          <UpstreamField type={type} control={form.control} />
          <TSIGKeyField type={type} control={form.control} />
          <span className="text-right font-mono text-sm text-muted-foreground">—</span>
          <span className="text-right font-mono text-xs text-muted-foreground">—</span>
          <div className="flex items-center justify-end gap-1">
            <Button type="submit" size="sm" disabled={createZone.isPending}>
              {createZone.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the add-zone row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>

        <CreateRowHint type={type} control={form.control} />
      </form>
    </Form>
  );
}

/**
 * The STATUS cell: the status word, the sub-line underneath it when a pulled
 * zone is in trouble, and — for the two rows that have something to explain —
 * the trigger of the tooltip that explains it.
 *
 * The sub-line lives **here**, in this one column, rather than in a band
 * across the foot of the row. That is the artboard's arrangement and it is
 * the right one: a warned row is two lines tall inside a single cell, so
 * every other column on it stays on the same baseline as its neighbours' and
 * the eye can still run down SERIAL or MODIFIED without stepping over
 * anything. A full-width band under the row put the failure under the *name*
 * instead of under the status it belongs to, and pushed the alarm out of the
 * column an operator is scanning.
 *
 * A real Tooltip rather than the artboard's `title=`, for the reason any
 * `title` is replaced: it is unstyleable, slow, and appears at the pointer
 * rather than at the thing it is about. The two rows that get one are a
 * pulled zone in trouble (the label and the sentence, which are drawn nowhere
 * else) and a forwarder (whose green means less than it looks).
 *
 * It stays **supplementary**. Everything that says "this needs a human" is in
 * the row already — the weight and colour of this label, the dot beside it,
 * the edge mark and tint of the row, and the sub-line under it. A row that
 * only read correctly on hover would still lie at a glance across forty of
 * them, which is the bug this whole treatment exists to fix.
 *
 * A row with nothing to say is not a trigger at all: no help cursor, no tab
 * stop, no popup. Forty tooltips that say "Enabled" is how a tooltip stops
 * being read.
 */
function StatusCell({
  zone,
  status,
  warning,
}: {
  zone: Zone;
  status: ReturnType<typeof zoneStatus>;
  warning: ReturnType<typeof pullWarning>;
}) {
  // Baseline-aligned rather than centred, and nudged down to sit on the
  // status word's own baseline — so a two-line cell keeps the dot beside the
  // line it is about instead of floating between the two.
  const dot = <span aria-hidden className={cn("mt-[5px] size-1.5 shrink-0", status.dot)} />;
  const body = (
    <span className="flex min-w-0 flex-col gap-0.5">
      <span className={cn("text-sm", status.text)}>{status.label}</span>
      {/* The sub-line: when it was last tried, then what went wrong. The
          label and the sentence about the consequence are the tooltip's —
          everything here is a fact with a date on it, and the cell keeps both
          of those whatever the tooltip does. */}
      {warning && (
        <span
          data-testid="zone-pull-warning"
          className={cn(
            "flex min-w-0 items-baseline gap-1.5 font-mono text-[10.5px]",
            warning.tone === "bad" ? "text-destructive-foreground" : "text-warning-foreground",
          )}
        >
          {/* Never shrinks: an error with no date is a claim about the
              present made by an unknown past. */}
          <span data-testid="zone-pull-attempt" className="shrink-0 whitespace-nowrap">
            {warning.when}
          </span>
          {/* The server's own words, and the one thing in this cell with no
              bound on its length — so it is the only thing here that gives
              way when the column runs out of width.

              No `title` here. It used to carry the untruncated message, which
              put a second hover surface inside the cell the Tooltip already
              wraps: both fired, at different moments, in different styles,
              saying different things. The full message is in the Tooltip
              above, and the zone's own page prints it too. */}
          {warning.cause && (
            <span data-testid="zone-pull-error" className="min-w-0 truncate opacity-80">
              {warning.cause.lead}
            </span>
          )}
        </span>
      )}
    </span>
  );

  if (!warning && zone.type !== "forwarder") {
    return (
      <span data-testid="zone-status" className="flex min-w-0 items-baseline gap-2">
        {dot}
        {body}
      </span>
    );
  }

  return (
    <Tooltip>
      {/* A span, not the default button: this is a cell of a table, and the
          hover is help rather than an action. `tabIndex` is what keeps it
          reachable without a mouse — Base UI opens on focus as well as on
          hover — and only rows that carry a tooltip take a tab stop. */}
      <TooltipTrigger
        data-testid="zone-status"
        render={<span />}
        tabIndex={0}
        className="flex min-w-0 cursor-help items-baseline gap-2 rounded-xs"
      >
        {dot}
        {body}
      </TooltipTrigger>
      {/* align="start" and 380px are the artboard's, not the component's
          defaults (center, max-w-xs = 320px): the popup left-edge-aligns to
          the status it is about, and the extra 60px is what stops the expired
          sentence wrapping "past the SOA / expiry". */}
      <TooltipContent
        data-testid="zone-status-tip"
        role="tooltip"
        align="start"
        // bg-tooltip rather than the component's bg-foreground, which flips
        // with the mode and put a near-white slab on a near-black page. The
        // arrow is styled separately inside the component and inherits none
        // of this, so it is reached by the only handle it offers: it is the
        // one descendant carrying data-side.
        className="max-w-[380px] bg-tooltip text-tooltip-foreground [&_[data-side]]:bg-tooltip [&_[data-side]]:fill-tooltip"
      >
        {warning ? (
          <span className="flex flex-col gap-1.5">
            <span className="font-mono text-[9.5px] leading-none font-semibold tracking-[0.14em] uppercase opacity-[0.72]">
              {warning.label}
            </span>
            <span className="text-[12.5px] leading-[1.4] text-pretty">{warning.note}</span>
            {/* The server's own words, in full. The sub-line under the status
                truncates them to whatever the column has left, and this is
                the only place in the list that does not — so it belongs in
                the one hover surface rather than in a second one. */}
            {warning.cause && (
              <span
                data-testid="zone-status-tip-cause"
                // break-all because this is the one string here with no
                // bound and no spaces to break at: a dial error is one long
                // token, and without it the popup grows past its own width
                // instead of wrapping.
                className="font-mono text-[11.5px] leading-[1.45] break-all opacity-[0.78]"
              >
                {warning.cause.full}
              </span>
            )}
          </span>
        ) : (
          FORWARDER_STATUS_TIP
        )}
      </TooltipContent>
    </Tooltip>
  );
}

/**
 * One zone. `internal` zones are read-only — no actions menu at all —
 * because they are the RFC 6303 zones that stop junk queries (localhost, the
 * reverse-mapping arpa zones, …) reaching the root servers, seeded in a
 * later milestone rather than managed by hand. The Actions cell says so
 * outright (`BUILT-IN`) rather than merely omitting the control, so an admin
 * scanning the column never wonders whether it failed to render.
 */
function ZoneRow({
  zone,
  onClone,
  onDelete,
}: {
  zone: Zone;
  /** Absent for a built-in zone, which has no menu to hold the action. */
  onClone?: () => void;
  onDelete?: () => void;
}) {
  const isInternal = zone.type === "internal";
  const status = zoneStatus(zone);
  const warning = pullWarning(zone);
  // Zone *definitions* travel in the bundle, so on a replica clone, enable,
  // disable and delete are the main's. Pulling is not: a copy fetching its
  // current version is this box's own operational act (spec §7).
  const managedBy = useManagedBy();
  const pullVerb = pullVerbFor(zone);
  const refreshZone = useRefreshZone();
  const updateZone = useUpdateZone();
  // One endpoint, two words for it: POST /zones/{id}/refresh takes either
  // type that pulls (the server's own gate — "only secondary and stub zones
  // pull from a master"), and a stub fetches an NS set rather than
  // transferring a zone.
  const fetches = zone.type === "stub";

  function onPullNow() {
    refreshZone.mutate(zone.id, {
      onSuccess: () => toast.success(`${zone.name} ${fetches ? "fetched" : "transferred"}`),
      // The server's text names every primary it tried and why each refused
      // — see useRefreshZone. There is no band on this row to hold it, so it
      // goes to a toast rather than being thrown away for a shorter message.
      onError: (err) =>
        toast.error(
          err instanceof ApiError
            ? err.message
            : `Couldn't ${fetches ? "fetch" : "transfer"} ${zone.name}`,
        ),
    });
  }

  /**
   * Enable or disable, ported verbatim from the zone detail page's own
   * `onToggleEnabled` (detail.tsx) — same PATCH of `{ enabled }`, same two
   * toasts, and deliberately the same *absence* of a confirmation. Disabling
   * is one click to undo and destroys nothing, so a dialog in front of it
   * would only train people to dismiss the one that guards the delete.
   */
  function onToggleEnabled() {
    updateZone.mutate(
      { id: zone.id, enabled: !zone.enabled },
      {
        onSuccess: () => toast.success(zone.enabled ? "Zone disabled" : "Zone enabled"),
        onError: () => toast.error(`Couldn't ${zone.enabled ? "disable" : "enable"} ${zone.name}`),
      },
    );
  }

  return (
    <div
      data-testid="zone-row"
      data-slot="zone-row"
      className={cn(
        "border-b border-border-muted",
        isInternal && "bg-muted/30 text-muted-foreground",
        // A row whose status cell carries a warning gets a left-edge mark
        // and a tint — the same "this needs a human" treatment idle filter
        // lists get in filtering/lists.tsx, on the row that holds the reason.
        warning?.tone === "bad" && "bg-destructive/5 shadow-[inset_3px_0_0_var(--destructive)]",
        warning?.tone === "warn" && "bg-warning/5 shadow-[inset_3px_0_0_var(--warning)]",
      )}
    >
      <div className={cn(GRID, "py-2.5")}>
        {isInternal ? (
          <span className="flex min-w-0 items-center gap-1.5 truncate text-sm" title={zone.name}>
            <Lock aria-hidden="true" className="size-3.5 shrink-0" />
            <span className="truncate">{zone.name}</span>
          </span>
        ) : (
          // Zone detail (Task 12) owns SOA fields and records — see
          // pages/zones/detail.tsx.
          <span className="flex min-w-0 items-center gap-1.5">
            <Link
              to={`/zones/${zone.id}`}
              className="truncate text-sm font-medium text-primary underline decoration-2 underline-offset-[3px]"
              title={zone.name}
            >
              {zone.name}
            </Link>
            {/* A pulled zone is marked at its name, not only by its type
                badge: the badge says what kind of zone it is, this says the
                records under it were written by someone else. It is also the
                one thing that stays visible when the type column is scanned
                past. */}
            {pullsFromAMaster(zone.type) && (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <ArrowDownToLine
                      aria-label="Pulled from another server"
                      className="size-3 shrink-0 text-muted-foreground"
                    />
                  }
                />
                <TooltipContent role="tooltip">Pulled from another server</TooltipContent>
              </Tooltip>
            )}
          </span>
        )}
        <span>
          <Badge variant={ZONE_TYPE_VARIANT[zone.type]}>{zone.type}</Badge>
        </span>
        <StatusCell zone={zone} status={status} warning={warning} />
        {/* No thousands grouping — a serial is an opaque counter, not a
            quantity, and the artboard draws that line deliberately. */}
        <span className="text-right font-mono text-sm">{zone.soa_serial}</span>
        <RecordsCell zoneId={zone.id} />
        <span className="text-right font-mono text-xs">{relativeTime(zone.modified_at)}</span>
        <span className="flex items-center justify-end">
          {isInternal ? (
            // 9.5px, not text-xs (12px) — matches the Expired badge's own
            // text-[9.5px] below and the artboard's spec for this cell.
            <span className="font-mono text-[9.5px] tracking-widest text-muted-foreground uppercase">
              Built-in
            </span>
          ) : (
            /**
             * One kebab rather than a row of icon buttons — the artboard's
             * own shape, and what lets three actions of very different weight
             * (fetch, toggle, delete) share a 92px cell without a red trash
             * can sitting permanently under the pointer.
             *
             * Nothing is lost by dropping the pencil that used to be here: it
             * was a Link to the detail page, never a rename, and the zone's
             * name in the first column is the same link.
             *
             * **Rename is deliberately absent** though the artboard draws it:
             * no rename exists anywhere in this app or its API.
             */
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    type="button"
                    size="icon-sm"
                    variant="ghost"
                    // Named for the zone, not just "Actions": in a list of
                    // forty rows the bare word is forty identical buttons.
                    aria-label={`Actions for ${zone.name}`}
                  />
                }
              >
                <EllipsisVertical />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                {/* The pull action, on `secondary || stub` — the API's own
                    gate (pullsFromAMaster in zones_handlers.go, which answers
                    everything else 400 "only secondary and stub zones pull
                    from a master"), and the same one the detail page's
                    "Refresh now" uses.

                    Not gated on the zone being in trouble. An earlier draft
                    tied this to `warning`, because that was the condition the
                    band it replaced appeared under — but that was an artifact
                    of the band, not a rule: "go and get the current version
                    now" is a reasonable thing to ask of a copy that is
                    perfectly healthy, and the detail page has always allowed
                    it. Gating it here would have meant the same zone offering
                    the action on one screen and withholding it on the other.

                    A forwarder is outside the gate: it has no master and
                    nothing to fetch, so the item would be a no-op that
                    returns an error. */}
                {pullsFromAMaster(zone.type) && (
                  <>
                    <DropdownMenuItem onClick={onPullNow} disabled={refreshZone.isPending}>
                      {refreshZone.isPending ? pullVerb.retrying : pullVerb.retry}
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                  </>
                )}
                {/* A second site's zone is this one with a different apex:
                    the same hosts, the same ACL, the same notify targets.
                    The server does the copying (POST /zones/{id}/clone) —
                    this only asks for the one value a copy cannot inherit. */}
                <DropdownMenuItem disabled={managedBy !== ""} onClick={onClone}>
                  Clone
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  onClick={onToggleEnabled}
                  disabled={updateZone.isPending || managedBy !== ""}
                >
                  {zone.enabled ? "Disable" : "Enable"}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                {/* The one item that cannot be undone, so it is the one item
                    behind a confirmation — and the only one drawn in the
                    destructive variant. */}
                <DropdownMenuItem
                  variant="destructive"
                  disabled={managedBy !== ""}
                  onClick={onDelete}
                >
                  Delete zone
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </span>
      </div>
    </div>
  );
}

/**
 * The collapsed-by-default row between the user's own zones and the RFC
 * 6303 built-ins (see internal/store/builtins.go) — 15 of them, seeded so
 * loopback and the reverse-mapping arpa ranges stop reaching the root
 * servers, and enough to bury the one or two zones a user actually cares
 * about if they rendered inline. Built on rnui's Collapsible (already used
 * for the zone detail page's SOA band — see detail.tsx's SoaBand) rather
 * than a hand-rolled clickable div: CollapsibleTrigger renders a real
 * `<button>`, which is Enter/Space-operable and carries `role="button"`
 * without any of that having to be wired up by hand here. The panel is
 * unmounted (not merely hidden) while closed — rnui's default — so the
 * built-in rows genuinely aren't in the DOM until asked for.
 *
 * No trailing "Show"/"Hide" word (deviation from the artboard, recorded in
 * the redesign notes) — the caret already carries the state and the row
 * is evidently clickable, so the word would only repeat it. Nothing lost
 * for assistive tech either: the count label is the button's own
 * accessible name and `aria-expanded` carries the state regardless.
 */
function BuiltinsDisclosure({ zones }: { zones: Zone[] }) {
  const [open, setOpen] = useState(false);
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger
        type="button"
        className="flex w-full cursor-pointer items-center gap-[9px] border-b border-border-muted px-4 py-[9px] text-left font-mono text-[11px] text-muted-foreground select-none hover:bg-muted hover:text-foreground"
      >
        <ChevronRight
          aria-hidden="true"
          className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")}
        />
        <span>
          {zones.length} built-in {zones.length === 1 ? "zone" : "zones"}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        {zones.map((zone) => (
          // No onDelete: ZoneRow draws no menu at all for an `internal`
          // zone (see its own comment), so there is nothing to hand it.
          <ZoneRow key={zone.id} zone={zone} />
        ))}
      </CollapsibleContent>
    </Collapsible>
  );
}

/**
 * Zones — the authoritative DNS zones this instance serves, replacing the
 * old flat Local DNS override table (see api/types.ts's Zone doc). A name
 * inside an enabled zone is answered or refused, never forwarded.
 *
 * GET/POST/DELETE /zones via use-zones.ts's hooks (no inline edit here —
 * enabling/disabling and SOA changes live on the zone detail page, Task 12).
 */
export function ZonesList() {
  const zones = useZones();
  const deleteZone = useDeleteZone();
  const cloneZone = useCloneZone();
  const managedBy = useManagedBy();
  // The Records column is the one thing here a transfer changes that the
  // zones response does not carry, so it follows the zone rather than
  // polling on its own — see useRecordsFollowTransfers.
  useRecordsFollowTransfers(zones.data);

  const [addOpen, setAddOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Zone | null>(null);
  const [cloneTarget, setCloneTarget] = useState<Zone | null>(null);

  const all = useMemo(() => zones.data ?? [], [zones.data]);
  // The header count and the empty state must count only the user's own
  // zones — the 15 RFC 6303 built-ins (internal/store/builtins.go) are
  // always present, so counting `all` would say "15 zones" on a fresh
  // install and `all.length === 0` could never be true, meaning the "No
  // zones yet" guidance would never show.
  const { mine, builtins } = useMemo(() => {
    const mine: Zone[] = [];
    const builtins: Zone[] = [];
    for (const zone of all) (zone.type === "internal" ? builtins : mine).push(zone);
    return { mine, builtins };
  }, [all]);

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteZone.mutate(target.id, {
      onSuccess: () => {
        toast.success("Zone deleted");
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  /**
   * The name rules are the server's, so a refusal is shown in its own words
   * and the dialog stays open on the value that was typed. The only rule
   * checked here is the shape of a domain name, which saves a round trip that
   * would always 400 — the same division AddZoneRow draws.
   */
  function onConfirmClone(name: string) {
    if (!cloneTarget) return;
    cloneZone.mutate(
      { id: cloneTarget.id, name },
      {
        onSuccess: (created) => {
          toast.success(`Cloned to ${created.name}`);
          setCloneTarget(null);
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : `Couldn't clone ${cloneTarget.name}`),
      },
    );
  }

  let body: ReactNode;
  if (zones.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (zones.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load zones</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else {
    // Rows and the empty state are mutually exclusive, so the disclosure
    // sits between them unconditionally: with rows it trails them (the
    // artboard's own order); with none, it leads the empty state instead
    // of trailing it — the empty state is `h-full` (it centers in
    // whatever height it's given), so trailing it here would push "N
    // built-in zones" entirely below the fold.
    body = (
      <>
        {mine.length > 0 &&
          mine.map((zone) => (
            <ZoneRow
              key={zone.id}
              zone={zone}
              onClone={() => setCloneTarget(zone)}
              onDelete={() => setDeleteTarget(zone)}
            />
          ))}
        {builtins.length > 0 && <BuiltinsDisclosure zones={builtins} />}
        {mine.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
            <p className="font-heading text-sm font-semibold">No zones yet</p>
            <p className="text-sm text-muted-foreground">Everything is forwarded upstream.</p>
            {!addOpen && (
              <Button
                type="button"
                size="sm"
                disabled={managedBy !== ""}
                onClick={() => setAddOpen(true)}
              >
                <Plus />
                New zone
              </Button>
            )}
          </div>
        )}
      </>
    );
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {managedBy !== "" && <ManagedNotice peer={managedBy} />}

      <div className="flex shrink-0 items-center gap-2.5 border-b border-border bg-card px-4 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {mine.length} {mine.length === 1 ? "zone" : "zones"}
        </span>
        {!addOpen && (
          <Button
            type="button"
            size="sm"
            className="ml-auto"
            disabled={managedBy !== ""}
            onClick={() => setAddOpen(true)}
          >
            <Plus />
            New zone
          </Button>
        )}
      </div>

      {zones.isError && zones.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="zones"
            onRetry={() => void zones.refetch()}
            isRetrying={zones.isFetching}
          />
        </div>
      )}

      <div
        className={cn(
          GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-xs tracking-widest text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Type</span>
        <span>Status</span>
        <span className="text-right">Serial</span>
        <span className="text-right">Records</span>
        <span className="text-right">Modified</span>
        <span className="text-right">Actions</span>
      </div>

      {addOpen && <AddZoneRow onClose={() => setAddOpen(false)} />}

      {/* Blank rather than pre-filled with the source's name: the one value a
          copy cannot inherit is the one being asked for, and a field holding
          a name the server will refuse is a field that has to be cleared
          before it can be used. */}
      <RenameDialog
        targetId={cloneTarget?.id ?? null}
        title="Clone this zone"
        description={
          cloneTarget && (
            <>
              Everything in <span className="font-medium text-foreground">{cloneTarget.name}</span>{" "}
              — its SOA settings, transfer configuration and every record — is copied under the new
              name. The serial starts at 1.
            </>
          )
        }
        label="New zone name"
        initialName=""
        placeholder="e412.dev"
        schema={cloneSchema}
        submitLabel="Clone"
        pendingLabel="Cloning…"
        isPending={cloneZone.isPending}
        onSubmit={onConfirmClone}
        onClose={() => setCloneTarget(null)}
      />

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this zone?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <span className="font-medium text-foreground">{deleteTarget.name}</span> and its
                  records will stop being served immediately.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteZone.isPending}
            >
              {deleteZone.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
