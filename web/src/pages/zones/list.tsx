import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router";
import {
  ArrowDownToLine,
  ChevronRight,
  Lock,
  Pencil,
  Plus,
  Trash2,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
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
  Form,
  FormControl,
  FormField,
  FormItem,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  type BadgeProps,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { Zone } from "../../api/types";
import {
  useCreateZone,
  useDeleteZone,
  useRecordsFollowTransfers,
  useRefreshZone,
  useZoneRecords,
  useZones,
} from "../../hooks/use-zones";
import { useTSIGKeys } from "../../hooks/use-tsig-keys";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { relativeTime } from "../../lib/format";
import { lastTransferError, transferErrorLead, transferState } from "../../lib/zones";

// A category tag, not a verdict. Exact mapping from the artboard —
// deliberately not "each type gets a fresh colour": stub shares its badge
// with the neutral `secondary` variant rather than getting one of its own.
const TYPE_VARIANT: Record<Zone["type"], NonNullable<BadgeProps["variant"]>> = {
  primary: "primary-light",
  secondary: "info-light",
  stub: "secondary",
  forwarder: "warning-light",
  internal: "secondary",
};

/**
 * The types the create row's select offers, which is exactly the set the API
 * accepts (`zoneTypePrimary`/`zoneTypeSecondary` in
 * internal/api/zones_handlers.go — anything else is a 400).
 *
 * Milestone D2 is what widened this from `primary` alone: a secondary is now
 * servable because it is *configured*, with somewhere to pull from and a
 * transfer that runs on the zone's own SOA schedule to fill it.
 *
 * `stub` and `forwarder` are deliberately still absent, and the artboard
 * offering all four is a contradiction rather than a spec. The API 400s both;
 * `forwarder` arrives in Milestone D6 and `stub` is in no milestone at all.
 * A select whose options half-work is worse than a shorter select — it
 * promises a zone type and then hands back a validation error from the
 * server.
 */
const CREATABLE_TYPES = ["primary", "secondary"] as const satisfies readonly Zone["type"][];

/** Mirrors the server's own check (internal/api/zones_handlers.go's
 * normalizeZoneName) closely enough that a name accepted here round-trips —
 * lowercasing and the trailing-dot strip happen server-side regardless, so
 * this only has to catch what would otherwise be a wasted request: blank,
 * whitespace/path characters, or an empty label (e.g. "e412..in"). */
const zoneNameSchema = z
  .string()
  .trim()
  .refine((value) => {
    const name = value.replace(/\.$/, "");
    if (name === "" || /[ \t\r\n/\\]/.test(name)) return false;
    return !name.split(".").some((label) => label === "");
  }, "Enter a domain name, e.g. example.com");

const addZoneSchema = z
  .object({
    name: zoneNameSchema,
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
    // The server refuses a secondary with no primaries, and it is right to:
    // a secondary that names nowhere to pull from can never transfer, so it
    // would answer SERVFAIL for its whole suffix forever. Caught here only to
    // save the round trip, and phrased as the thing to type rather than as
    // "required".
    if (values.type === "secondary" && values.primaries.trim() === "") {
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
  tsig_key_id: "",
};

/** One declaration of the column geometry, shared by the header, the create
 * row and every zone row — matching the artboard's grid exactly. */
const GRID = "grid grid-cols-[1fr_108px_168px_116px_84px_116px_92px] items-center gap-3.5 px-4";

/** "17 days" — the expired-secondary warning's exact wording. lib/format.ts's
 * relativeTime abbreviates ("17d ago"); this line spells the unit out, so it
 * gets its own tiny formatter rather than a flag on the shared one. Only
 * ever fed a real refreshed_at (secondary zones only), so it doesn't need
 * relativeTime's "never"/zero-sentinel handling. */
function daysAgo(epochMs: number): string {
  const days = Math.max(0, Math.floor((Date.now() - epochMs) / 86_400_000));
  return `${days} ${days === 1 ? "day" : "days"}`;
}

/**
 * What the STATUS cell says, which for a secondary is a different question
 * from "is it enabled".
 *
 * A secondary holds its primary's data on loan. Two of its states — never
 * transferred, and expired — leave it answering SERVFAIL for its whole suffix
 * while `enabled` is still true in the database (Zone.Serving,
 * internal/zones/answer.go), so a cell that read `enabled` alone would call
 * such a zone healthy at exactly the moment it is serving nothing. The
 * artboard makes this the pulled zone's whole column, and it is right to: the
 * transfer state *is* the thing worth knowing about a zone that is a copy.
 *
 * Disabled is checked first, for every type. A zone the admin switched off is
 * not answering because they switched it off, and the scheduler skips it
 * entirely — reporting its transfer state instead would be pointing at a
 * consequence and calling it the cause.
 */
function zoneStatus(zone: Zone): { dot: string; text: string; label: string } {
  const muted = { dot: "bg-muted-foreground", text: "text-muted-foreground" };
  const bad = { dot: "bg-destructive", text: "font-semibold text-destructive-foreground" };
  const warn = { dot: "bg-warning", text: "font-semibold text-warning-foreground" };

  if (!zone.enabled) return { ...muted, label: "Disabled" };
  if (zone.type !== "secondary") return { dot: "bg-success", text: "", label: "Enabled" };

  switch (transferState(zone)) {
    // One label for both, because it is one fact: the zone holds nothing it
    // may speak for and answers SERVFAIL. *Which* of the two it is belongs in
    // the warning row below, which has room to say it.
    case "never":
    case "expired":
      return { ...bad, label: "Not answering" };
    // Not "Serving, transfer failing": the warning row below now ends in
    // "Still serving the copy from 31h ago.", which says the serving half
    // better than a clause in a status cell can — including *what* is being
    // served and how old it is. Overdue keeps its "Serving," because its own
    // warning row does not say it: nothing has transferred, and that sentence
    // is about the schedule rather than about what is being answered.
    case "failing":
      return { ...warn, label: "Transfer failing" };
    case "overdue":
      return { ...warn, label: "Serving, transfer overdue" };
    default:
      // Not "Enabled": for a copy, when it was last confirmed current is
      // strictly more than the fact that it is switched on.
      return { dot: "bg-success", text: "", label: `Refreshed ${relativeTime(zone.refreshed_at)}` };
  }
}

/**
 * The warning band under a secondary that is not in good standing, or null
 * when it is.
 *
 * The cause comes from the zone row (`last_error`), not from anything this
 * page remembers, which is what lets it be here at all: the scheduler's own
 * record of a failure is process-local, so a row built on that would go blank
 * after a restart for a zone that is still failing.
 *
 * It is deliberately its own value rather than a phrase glued to the front of
 * `note` with a dash. Concatenated, the transfer's own words and this page's
 * sentence about the consequence ran together as one long line with nothing
 * saying where the server stopped talking and the UI started — which for the
 * longest string on the row was exactly backwards. `lead` is what the row
 * shows and `full` is what it carries in `title`, so nothing the server said
 * is lost by shortening what is drawn (see transferErrorLead).
 */
function transferWarning(zone: Zone): {
  label: string;
  tone: "bad" | "warn";
  cause: { lead: string; full: string } | null;
  note: string;
} | null {
  if (zone.type !== "secondary" || !zone.enabled) return null;
  const from = zone.primaries || "unknown";
  const failure = lastTransferError(zone);
  const cause = failure
    ? { lead: transferErrorLead(failure.message), full: failure.message }
    : null;
  switch (transferState(zone)) {
    case "never":
      return {
        label: "Never transferred",
        tone: "bad",
        cause,
        note: `No transfer from ${from} has succeeded yet. Answering nothing.`,
      };
    case "expired":
      return {
        label: "Expired",
        tone: "bad",
        cause,
        note: `Transfer from ${from} last succeeded ${daysAgo(zone.refreshed_at)} ago, past the SOA expiry.`,
      };
    case "failing":
      return {
        label: "Last transfer",
        tone: "warn",
        cause,
        note: `Still serving the copy from ${relativeTime(zone.refreshed_at)}.`,
      };
    case "overdue":
      // No cause by definition: overdue is the state with nothing recorded
      // against it (see lib/zones.ts), so there is no error to lead with.
      return {
        label: "Overdue",
        tone: "warn",
        cause: null,
        note: `Nothing has transferred from ${from} since ${relativeTime(zone.refreshed_at)}, past the refresh the SOA asks for.`,
      };
    default:
      return null;
  }
}

/** The Records column: a per-zone request rather than a field on Zone
 * itself (the API has no record-count field — see api/types.ts). Same
 * shape as filtering/groups-clients.tsx's GroupListsCell, which reads a
 * per-group query the same way for the same reason: the list endpoint
 * doesn't carry it, and the count is still worth showing without turning
 * this page into a records browser. Grouped with `.toLocaleString()`,
 * unlike Serial beside it — the artboard draws that distinction
 * deliberately (a serial is an opaque counter, not a quantity). */
function RecordsCell({ zoneId }: { zoneId: number }) {
  const records = useZoneRecords(zoneId);
  return (
    <span className="text-right font-mono text-sm">
      {records.data ? records.data.length.toLocaleString() : "—"}
    </span>
  );
}

/**
 * Everything the create row shows only for a secondary: where to pull from,
 * and which key to sign the request with.
 *
 * The artboard puts these two in the STATUS and SERIAL columns — the columns
 * a zone that does not exist yet has nothing to put in anyway — so the row
 * keeps one grid rather than reflowing when the type changes.
 *
 * `type` is a plain prop rather than a `form.watch` of its own, so
 * list.test.tsx can render this cell directly with either type instead of
 * having to drive the select first.
 */
export function PrimariesField({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  // Secondary only. The artboard's `needsPrimaries` also covers stub and
  // forwarder, which is wrong twice over: neither is creatable (see
  // CREATABLE_TYPES), and a forwarder does not have primaries at all — it has
  // upstreams to ask, which is a different thing that will need its own field
  // when Milestone D6 brings it.
  if (type !== "secondary") return <span />;
  return (
    <FormField
      control={control}
      name="primaries"
      render={({ field }) => (
        <FormItem>
          <FormControl>
            <Input
              {...field}
              aria-label="Primary servers"
              placeholder="192.168.150.1:53"
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
 * The TSIG key select, in the SERIAL column and for a secondary only.
 *
 * Options are valued by **id**, labelled by name — see the schema's
 * `tsig_key_id` comment for why the artboard's name-valued select is a
 * silent-wrong-key bug rather than a style choice.
 *
 * Fetches the key list itself, so nothing is requested until a secondary is
 * actually being created. An instance with no keys still gets the select,
 * showing only "No TSIG": an unsigned transfer is a legitimate configuration
 * (a primary on a trusted LAN), so there is nothing here to guide anyone out
 * of, and a control that vanishes is a control someone goes looking for.
 */
export function TSIGKeyField({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  if (type !== "secondary") return <TSIGKeyFieldPlaceholder />;
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
 * filled in."), columns 3 and 4 for a secondary, under the two fields that
 * only it shows — matching the artboard exactly rather than pinning
 * everything to column 1. A field's validation error takes over its own
 * column when there is one.
 *
 * Same note as PrimariesField: `type` is a prop rather than read from the
 * form, so this cell can be rendered directly in a test.
 */
export function CreateRowHint({
  type,
  control,
  nameError,
  primariesError,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
  nameError: string | undefined;
  primariesError: string | undefined;
}) {
  const isSecondary = type === "secondary";
  return (
    <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
      <span>
        {nameError ? (
          <FormField control={control} name="name" render={() => <FormMessage />} />
        ) : !isSecondary ? (
          <span className="text-muted-foreground">SOA defaults are filled in.</span>
        ) : null}
      </span>
      <span />
      <span>
        {isSecondary &&
          (primariesError ? (
            <FormField control={control} name="primaries" render={() => <FormMessage />} />
          ) : (
            <span className="text-muted-foreground">Primary servers, comma separated.</span>
          ))}
      </span>
      <span>{isSecondary && <span className="text-muted-foreground">Optional.</span>}</span>
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

  const type = form.watch("type");
  const nameError = form.formState.errors.name?.message;
  const primariesError = form.formState.errors.primaries?.message;

  function onSubmit(values: AddZoneValues) {
    // primaries and tsig_key_id are sent for a secondary and omitted
    // otherwise, not sent empty: the server refuses either field on a zone
    // that never transfers, deliberately, rather than storing configuration
    // nothing will read (checkZoneTransferConfig in zones_handlers.go).
    const transfer =
      values.type === "secondary"
        ? {
            primaries: values.primaries.trim(),
            // "" means an unsigned transfer, which is 0 on the wire — and 0
            // is also what the server reads an omitted field as, so the key
            // is only named when one was actually chosen.
            ...(values.tsig_key_id === "" ? {} : { tsig_key_id: Number(values.tsig_key_id) }),
          }
        : {};
    createZone.mutate(
      {
        name: values.name.trim(),
        // Omitted at the default: handleZoneCreate already treats a
        // missing type as "primary" (zones_handlers.go), so there is no
        // need to say it explicitly in the common case.
        ...(values.type === "primary" ? {} : { type: values.type }),
        ...transfer,
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
                  {/* primary and secondary, which is the whole of what the
                      API accepts — see CREATABLE_TYPES. */}
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
          <PrimariesField type={type} control={form.control} />
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

        <CreateRowHint
          type={type}
          control={form.control}
          nameError={nameError}
          primariesError={primariesError}
        />
      </form>
    </Form>
  );
}

/**
 * One zone. `internal` zones are read-only — no delete, no edit — because
 * they are the RFC 6303 zones that stop junk queries (localhost, the
 * reverse-mapping arpa zones, …) reaching the root servers, seeded in a
 * later milestone rather than managed by hand. The Actions cell says so
 * outright (`BUILT-IN`) rather than merely omitting buttons, so an admin
 * scanning the column never wonders whether the buttons failed to render.
 */
function ZoneRow({ zone, onDelete }: { zone: Zone; onDelete: () => void }) {
  const isInternal = zone.type === "internal";
  const status = zoneStatus(zone);
  const warning = transferWarning(zone);
  const refreshZone = useRefreshZone();

  function onTransferNow() {
    refreshZone.mutate(zone.id, {
      onSuccess: () => toast.success(`${zone.name} transferred`),
      // The server's text names every primary it tried and why each refused
      // — see useRefreshZone. There is no band on this row to hold it, so it
      // goes to a toast rather than being thrown away for a shorter message.
      onError: (err) =>
        toast.error(err instanceof ApiError ? err.message : `Couldn't transfer ${zone.name}`),
    });
  }

  return (
    <div
      data-testid="zone-row"
      data-slot="zone-row"
      className={cn(
        "border-b border-border-muted",
        isInternal && "bg-muted/30 text-muted-foreground",
        // The warning below gets its own left-edge mark and tint — the same
        // "this needs a human" treatment idle filter lists get in
        // filtering/lists.tsx, on the row that contains the reason.
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
            {zone.type === "secondary" && (
              <ArrowDownToLine
                aria-label="Pulled from another server"
                className="size-3 shrink-0 text-muted-foreground"
              />
            )}
          </span>
        )}
        <span>
          <Badge variant={TYPE_VARIANT[zone.type]}>{zone.type}</Badge>
        </span>
        <span className="flex items-center gap-2">
          <span aria-hidden className={cn("size-1.5 shrink-0", status.dot)} />
          <span className={cn("text-sm", status.text)}>{status.label}</span>
        </span>
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
            <div className="flex items-center gap-1">
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label={`Edit ${zone.name}`}
                render={<Link to={`/zones/${zone.id}`} />}
              >
                <Pencil />
              </Button>
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label={`Delete ${zone.name}`}
                onClick={onDelete}
              >
                <Trash2 />
              </Button>
            </div>
          )}
        </span>
      </div>

      {warning && (
        <div className="flex items-center gap-2.5 px-4 pb-2.5">
          <span
            className={cn(
              "shrink-0 font-mono text-[9.5px] font-semibold tracking-[0.14em] uppercase",
              warning.tone === "bad" ? "text-destructive-foreground" : "text-warning-foreground",
            )}
          >
            {warning.label}
          </span>
          {/* The transfer's own words, and the one thing on this row with no
              bound on its length — so it is the only thing here that gives way
              when the row runs out of width. The whole message stays in
              `title`, and the zone's own page prints it in full. */}
          {warning.cause && (
            <span
              data-testid="zone-transfer-error"
              title={warning.cause.full}
              className={cn(
                "min-w-0 truncate font-mono text-[11.5px]",
                warning.tone === "bad" ? "text-destructive-foreground" : "text-warning-foreground",
              )}
            >
              {warning.cause.lead}
            </span>
          )}
          {/* Never shrinks: what the zone is doing about it is the part an
              operator scanning the list has to be able to read outright. */}
          <span data-testid="zone-transfer-note" className="shrink-0 text-xs text-muted-foreground">
            {warning.note}
          </span>
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto shrink-0"
            onClick={onTransferNow}
            disabled={refreshZone.isPending}
          >
            {refreshZone.isPending ? "Transferring…" : "Retry transfer"}
          </Button>
        </div>
      )}
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
          // Built-ins never take onDelete (ZoneRow omits the button
          // entirely for `internal` zones — see its own comment), so this
          // is never actually called.
          <ZoneRow key={zone.id} zone={zone} onDelete={() => {}} />
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
  // The Records column is the one thing here a transfer changes that the
  // zones response does not carry, so it follows the zone rather than
  // polling on its own — see useRecordsFollowTransfers.
  useRecordsFollowTransfers(zones.data);

  const [addOpen, setAddOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Zone | null>(null);

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
            <ZoneRow key={zone.id} zone={zone} onDelete={() => setDeleteTarget(zone)} />
          ))}
        {builtins.length > 0 && <BuiltinsDisclosure zones={builtins} />}
        {mine.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
            <p className="font-heading text-sm font-semibold">No zones yet</p>
            <p className="text-sm text-muted-foreground">Everything is forwarded upstream.</p>
            {!addOpen && (
              <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
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
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border bg-card px-4 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {mine.length} {mine.length === 1 ? "zone" : "zones"}
        </span>
        {!addOpen && (
          <Button type="button" size="sm" className="ml-auto" onClick={() => setAddOpen(true)}>
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
