import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import {
  AlertCircle,
  ArrowDownToLine,
  ChevronLeft,
  ChevronRight,
  Download,
  Lock,
  Pencil,
  Plus,
  RotateCw,
  Trash2,
  TriangleAlert,
  Upload,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
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
import type { Zone, ZoneRecord } from "../../api/types";
import {
  useCreateZoneRecord,
  useDeleteZone,
  useDeleteZoneRecord,
  useExportZoneFile,
  useImportZoneFile,
  useRecordsFollowTransfers,
  useRefreshZone,
  useUpdateZone,
  useUpdateZoneRecord,
  useZone,
  useZoneRecords,
  zoneFileErrors,
  type ZoneFileDiff,
  type ZoneRecordChange,
} from "../../hooks/use-zones";
import { useTSIGKeys } from "../../hooks/use-tsig-keys";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { formatBytes, formatDuration, relativeTime } from "../../lib/format";
import {
  isServing,
  lastTransferError,
  nextRefreshAt,
  retryIntervalMs,
  transferErrorLead,
  transferState,
} from "../../lib/zones";

// The nine record types the create/edit row's select offers — this list is
// the union dns.NewRR (the server's own validator, see
// internal/api/zonerecords_handlers.go's buildZoneRecord) round-trips for a
// hand-authored zone. rdata itself gets no per-type schema at all: see
// DATA_PLACEHOLDER's own comment for why.
const RECORD_TYPES = ["A", "AAAA", "CNAME", "TXT", "MX", "SRV", "NS", "CAA", "PTR"] as const;
type RecordType = (typeof RECORD_TYPES)[number];

/**
 * One text input serves every record type because the server validates
 * rdata with dns.NewRR — the same parser that builds the RR the resolver
 * actually serves — so a second, hand-written per-type form here would only
 * ever be a copy of that parser's rules, drifting the moment they change.
 * These placeholders are the form's only per-type guidance; exact strings
 * from the artboard, except TXT.
 *
 * TXT is quoted deliberately. Unquoted, rdata is a master-file line: spaces
 * separate character-strings and ';' opens a comment, so `v=spf1 -all`
 * stores two strings and a DKIM key pasted in raw silently truncates at the
 * first semicolon. Quoting is what makes a TXT value one literal string,
 * and the placeholder is where a user learns that.
 *
 * PTR's placeholder is a name, not an address — deliberately, since it's the
 * one type where that distinction bites: the *record's own name* is what
 * encodes the address (e.g. "10" under 150.168.192.in-addr.arpa), and the
 * rdata just names the host it points at. A placeholder shaped like an IP
 * would teach exactly the wrong thing and produce a value dns.NewRR rejects.
 */
const DATA_PLACEHOLDER: Record<RecordType, string> = {
  A: "192.168.150.28",
  AAAA: "fd00::28",
  CNAME: "bifrost.home.lan.",
  TXT: '"v=spf1 -all"',
  MX: "10 mail.example.com.",
  SRV: "0 5 5060 sip.home.lan.",
  NS: "ns1.home.lan.",
  CAA: '0 issue "letsencrypt.org"',
  PTR: "bifrost.e412.in.",
};

/**
 * The types whose rdata is a domain name, and so the ones whose Data field
 * carries the FULL NAME chip — exactly the artboard's set.
 *
 * The two columns read their names under different origins, and nothing on
 * screen used to say so. Name is relative to the zone ("bifrost" under
 * e412.in is bifrost.e412.in), but rdata is parsed by ToRR under *no*
 * origin (see buildZoneRecord in internal/api/zonerecords_handlers.go), so
 * a dotless target there is already absolute — "nas" is the name `nas.`,
 * not nas.e412.in. Cloudflare and Route 53 accept the dotless spelling and
 * resolve it against the zone, so that is the habit users arrive with.
 *
 * The chip is a label, not a rule: the server stores whatever either
 * spelling parses to, and both are valid records. Nothing here validates.
 */
const NAME_VALUED_TYPES = new Set<RecordType>(["CNAME", "MX", "NS", "PTR", "SRV"]);

/**
 * How much of the zone apex the Name field's suffix chip keeps when it
 * doesn't fit, per the artboard's own clip.
 *
 * The chip keeps the *head* and drops the tail: `.150.168.192…` rather than
 * `.…in-addr.arpa`. A chip is only useful if what survives it is the part
 * that says which zone this is, and for the apexes long enough to need
 * clipping at all — the IPv4 reverse zones — the tail is exactly the part
 * they all share. `in-addr.arpa` distinguishes nothing; the leading octets
 * are the whole of the zone's identity. The full apex stays reachable
 * either way: `title` for a pointer, and the field's accessible description
 * for everyone else (see the chip's own comment below).
 *
 * A character clip rather than CSS truncation because the string is joined
 * to a leading dot and the two have to be measured together; `max-w` on the
 * chip is still there as a backstop.
 */
const SUFFIX_VISIBLE_CHARS = 12;

function clipApex(apex: string): string {
  // At exactly one character over, the ellipsis costs as much as it saves.
  if (apex.length <= SUFFIX_VISIBLE_CHARS + 1) return apex;
  // A dot immediately before the ellipsis reads as a fourth, empty octet —
  // `.150.168.192.…` — so the cut moves back onto the label it lands after.
  return `${apex.slice(0, SUFFIX_VISIBLE_CHARS).replace(/\.$/, "")}…`;
}

/**
 * The create/edit row's two composite fields: a borderless input plus a
 * fixed chip, sharing one border and one focus ring so the pair reads as a
 * single control. The shell carries the box rnui's Input would normally
 * draw itself, which is why the input inside has to give its own back.
 */
const FIELD_SHELL = cn(
  "flex h-8 min-w-0 items-stretch rounded-lg border border-input bg-transparent transition-colors",
  "focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 dark:bg-input/30",
);

/** The invalid state moves to the shell with the border — same tones rnui's
 * Input uses for `aria-invalid`, so an errored Data field looks unchanged. */
const FIELD_SHELL_INVALID =
  "border-destructive ring-3 ring-destructive/20 dark:border-destructive/50 dark:ring-destructive/40";

/** Strips the box off the input so the shell above can draw it once. */
const FIELD_INPUT = cn(
  "h-full flex-1 rounded-none border-0 bg-transparent font-mono shadow-none dark:bg-transparent",
  "focus-visible:ring-0 aria-invalid:ring-0",
);

/** The muted chip both fields end in, divided from the input by a rule. */
const FIELD_CHIP =
  "flex shrink-0 items-center border-l border-input bg-muted px-2 font-mono whitespace-nowrap text-muted-foreground";

/** A category tag, not a verdict — exact mapping from the artboard. */
const RECORD_TYPE_VARIANT: Record<RecordType, NonNullable<BadgeProps["variant"]>> = {
  A: "primary-light",
  AAAA: "info-light",
  CNAME: "secondary",
  TXT: "warning-light",
  MX: "info-light",
  SRV: "secondary",
  NS: "primary-light",
  CAA: "warning-light",
  PTR: "secondary",
};

/** The header band's zone-type badge. Mirrors list.tsx's TYPE_VARIANT (kept
 * as a separate copy rather than imported — that map is scoped to the zones
 * list's own create-row narrowing comments, which don't apply here). */
const ZONE_TYPE_VARIANT: Record<Zone["type"], NonNullable<BadgeProps["variant"]>> = {
  primary: "primary-light",
  secondary: "info-light",
  stub: "secondary",
  forwarder: "warning-light",
  internal: "secondary",
};

/** One declaration of the column geometry, shared by the records header,
 * the create/edit row and every record row — matching the artboard's grid
 * exactly. TTL and Actions are the two right-aligned columns; Data is 1fr. */
const GRID = "grid grid-cols-[224px_96px_84px_1fr_92px] items-center gap-3.5 px-4";

/** RFC 2181 §8: the largest TTL a resolver reads back as typed, mirroring
 * maxRecordTTL in internal/api/zonerecords_handlers.go. */
const MAX_TTL = 2147483647;
/** The top of a uint32, mirroring zonePatch's SOA fields (internal/api/zones_handlers.go). */
const MAX_UINT32 = 4294967295;

const recordFormSchema = z.object({
  // Blank is a legitimate value — the server folds "" and the zone's own
  // name to "@" (normalizeRecordName) — so this is deliberately
  // unconstrained rather than "required".
  name: z.string(),
  type: z.enum(RECORD_TYPES),
  ttl: z
    .string()
    .trim()
    .regex(/^\d+$/, "TTL must be a whole number of seconds")
    .refine((v) => Number(v) <= MAX_TTL, "TTL must not exceed 2147483647"),
  // Deliberately no rdata rule: dns.NewRR is the one validator (see
  // DATA_PLACEHOLDER's comment) and its rejection is surfaced verbatim via
  // the mutation's onError below, not pre-empted by a client guess.
  rdata: z.string(),
});
type RecordFormValues = z.infer<typeof recordFormSchema>;

function recordFormDefaults(record: ZoneRecord | null): RecordFormValues {
  return {
    name: record?.name ?? "",
    type: (record?.type as RecordType | undefined) ?? "A",
    ttl: String(record?.ttl ?? 300),
    rdata: record?.rdata ?? "",
  };
}

/** Mono, with `@` and any wildcard name (`*`, `*.nexus`) picked out in
 * primary/600 — the two shapes worth scanning for in a long record list. */
function RecordName({ name }: { name: string }) {
  const highlighted = name === "@" || name.startsWith("*");
  return (
    <span
      className={cn(
        "truncate font-mono text-sm",
        highlighted ? "font-semibold text-primary" : "font-normal text-foreground",
      )}
      title={name}
    >
      {name}
    </span>
  );
}

/**
 * The record form — one control row on the records grid, in one of two
 * placements, and `editing` is what tells them apart.
 *
 * `null` is the add case (Add, POST): a new record has no row of its own to
 * become, so the form sits as a band between the grid header and the list.
 * Bound to a record it *is* that record's row (Save, PUT), rendered inside
 * the scrolling list in the row's own place — so the record is never on
 * screen twice, which is exactly what a band above the list made it.
 *
 * Only ever one instance is mounted; the call site enforces that (see
 * `addOpen`'s comment in ZoneDetail).
 *
 * Every mount is bound to one target for its whole life — the edit row is
 * keyed by record id, the add band by a counter the header's button bumps —
 * so no live form ever has to re-seed itself from a different record.
 * Mounting is also what drops a previous attempt's server error.
 *
 * The X closes the form in both placements — `onDone` does that at the call
 * site. A successful *edit* has nothing left to keep it open for, so it
 * closes the same way (`onDone` again). A successful *add* is the one
 * exception: the band re-clears itself and stays open instead of closing,
 * because adding one record is usually adding several, and forcing a
 * re-open for each one would make the common case the annoying one.
 */
function RecordFormRow({
  zoneId,
  apex,
  editing,
  onDone,
}: {
  zoneId: number;
  /** The zone's own name, shown beside Name so the field reads as the
   * subdomain it actually is. Passed in rather than re-fetched: the row is
   * only ever mounted from a loaded zone. */
  apex: string;
  editing: ZoneRecord | null;
  onDone: () => void;
}) {
  const createRecord = useCreateZoneRecord();
  const updateRecord = useUpdateZoneRecord();
  const form = useForm<RecordFormValues>({
    resolver: zodResolver(recordFormSchema),
    defaultValues: recordFormDefaults(editing),
  });
  const isEdit = editing !== null;
  const busy = createRecord.isPending || updateRecord.isPending;

  // Focus Name on mount. Seeding is `defaultValues` above rather than a
  // reset here, because a mount is bound to one target for its whole life
  // (see this component's own comment) — there is no "the target changed
  // underneath me" case left to handle.
  //
  // The focus is also what keeps an edited row on screen: it lives in the
  // scrolling list now, so the browser scrolls it into view the moment it
  // opens. See ZoneDetail's `editingId` comment.
  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  const type = form.watch("type");

  // At "@" the zone *is* the name, so the chip carries the apex whole and
  // the typed text goes muted — the field is saying "there is nothing left
  // for you to add". Anywhere else it carries the apex with its joining
  // dot, and what's typed reads as the subdomain in front of it. A wildcard
  // ("*", "*.nexus") is an ordinary relative name and gets no special case.
  const atApex = form.watch("name").trim() === "@";
  const suffixFull = atApex ? apex : `.${apex}`;
  const suffixShown = atApex ? clipApex(apex) : `.${clipApex(apex)}`;
  const dataIsName = NAME_VALUED_TYPES.has(type);
  const fieldId = useId();
  const nameSuffixId = `${fieldId}-name-suffix`;
  const dataSuffixId = `${fieldId}-data-suffix`;

  function onSubmit(values: RecordFormValues) {
    const payload = {
      name: values.name.trim(),
      type: values.type,
      ttl: Number(values.ttl.trim()),
      rdata: values.rdata.trim(),
    };
    // The server's own text (dns.NewRR's parser message, or an RFC-conflict
    // message) is more precise than anything this form could invent — it
    // lands on the rdata field itself rather than a toast, aria-invalid and
    // all, via setError below.
    const onError = (err: unknown) => {
      const message =
        err instanceof ApiError ? err.message : `Couldn't ${isEdit ? "update" : "add"} the record`;
      form.setError("rdata", { type: "server", message });
    };
    if (isEdit) {
      updateRecord.mutate(
        { zoneId, id: editing.id, ...payload },
        {
          onSuccess: () => {
            toast.success("Record updated");
            onDone();
          },
          onError,
        },
      );
      return;
    }
    createRecord.mutate(
      { zoneId, ...payload },
      {
        onSuccess: () => {
          toast.success("Record added");
          form.reset(recordFormDefaults(null));
          form.setFocus("name");
        },
        onError,
      },
    );
  }

  // name and rdata carry no client-side rule (see recordFormSchema's own
  // comments) so their FormMessage is always null in practice; type and ttl
  // do, and — per Task 12's fix round 1 — a zod failure must never be a
  // silent no-op: the button blocking submit with nothing on screen to say
  // why. rdata's *server* error still gets its own distinct rendering below
  // (mono, alert-circle) rather than a plain FormMessage, since that one is
  // the DNS parser's own text, not a client-authored message.
  const nameError = form.formState.errors.name?.message;
  const typeError = form.formState.errors.type?.message;
  const ttlError = form.formState.errors.ttl?.message;
  const rdataError = form.formState.errors.rdata?.message;
  const hasRowError = Boolean(nameError || typeError || ttlError || rdataError);

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="zone-record-form-row"
        data-testid="record-form-row"
        // One treatment for both placements, because it is one control: the
        // card background, the 3px primary edge and a full-strength bottom
        // border. In the list — where the neighbouring rows are transparent
        // and ruled with border-muted — those three are what mark the row
        // out as the one being written.
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                {/* The shell sits inside FormItem rather than replacing it,
                    so FormControl still slots onto the input itself — the
                    id, the label association and aria-invalid all stay
                    where a form control's belong. */}
                <div className={FIELD_SHELL}>
                  <FormControl>
                    <Input
                      {...field}
                      aria-label="Zone record name"
                      // The suffix is context, not part of the value being
                      // typed, so it reaches assistive tech as this field's
                      // *description* — the accessible name is untouched.
                      // This replaces FormControl's own describedby, which
                      // points at a FormDescription this row never renders.
                      aria-describedby={nameSuffixId}
                      placeholder="@ or bifrost"
                      autoComplete="off"
                      className={cn(FIELD_INPUT, atApex && "text-muted-foreground")}
                    />
                  </FormControl>
                  {/* Hidden from assistive tech because what it shows may be
                      clipped; the unclipped apex is next to it, and that is
                      what gets announced. `title` is the pointer's copy of
                      the same thing. */}
                  <span
                    data-testid="zone-name-suffix"
                    aria-hidden="true"
                    title={suffixFull}
                    className={cn(FIELD_CHIP, "max-w-[126px] overflow-hidden text-xs")}
                  >
                    {suffixShown}
                  </span>
                  <span id={nameSuffixId} className="sr-only">
                    {suffixFull}
                  </span>
                </div>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="type"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <NativeSelect {...field} aria-label="Record type">
                    {RECORD_TYPES.map((t) => (
                      <NativeSelectOption key={t} value={t}>
                        {t}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="ttl"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input {...field} aria-label="TTL" inputMode="numeric" />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="rdata"
            render={({ field }) => (
              <FormItem>
                <div className={cn(FIELD_SHELL, rdataError && FIELD_SHELL_INVALID)}>
                  <FormControl>
                    <Input
                      {...field}
                      onChange={(e) => {
                        field.onChange(e);
                        // A fresh attempt deserves a fresh read of the parser,
                        // not last submit's stale rejection sitting under it.
                        if (form.formState.errors.rdata) form.clearErrors("rdata");
                      }}
                      aria-label="Data"
                      // Described by the chip itself, not a hidden twin: this
                      // one is never clipped, so its own text is the whole of
                      // what it says. Types without the chip fall back to no
                      // description at all.
                      aria-describedby={dataIsName ? dataSuffixId : undefined}
                      placeholder={DATA_PLACEHOLDER[type]}
                      autoComplete="off"
                      className={FIELD_INPUT}
                    />
                  </FormControl>
                  {dataIsName && (
                    <span
                      id={dataSuffixId}
                      data-testid="record-data-suffix"
                      className={cn(FIELD_CHIP, "text-xs tracking-widest uppercase")}
                    >
                      Full name
                    </span>
                  )}
                </div>
              </FormItem>
            )}
          />
          <div className="flex items-center justify-end gap-1">
            <Button type="submit" size="sm" disabled={busy}>
              {busy ? "Saving…" : isEdit ? "Save" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label={isEdit ? "Cancel editing" : "Close"}
              onClick={() => {
                form.reset(recordFormDefaults(null));
                onDone();
              }}
            >
              <X />
            </Button>
          </div>
        </div>

        {/* The error line: each column's own message sits under it, mirroring
            list.tsx's CreateRowHint (see its own comment). When the only
            problem is a rejected Data value — the common case, per the
            artboard — columns 1-3 stay empty and this is exactly the
            artboard's "second line under the DATA column only" state. A
            client-side name/type/ttl failure is not a silent no-op: it has
            to land somewhere, or the Add/Save button just looks dead. */}
        {hasRowError && (
          <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
            {/* Each cell is a real element even when its column has nothing
                to say, because FormMessage renders *null* when there is no
                error. Left bare, the three quiet columns would contribute no
                grid children at all and auto-placement would slide Data's
                error left into the Name column — which is where it used to
                land, the common case being a rejected Data value and nothing
                else. The span is what holds the column open. */}
            <span>
              <FormField control={form.control} name="name" render={() => <FormMessage />} />
            </span>
            <span>
              <FormField control={form.control} name="type" render={() => <FormMessage />} />
            </span>
            <span>
              <FormField control={form.control} name="ttl" render={() => <FormMessage />} />
            </span>
            {/* Data's error is the DNS parser's own output (or the server's
                RFC-conflict text) — rendered distinctly, mono with an
                alert-circle icon, rather than as a plain FormMessage; see
                the artboard's own note on not rewriting parser errors. */}
            <span className="flex items-start gap-1.5 text-destructive-foreground">
              {rdataError && (
                <>
                  <AlertCircle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                  <span className="font-mono">{rdataError}</span>
                </>
              )}
            </span>
            <span />
          </div>
        )}
      </form>
    </Form>
  );
}

function RecordRow({
  record,
  readOnly,
  actionsInert,
  onEdit,
  onDelete,
}: {
  record: ZoneRecord;
  readOnly: boolean;
  /** Some other row is being edited. One row is written at a time, so this
   * row's actions stay where they are — pulling them out would make the
   * whole list jump — but recede and stop working. */
  actionsInert: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const variant = RECORD_TYPE_VARIANT[record.type as RecordType] ?? "secondary";
  const label = `${record.name} ${record.type} ${record.rdata}`;
  return (
    <div data-testid="zone-record-row" className={cn(GRID, "border-b border-border-muted py-2")}>
      <RecordName name={record.name} />
      <span>
        <Badge variant={variant}>{record.type}</Badge>
      </span>
      <span className="text-right font-mono text-sm text-muted-foreground">
        {record.ttl.toLocaleString()}
      </span>
      <span className="truncate font-mono text-sm" title={record.rdata}>
        {record.rdata}
      </span>
      {/* A built-in zone's records are readable but not editable — the
          server 409s every write to one (see ZoneDetail's own comment), so
          these icons are omitted rather than left to fail on click. */}
      {!readOnly && (
        /* While another row is being edited, the artboard fades these to 0.3
           and turns pointer events off. Real `disabled` is what actually
           delivers that: pointer-events alone still lets a keyboard press
           activate a focused button, and tells assistive tech nothing.
           `disabled:opacity-30` is the artboard's own fade, in place of
           rnui's default 0.5 — the base class already brings
           `disabled:pointer-events-none` with it. */
        <div className="flex items-center justify-end gap-1">
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Edit ${label}`}
            className="disabled:opacity-30"
            disabled={actionsInert}
            onClick={onEdit}
          >
            <Pencil />
          </Button>
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Delete ${label}`}
            className="disabled:opacity-30"
            disabled={actionsInert}
            onClick={onDelete}
          >
            <Trash2 />
          </Button>
        </div>
      )}
    </div>
  );
}

const soaFieldSchema = z
  .string()
  .trim()
  .regex(/^\d+$/, "Whole number of seconds")
  .refine((v) => Number(v) <= MAX_UINT32, "Too large");

const soaFormSchema = z.object({
  soa_ns: z.string().trim().min(1, "Required"),
  soa_mbox: z.string().trim().min(1, "Required"),
  soa_refresh: soaFieldSchema,
  soa_retry: soaFieldSchema,
  soa_expire: soaFieldSchema,
  soa_minimum: soaFieldSchema,
});
type SoaFormValues = z.infer<typeof soaFormSchema>;

function soaDefaults(zone: Zone): SoaFormValues {
  return {
    soa_ns: zone.soa_ns,
    soa_mbox: zone.soa_mbox,
    soa_refresh: String(zone.soa_refresh),
    soa_retry: String(zone.soa_retry),
    soa_expire: String(zone.soa_expire),
    soa_minimum: String(zone.soa_minimum),
  };
}

/** Label + optional unit above a control — the SOA grid's own field shape,
 * per the artboard's Label/Unit/Control table. Not a `<label for>`: each
 * control already carries its own `aria-label` (below), so this is purely
 * the visible caption rather than a second, unassociated accessible name. */
function SoaFieldShell({
  label,
  unit,
  children,
}: {
  label: string;
  unit?: string;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs text-muted-foreground">
        {label}
        {unit && <span className="ml-1 text-muted-foreground/70">{unit}</span>}
      </span>
      {children}
    </div>
  );
}

/**
 * The SOA band — collapsible, collapsed by default. Closed, it's one
 * summary line; open, it's the 7-field grid the artboard specifies. Serial
 * is shown but not editable: the box holds the actual number (the server
 * owns it — it bumps on every record write via bumpZoneSerial) with an
 * `AUTO` marker pinned to the right edge, the design's way of saying "you
 * can read this, but it's not yours to type into".
 */
function SoaBand({ zone }: { zone: Zone }) {
  const [open, setOpen] = useState(false);
  const updateZone = useUpdateZone();
  const form = useForm<SoaFormValues>({
    resolver: zodResolver(soaFormSchema),
    defaultValues: soaDefaults(zone),
  });

  // Reset only when the *saved* SOA fields actually change (initial load,
  // or the refetch that follows a successful save) — not on every
  // unrelated zone refetch (e.g. one triggered by a record write bumping
  // soa_serial), which would otherwise wipe an in-progress, unsaved edit.
  // Destructured to primitives rather than depending on `zone` itself,
  // whose object identity changes on every refetch regardless of whether
  // any of these particular fields did.
  const { soa_ns, soa_mbox, soa_refresh, soa_retry, soa_expire, soa_minimum } = zone;
  useEffect(() => {
    form.reset({
      soa_ns,
      soa_mbox,
      soa_refresh: String(soa_refresh),
      soa_retry: String(soa_retry),
      soa_expire: String(soa_expire),
      soa_minimum: String(soa_minimum),
    });
  }, [soa_ns, soa_mbox, soa_refresh, soa_retry, soa_expire, soa_minimum, form]);

  function onSubmit(values: SoaFormValues) {
    updateZone.mutate(
      {
        id: zone.id,
        soa_ns: values.soa_ns.trim(),
        soa_mbox: values.soa_mbox.trim(),
        soa_refresh: Number(values.soa_refresh),
        soa_retry: Number(values.soa_retry),
        soa_expire: Number(values.soa_expire),
        soa_minimum: Number(values.soa_minimum),
      },
      {
        onSuccess: () => toast.success("SOA saved"),
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't save the SOA"),
      },
    );
  }

  const summary = `${zone.soa_ns} · ${zone.soa_mbox} · ${zone.soa_refresh}/${zone.soa_retry}/${zone.soa_expire}/${zone.soa_minimum}`;

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        className="shrink-0 border-b border-border bg-card"
      >
        <Collapsible open={open} onOpenChange={setOpen}>
          <div className="flex items-center gap-2.5 px-4 py-2">
            <CollapsibleTrigger
              type="button"
              // gap-1 matches the header's back link above, so the SOA label
              // and "Zones" start at the same x. Both rows are px-4 with a
              // size-3.5 chevron, so the gap is the only thing that can
              // misalign them. cursor-pointer explicitly: Tailwind v4
              // preflight sets no cursor on <button>.
              className="flex min-w-0 flex-1 cursor-pointer items-center gap-1 text-left"
            >
              <ChevronRight
                aria-hidden="true"
                className={cn(
                  "size-3.5 shrink-0 text-muted-foreground transition-transform",
                  open && "rotate-90",
                )}
              />
              <span className="shrink-0 font-mono text-[9.5px] tracking-[0.14em] text-muted-foreground uppercase">
                SOA
              </span>
              {!open && (
                <span className="truncate font-mono text-xs text-muted-foreground">{summary}</span>
              )}
            </CollapsibleTrigger>
            {open && (
              <Button
                type="submit"
                size="sm"
                variant="ghost"
                disabled={!form.formState.isDirty || updateZone.isPending}
              >
                {updateZone.isPending ? "Saving…" : "Save SOA"}
              </Button>
            )}
          </div>
          <CollapsibleContent>
            <div className="grid grid-cols-4 gap-x-[18px] gap-y-4 px-4 pb-4">
              <FormField
                control={form.control}
                name="soa_ns"
                render={({ field }) => (
                  <SoaFieldShell label="Primary NS">
                    <Input {...field} aria-label="Primary NS" className="font-mono" />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
              <FormField
                control={form.control}
                name="soa_mbox"
                render={({ field }) => (
                  <SoaFieldShell label="Responsible">
                    <Input {...field} aria-label="Responsible" className="font-mono" />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
              {/* Not editable, but the value IS shown (Task 12 fix round 1:
                  an earlier draft of the artboard notes said only "AUTO",
                  omitting the number — corrected). The serial itself is the
                  box's content; AUTO is pushed to the right edge as an
                  annotation that the server, not this form, owns the number. */}
              <SoaFieldShell label="Serial">
                <div className="flex h-8 items-center rounded-lg border border-input bg-muted px-2.5">
                  <span className="font-mono text-sm text-muted-foreground">{zone.soa_serial}</span>
                  <span className="ml-auto font-mono text-[9.5px] tracking-[0.14em] text-muted-foreground/70 uppercase">
                    AUTO
                  </span>
                </div>
              </SoaFieldShell>
              <FormField
                control={form.control}
                name="soa_refresh"
                render={({ field }) => (
                  <SoaFieldShell label="Refresh" unit="s">
                    <Input
                      {...field}
                      aria-label="Refresh"
                      inputMode="numeric"
                      className="font-mono"
                    />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
              <FormField
                control={form.control}
                name="soa_retry"
                render={({ field }) => (
                  <SoaFieldShell label="Retry" unit="s">
                    <Input
                      {...field}
                      aria-label="Retry"
                      inputMode="numeric"
                      className="font-mono"
                    />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
              <FormField
                control={form.control}
                name="soa_expire"
                render={({ field }) => (
                  <SoaFieldShell label="Expire" unit="s">
                    <Input
                      {...field}
                      aria-label="Expire"
                      inputMode="numeric"
                      className="font-mono"
                    />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
              <FormField
                control={form.control}
                name="soa_minimum"
                render={({ field }) => (
                  <SoaFieldShell label="Minimum" unit="s">
                    <Input
                      {...field}
                      aria-label="Minimum"
                      inputMode="numeric"
                      className="font-mono"
                    />
                    <FormMessage />
                  </SoaFieldShell>
                )}
              />
            </div>
          </CollapsibleContent>
        </Collapsible>
      </form>
    </Form>
  );
}

/**
 * The transfer band — everything about a secondary that a primary's SOA band
 * would have said, plus the two things only a copy has: where it comes from
 * and when it was last confirmed current.
 *
 * It *replaces* the SOA band for a secondary rather than sitting beside it,
 * per the artboard, and the reason is not space: a secondary's SOA is its
 * primary's, arriving with each transfer and overwritten by the next one, so
 * an editable SOA form here would offer to write values that the next
 * transfer silently discards.
 *
 * The last transfer error is shown verbatim, as the artboard draws it, and it
 * is read off the zone row rather than remembered from a request this page
 * made. That distinction is the whole of why `zones.last_error` exists: the
 * scheduler's own record of a failed attempt is process-local
 * (zones.Refresher.Status), so a page built on it would show a zone that had
 * been failing for a week as healthy until its next attempt after a restart.
 * The column is written and cleared by the scheduler on every attempt, so
 * what is on screen here is the last thing that actually happened — whether
 * it happened because of the schedule or because someone pressed the button
 * above, and whether or not this browser was open at the time.
 *
 * It is never shown without its date. `last_attempt` is what makes the
 * difference between "connection refused" four minutes ago and the same words
 * four days ago, and only one of those is worth acting on now.
 */
function TransferBand({ zone }: { zone: Zone }) {
  const keys = useTSIGKeys();
  const state = transferState(zone);
  const failure = lastTransferError(zone);
  /**
   * A disabled zone is outside all of this, and has to be checked before any
   * of it — the same order the zones list checks it in.
   *
   * The scheduler skips a disabled zone outright (RefreshDue in
   * internal/zones/refresh.go), and the resolver skips it again at answer time
   * (Index.Find). So for one of these, *nothing is retrying* and *nothing is
   * being served*, and a band that reported its transfer state would say both
   * — while the badge one row above said Disabled.
   *
   * Its transfer state is still true and still shown: primaries, key, serial
   * and the last refresh are all facts, and a recorded failure is a dated
   * account of the last thing that actually happened. What changes is the two
   * forward-looking claims, which are consequences of the switch rather than
   * of the transfer.
   */
  const disabled = !zone.enabled;
  const serving = !disabled && state !== "never" && state !== "expired";

  // The key's own name, which is what the peer's config calls it and so the
  // only useful thing to show. An id that resolves to nothing — a key deleted
  // out from under the zone, which the API refuses but a hand-written row
  // could still produce — says so rather than rendering a bare number.
  let tsigLabel = "none";
  if (zone.tsig_key_id !== 0) {
    const key = keys.data?.find((k) => k.id === zone.tsig_key_id);
    tsigLabel = key ? key.name : `#${zone.tsig_key_id} (missing)`;
  }

  /**
   * When the schedule next comes round.
   *
   * A failing zone gets the interval rather than a moment, and that is a
   * limit rather than a preference: the exact time of the next retry lives in
   * the scheduler's in-memory back-off, so naming one would be a guess. The
   * interval itself is the zone's own soa_retry, clamped by the same floor
   * the scheduler clamps it by, and is true whenever the process is running.
   */
  let nextRefresh: string;
  if (disabled) {
    // Not "due now", and not an interval: the scheduler will not look at this
    // zone at all until it is enabled again.
    nextRefresh = "not while disabled";
  } else if (state === "never") {
    nextRefresh = "due now";
  } else if (state === "fresh") {
    nextRefresh = `in ${formatDuration(nextRefreshAt(zone) - Date.now())}`;
  } else {
    nextRefresh = `retrying every ${formatDuration(retryIntervalMs(zone))}`;
  }

  // A disabled zone's transfer state is a consequence of the switch, so it is
  // reported plainly rather than in the colours of a problem to solve — the
  // same reasoning that makes the zones list show it as a muted "Disabled"
  // instead of "Expired".
  const bad = !disabled && (state === "never" || state === "expired");
  const warn = !disabled && (state === "failing" || state === "overdue");

  const fields: { label: string; value: string; strong?: boolean; tone?: string }[] = [
    { label: "Primaries", value: zone.primaries || "none" },
    { label: "TSIG key", value: tsigLabel },
    { label: "Serial", value: String(zone.soa_serial) },
    {
      label: "Last refresh",
      value: relativeTime(zone.refreshed_at),
      strong: !disabled && state !== "fresh",
      tone: bad ? "text-destructive-foreground" : warn ? "text-warning-foreground" : undefined,
    },
    {
      label: "Next refresh",
      value: nextRefresh,
      tone: bad || warn ? "text-warning-foreground" : "text-muted-foreground",
    },
  ];

  // What the zone is doing about it right now, stated as a consequence rather
  // than a diagnosis — the cause is the error beside it, in the server's own
  // words.
  const note = disabled
    ? // Not "answers nothing", which is the one behaviour a disabled zone does
      // not have. Index.Find skips it entirely (internal/zones/zone.go), so
      // the zone stops being *consulted* rather than starting to refuse, and
      // names under it fall through to the forwarder — which for a
      // split-horizon zone means an internal name is now resolved by a public
      // server, the very leak the zone was holding closed. That is worth a
      // sentence, and "answers nothing" would have hidden it.
      "This zone is disabled: nothing transfers, and names under it are forwarded upstream."
    : serving
      ? `Still serving the copy from ${relativeTime(zone.refreshed_at)}.`
      : state === "never"
        ? "Answering nothing until the first transfer succeeds."
        : "Answering nothing until a transfer succeeds.";
  // "LAST ATTEMPT · 4m ago" when there is a failure to date, and otherwise the
  // reason there is nothing to date: the zone is switched off, or it is behind
  // with nothing recorded against it (a process that has only just started).
  const noteLabel = failure
    ? `Last attempt · ${relativeTime(failure.at)}`
    : disabled
      ? // Not "Disabled": the badge one row above already says that, and a
        // label that repeats it wastes the one line this band has for saying
        // what the *transfers* are doing.
        "Not transferring"
      : state === "overdue"
        ? "Overdue"
        : state;
  // A disabled zone always explains itself, whatever its transfer state:
  // "nothing is happening here" is the one thing the five fields above cannot
  // say on their own.
  const showNote = disabled || state !== "fresh";
  // One tone for the whole note line, so the icon, the label, the error and
  // the tint cannot disagree. Muted is the disabled case and is deliberately
  // neither of the alarm colours: nothing here needs fixing.
  const noteTone = bad
    ? "text-destructive-foreground"
    : warn
      ? "text-warning-foreground"
      : "text-muted-foreground";
  // What to lead the error line with. A verbatim tail of the message, never a
  // rewrite of it, and equal to the whole message whenever the message has no
  // shape to take a tail from — see transferErrorLead.
  const lead = failure ? transferErrorLead(failure.message) : "";

  return (
    <div
      className={cn(
        "shrink-0 border-b border-border",
        bad
          ? "bg-destructive/5 shadow-[inset_3px_0_0_var(--destructive)]"
          : warn
            ? "bg-warning/5 shadow-[inset_3px_0_0_var(--warning)]"
            : "bg-card",
      )}
    >
      <div className="grid grid-cols-5 gap-[18px] px-4 py-3">
        {fields.map((field) => (
          <div key={field.label} className="flex min-w-0 flex-col gap-1">
            <span className="font-mono text-[9.5px] font-semibold tracking-[0.14em] text-muted-foreground uppercase">
              {field.label}
            </span>
            <span
              className={cn(
                "truncate font-mono text-sm",
                field.strong && "font-semibold",
                field.tone,
              )}
              title={field.value}
            >
              {field.value}
            </span>
          </div>
        ))}
      </div>
      {/* Two lines on one grid rather than one flex row of four things, and
          the reason is that the error is the longest string on the page and
          was getting the least structure: it ran straight into the neutral
          note beside it with nothing between them, and neither line shared a
          left edge with anything. The icon now has a column of its own, so
          both lines start where the fields above do, and the rule closes the
          five-column grid off rather than letting the error look like a sixth
          field. */}
      {showNote && (
        <div className="grid grid-cols-[14px_1fr] gap-x-2 border-t border-border-muted px-4 pt-[9px] pb-2.5">
          <AlertCircle aria-hidden="true" className={cn("mt-0.5 size-3.5", noteTone)} />
          <div className="flex min-w-0 flex-col gap-0.5">
            {/* Wraps rather than overflows: the note is short and authored
                here, the error is not, so when the two cannot share a line the
                note drops to its own rather than being pushed off the end. */}
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2.5">
              <span
                className={cn(
                  "shrink-0 font-mono text-[9.5px] font-semibold tracking-[0.14em] uppercase",
                  noteTone,
                )}
              >
                {noteLabel}
              </span>
              {/* The innermost part of the transfer's own words — see
                  transferErrorLead. Not a rewrite of the error: it is a
                  verbatim tail of it, and the whole message is on the line
                  below (and in `title` here) whenever the two differ. Leading
                  with it is what makes a truncated error still worth reading,
                  since the part that says what went wrong is the part an
                  ellipsis at the end would eat first. */}
              {failure && (
                <span
                  data-testid="transfer-error"
                  title={failure.message}
                  className={cn("min-w-0 truncate font-mono text-sm font-semibold", noteTone)}
                >
                  {lead}
                </span>
              )}
              {/* Never shrinks. What the zone is doing about it is the one
                  thing on this line that an operator has to be able to read
                  without hovering anything, and an unbounded error string is
                  exactly what would have crowded it out. */}
              <span
                data-testid="transfer-note"
                className="shrink-0 text-[12.5px] text-muted-foreground"
              >
                {note}
              </span>
            </div>
            {/* The message in full, dim, on its own line — omitted when the
                lead already is the whole of it, since a short error would
                otherwise be printed twice. */}
            {failure && lead !== failure.message && (
              <span
                data-testid="transfer-error-raw"
                title={failure.message}
                className="truncate font-mono text-[11px] text-muted-foreground/70"
              >
                {failure.message}
              </span>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

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
 */
type ImportPhase =
  | { kind: "idle" }
  | { kind: "checking"; file: FileMeta }
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
function ZoneFileActions({ zone }: { zone: Zone }) {
  const [phase, setPhase] = useState<ImportPhase>({ kind: "idle" });
  const fileInput = useRef<HTMLInputElement>(null);
  const exportFile = useExportZoneFile();
  const importFile = useImportZoneFile();

  // The two kinds of zone this server refuses writes into: the RFC 6303
  // built-ins (seeded infrastructure) and a secondary (someone else's zone,
  // on loan). Both 409 an import, so Import is omitted rather than left to
  // fail on click — and for a secondary an import is worse than refused, it
  // is meaningless: the next transfer would replace whatever it wrote.
  // Export is offered for both: reading either is allowed.
  const canImport = zone.type !== "internal" && zone.type !== "secondary";

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
    setPhase({ kind: "checking", file: meta });

    void picked.text().then(
      (content) => {
        importFile.mutate(
          { id: zone.id, content, dryRun: true },
          {
            onSuccess: (diff) => setPhase({ kind: "diff", file: { ...meta, content }, diff }),
            onError: (err) =>
              setPhase({ kind: "rejected", file: meta, errors: zoneFileErrors(err) }),
          },
        );
      },
      () => {
        setPhase({ kind: "idle" });
        toast.error(`Couldn't read ${picked.name}`);
      },
    );
  }

  function onApply() {
    if (phase.kind !== "diff") return;
    const { file } = phase;
    importFile.mutate(
      { id: zone.id, content: file.content, dryRun: false },
      {
        onSuccess: (diff) => {
          toast.success(
            `Zone replaced — ${diff.add.length} added, ${diff.change.length} changed, ${diff.delete.length} deleted.`,
          );
          setPhase({ kind: "idle" });
        },
        // A file that passed the dry run can still be refused now: the zone
        // may have moved underneath it. The same rejected view says so.
        onError: (err) => setPhase({ kind: "rejected", file, errors: zoneFileErrors(err) }),
      },
    );
  }

  const applying = importFile.isPending && phase.kind === "diff";

  return (
    <>
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
          if (!open) setPhase({ kind: "idle" });
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
              <ImportFooter
                applyLabel="Checking…"
                applyDisabled
                onCancel={() => setPhase({ kind: "idle" })}
              />
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
                onCancel={() => setPhase({ kind: "idle" })}
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
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  onClick={() => setPhase({ kind: "idle" })}
                >
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

/**
 * Zone detail — records, SOA editing, and the zone-level actions (enable
 * toggle, delete) the list page (Task 11) deliberately left off its own
 * rows. GET/PATCH/DELETE /zones/{id} and the four /zones/{id}/records
 * endpoints, via use-zones.ts's hooks. Every record write bumps the zone's
 * SOA serial server-side; useZoneRecords' mutations already invalidate the
 * zone query, so the SOA band and header badges never go stale after one.
 */
export function ZoneDetail() {
  const params = useParams<{ id: string }>();
  const zoneId = Number(params.id);
  const navigate = useNavigate();

  const zone = useZone(zoneId);
  const records = useZoneRecords(zoneId);
  // A transfer replaces the whole record set, and the zone query is what
  // notices one landed — whether it was the scheduler's or this page's own
  // Refresh now. The record list is never polled; see
  // useRecordsFollowTransfers.
  useRecordsFollowTransfers(zone.data);
  const updateZone = useUpdateZone();
  const deleteZone = useDeleteZone();
  const deleteRecord = useDeleteZoneRecord();

  const [nameFilter, setNameFilter] = useState("");
  const [typeFilter, setTypeFilter] = useState<"" | RecordType>("");
  /**
   * The record whose own row in the list has become the form. Null means no
   * row is being edited.
   *
   * Not "the form is open, bound to a record": in the edit case the form has
   * no position of its own, it takes the record's. Nothing scrolls or jumps,
   * and the record is not on screen twice. The Save button therefore rides
   * with the row — it goes wherever the row is, including out of the
   * viewport if the list is scrolled. Opening an edit focuses Name, which
   * scrolls the row into view; nothing pins Save after that, and no sticky
   * action bar was added, because the artboard has no such element and the
   * row is where the record's own context is.
   *
   * An id rather than the record, because the record itself is read from the
   * list at render time — always the freshest copy, and one place for it to
   * live rather than two.
   */
  const [editingId, setEditingId] = useState<number | null>(null);
  /**
   * Whether the add band — the form in its other placement, above the list
   * — is mounted. Closed on load, so nothing is on screen until Add record
   * opens it.
   *
   * This and `editingId` are two different things and must stay so: one is a
   * band at the top, the other is a row in the list. They are also mutually
   * exclusive, because two live forms would put two "Zone record name"
   * fields in the document. `openAdd` and `openEdit` below are the only two
   * ways in, and each closes the other.
   */
  const [addOpen, setAddOpen] = useState(false);
  /** The add band's React key, bumped on every Add record press. Pressing
   * the button while the band is already open has to clear it, refocus Name
   * and drop any server error — a remount is all three at once, and a
   * still-mounted form would do none of them on its own. */
  const [addCue, setAddCue] = useState(0);
  const [deleteZoneOpen, setDeleteZoneOpen] = useState(false);
  const [deleteRecordTarget, setDeleteRecordTarget] = useState<ZoneRecord | null>(null);
  const refreshZone = useRefreshZone();

  /**
   * Ask for a transfer now.
   *
   * Nothing is kept about the failure here on purpose. A failed transfer
   * writes `last_error`/`last_attempt` onto the zone row and the mutation
   * refetches it either way (see useRefreshZone), so the band below is
   * already showing the server's account of this very attempt by the time
   * this returns. A second copy in component state could only ever disagree
   * with it — and would vanish on reload, which is exactly the dishonesty the
   * column exists to remove.
   */
  function onRefreshNow() {
    refreshZone.mutate(zoneId, {
      onSuccess: (result) =>
        toast.success(`Transferred ${result.records} records from ${result.primary}`),
      // A short toast to close the interaction; the band carries the detail.
      onError: () => toast.error("The transfer failed"),
    });
  }

  const allRecords = useMemo(() => records.data ?? [], [records.data]);
  // The edited row filters like any other row — there is no carve-out
  // keeping it listed, because touching a filter closes the edit outright
  // (see onFiltersTouched below).
  const shownRecords = useMemo(() => {
    const q = nameFilter.trim().toLowerCase();
    return allRecords.filter(
      (r) =>
        (typeFilter === "" || r.type === typeFilter) &&
        (q === "" || r.name.toLowerCase().includes(q)),
    );
  }, [allRecords, nameFilter, typeFilter]);
  /**
   * The record `editingId` names, or null if it names none.
   *
   * Derived rather than stored, and derived from the *shown* records, so the
   * edit self-heals: an id can outlive its row — another session deleting
   * the record, a refetch renaming it out of the current filter — and an
   * edit with no row on screen is no edit at all. Stored, that would leave
   * every other row inert around a form rendered nowhere, with no way out.
   */
  const editingRecord = shownRecords.find((r) => r.id === editingId) ?? null;

  /** One form at a time — see `addOpen`'s comment. */
  function openAdd() {
    setEditingId(null);
    setAddOpen(true);
    setAddCue((c) => c + 1);
  }

  function openEdit(target: ZoneRecord) {
    setAddOpen(false);
    setEditingId(target.id);
  }

  /**
   * Either filter changing closes an in-progress edit, discarding it.
   *
   * Unconditional on purpose — not "closes it if the edited row stops
   * matching". A rule that depended on whether your own row survived the
   * filter would make you work out which case you were in before knowing
   * whether your typing did; "touch a filter, the edit closes" needs no
   * reasoning. The discard is silent: the row was on screen, and a confirm
   * prompt fired by a keystroke in a filter box would be worse than the
   * thing it guards against.
   *
   * The add band is deliberately left alone. It is not a record and has
   * nothing to match, so narrowing a list of records says nothing about it.
   */
  function onFiltersTouched() {
    setEditingId(null);
  }

  function onConfirmDeleteZone() {
    if (!zone.data) return;
    const target = zone.data;
    deleteZone.mutate(target.id, {
      onSuccess: () => {
        toast.success("Zone deleted");
        navigate("/zones");
      },
      onError: () => {
        toast.error(`Couldn't delete ${target.name}`);
        setDeleteZoneOpen(false);
      },
    });
  }

  function onToggleEnabled() {
    if (!zone.data) return;
    const target = zone.data;
    updateZone.mutate(
      { id: target.id, enabled: !target.enabled },
      {
        onSuccess: () => toast.success(target.enabled ? "Zone disabled" : "Zone enabled"),
        onError: () =>
          toast.error(`Couldn't ${target.enabled ? "disable" : "enable"} ${target.name}`),
      },
    );
  }

  function onConfirmDeleteRecord() {
    if (!deleteRecordTarget) return;
    const target = deleteRecordTarget;
    deleteRecord.mutate(
      { zoneId, id: target.id },
      {
        onSuccess: () => {
          toast.success("Record deleted");
          if (editingId === target.id) setEditingId(null);
          setDeleteRecordTarget(null);
        },
        onError: () => toast.error(`Couldn't delete ${target.name}`),
      },
    );
  }

  if (zone.isPending) {
    return (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  }

  if (zone.data === undefined) {
    return (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load this zone</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  }

  const z = zone.data;
  // The RFC 6303 zones seeded at migration (localhost, the reverse-mapping
  // arpa zones, …) — the server 409s every write to one ("built-in zones
  // cannot be changed"), so every control that would produce that 409 is
  // omitted here rather than left to fail on click.
  const isInternal = z.type === "internal";
  /**
   * A secondary's records are its primary's. All four record-write routes
   * answer 409 for one (POST/PUT/DELETE records, POST file — see Task 2's
   * "refuse hand writes into a secondary zone"), so the same rule applies as
   * for a built-in: the controls that would produce that 409 are not
   * rendered. Unlike a built-in, the reason is worth saying out loud in the
   * header, because a secondary *looks* like an ordinary zone the admin
   * created — they did create it — and the read-only-ness is a property of
   * where its contents come from, not of the zone itself.
   */
  const isSecondary = z.type === "secondary";
  const recordsReadOnly = isInternal || isSecondary;

  let body: ReactNode;
  if (records.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (records.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load records</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (shownRecords.length === 0) {
    // An empty secondary is not an invitation to add a record — it cannot
    // hold one — it is a zone whose first transfer has not landed, which is
    // the same fact the transfer band above states in more detail.
    const emptyMessage = isSecondary
      ? "Nothing transferred yet. The records will arrive with the first transfer from the primary."
      : "No records yet. Add one above and dnsaur will answer for this zone directly.";
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        {allRecords.length === 0 ? emptyMessage : "No records match this filter."}
      </p>
    );
  } else {
    body = shownRecords.map((record) =>
      // The edited record's own row *is* the form: it replaces the row
      // rather than appearing above it, which is what stopped the record
      // reading as a duplicate of itself. Keyed by id, so switching the
      // edit to another row is a fresh mount bound to that record.
      record.id === editingRecord?.id ? (
        <RecordFormRow
          key={record.id}
          zoneId={z.id}
          apex={z.name}
          editing={record}
          onDone={() => setEditingId(null)}
        />
      ) : (
        <RecordRow
          key={record.id}
          record={record}
          readOnly={recordsReadOnly}
          actionsInert={editingRecord !== null}
          onEdit={() => openEdit(record)}
          onDelete={() => setDeleteRecordTarget(record)}
        />
      ),
    );
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* Header band */}
      <div className="flex shrink-0 items-center gap-3 border-b border-border bg-card px-4 py-2.5">
        <Link
          to="/zones"
          className="flex items-center gap-1 font-mono text-[10.5px] tracking-[0.08em] text-muted-foreground uppercase hover:text-foreground"
        >
          <ChevronLeft className="size-3.5" aria-hidden="true" />
          Zones
        </Link>
        <span aria-hidden="true" className="h-4 w-px bg-border" />
        <span className="truncate font-mono text-[17px] font-semibold">{z.name}</span>
        <Badge variant={ZONE_TYPE_VARIANT[z.type]}>{z.type}</Badge>
        {/* Three states, not two. A secondary that has never transferred, or
            whose data has expired, is `enabled` in the database and answering
            SERVFAIL to everything (Zone.Serving) — so an "Enabled" badge on
            one would be the screen's single most misleading element. See
            lib/zones.ts's isServing. */}
        {!z.enabled ? (
          <Badge variant="secondary">Disabled</Badge>
        ) : isServing(z) ? (
          <Badge variant="success-light">Enabled</Badge>
        ) : (
          <Badge variant="destructive-light">Not answering</Badge>
        )}
        {/* The marker sits with the badges rather than in the actions cluster
            because it is no longer what stands in for them: a built-in zone
            has an action now (Export), and a label reading "built-in" from
            inside the button group would read as a disabled button. Here it
            is what it actually is — a statement about the zone, next to the
            other two. */}
        {isInternal && (
          <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
            <Lock className="size-3" aria-hidden="true" />
            Built-in · Read-only
          </span>
        )}
        {isSecondary && (
          <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
            <ArrowDownToLine className="size-3" aria-hidden="true" />
            Pulled · Read-only
          </span>
        )}
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {!recordsReadOnly && (
            <Button type="button" size="sm" onClick={openAdd}>
              <Plus />
              Add record
            </Button>
          )}
          {/* The primary action for a copy: not "write a record", which it
              cannot do, but "go and get the current version now". */}
          {isSecondary && (
            <Button type="button" size="sm" onClick={onRefreshNow} disabled={refreshZone.isPending}>
              <RotateCw />
              {refreshZone.isPending ? "Transferring…" : "Refresh now"}
            </Button>
          )}
          {/* Export renders for every zone, built-ins and secondaries
              included — it is the one action that reads rather than writes.
              Import and the rest stay behind the guard; see
              ZoneFileActions. */}
          <ZoneFileActions zone={z} />
          {!isInternal && (
            <>
              <span aria-hidden="true" className="h-5 w-px bg-border" />
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={onToggleEnabled}
                disabled={updateZone.isPending}
              >
                {z.enabled ? "Disable zone" : "Enable zone"}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => setDeleteZoneOpen(true)}
              >
                Delete zone
              </Button>
            </>
          )}
        </div>
      </div>

      {/* SOA band, or — for a copy, whose SOA is its primary's and is
          overwritten by every transfer — the transfer band in its place. See
          TransferBand's own comment. */}
      {isSecondary ? <TransferBand zone={z} /> : <SoaBand zone={z} />}

      {/* Filter bar */}
      <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
        <Input
          value={nameFilter}
          onChange={(e) => {
            setNameFilter(e.target.value);
            onFiltersTouched();
          }}
          placeholder="filter by name…"
          aria-label="Filter by name"
          className="w-[236px] shrink-0 grow-0 font-mono"
        />
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-[9.5px] tracking-[0.14em] text-muted-foreground uppercase">
            Type
          </span>
          <NativeSelect
            value={typeFilter}
            onChange={(e) => {
              setTypeFilter(e.target.value as "" | RecordType);
              onFiltersTouched();
            }}
            aria-label="Filter by record type"
          >
            <NativeSelectOption value="">All types</NativeSelectOption>
            {RECORD_TYPES.map((t) => (
              <NativeSelectOption key={t} value={t}>
                {t}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <span className="ml-auto font-mono text-[10.5px] text-muted-foreground">
          {shownRecords.length} {shownRecords.length === 1 ? "record" : "records"}
        </span>
      </div>

      {records.isError && records.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="records"
            onRetry={() => void records.refetch()}
            isRetrying={records.isFetching}
          />
        </div>
      )}

      {/* Records grid header — first, per the artboard, with the create row
          sitting between it and the scrolling record rows (same order as
          the zones list). */}
      <div
        className={cn(
          GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-xs tracking-widest text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Type</span>
        <span className="text-right">TTL</span>
        <span>Data</span>
        <span className="text-right">Actions</span>
      </div>

      {/* The add band — the one placement that is still a band, because a
          record that doesn't exist yet has no row to become. Omitted for a
          built-in zone (see isInternal's own comment), and, for a writable
          one, until Add record opens it: closed is the loaded state, not
          just a visual one — see addOpen's own comment above. Editing does
          not come through here; it happens in the record's own row below. */}
      {!recordsReadOnly && addOpen && (
        <RecordFormRow
          key={addCue}
          zoneId={z.id}
          apex={z.name}
          editing={null}
          onDone={() => setAddOpen(false)}
        />
      )}

      <div data-testid="zone-record-list" className="min-h-0 flex-1 overflow-y-auto">
        {body}
      </div>

      <AlertDialog open={deleteZoneOpen} onOpenChange={setDeleteZoneOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this zone?</AlertDialogTitle>
            <AlertDialogDescription>
              <span className="font-medium text-foreground">{z.name}</span> and its records will
              stop being served immediately.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDeleteZone}
              disabled={deleteZone.isPending}
            >
              {deleteZone.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        open={deleteRecordTarget !== null}
        onOpenChange={(open) => !open && setDeleteRecordTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this record?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteRecordTarget && (
                <>
                  <code className="font-mono break-all text-foreground">
                    {deleteRecordTarget.name} {deleteRecordTarget.type}
                  </code>{" "}
                  will stop being served.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDeleteRecord}
              disabled={deleteRecord.isPending}
            >
              {deleteRecord.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
