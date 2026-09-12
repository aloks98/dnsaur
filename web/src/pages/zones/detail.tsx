import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import {
  AlertCircle,
  ArrowDownToLine,
  ChevronLeft,
  ChevronRight,
  Lock,
  Pencil,
  Plus,
  RotateCw,
  Trash2,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, useWatch, type Control, type UseFormReturn } from "react-hook-form";
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
  CopyButton,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormMessage,
  Input,
  Kbd,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  Textarea,
  useFormField,
  type BadgeProps,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { Zone, ZoneRecord } from "../../api/types";
import { parseACL } from "../../lib/acl";
import { isNotifyBehind, notifyRestLabel, notifyRollup, parseNotifyTo } from "../../lib/notify";
import {
  useCreateZoneRecord,
  useDeleteZone,
  useDeleteZoneRecord,
  useRecordsFollowTransfers,
  useRefreshZone,
  useSetZoneRecordsTTL,
  useUpdateZone,
  useUpdateZoneRecord,
  useZone,
  useZoneNotifies,
  useZoneRecords,
  type ZoneNotify,
  type ZoneUpdateInput,
} from "../../hooks/use-zones";
import { useTSIGKeys } from "../../hooks/use-tsig-keys";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { NotFound } from "../not-found";
import { ZONE_TYPE_VARIANT } from "./zone-type-variant";
import { formatDuration, relativeTime } from "../../lib/format";
import { RenameDialog } from "../dialogs";
import { ZoneFileActions } from "./zone-file-actions";
import {
  forwardTargets,
  isServing,
  lastTransferError,
  nextRefreshAt,
  retryIntervalMs,
  schedulerNote,
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
 * Everything the type select must be able to show for `current`: the nine
 * above plus — when the record already holds something else — that value.
 *
 * The server takes any type `dns.NewRR` parses, so a zone imported from
 * another server can hold an SSHFP, HTTPS or TLSA this list has never heard
 * of. Without its own option a native select shows (and submits) the first
 * one instead, so opening such a record and pressing Save would rewrite its
 * type to A. The same passthrough the TSIG key row uses for an algorithm it
 * doesn't recognise (lib/tsig.ts's algorithmOptions), for the same reason.
 */
function recordTypeOptions(current: string | undefined): string[] {
  const known: string[] = [...RECORD_TYPES];
  if (current === undefined || current === "" || known.includes(current)) return known;
  return [...known, current];
}

/**
 * The types whose rdata gets a growing textarea rather than a single-line
 * input: the two whose values are routinely longer than the column. A DKIM
 * key is 400-odd characters of base64 and a DNSKEY's is the same shape, and
 * in one line either is a ribbon nobody can read back.
 */
const LONG_RDATA_TYPES = new Set(["TXT", "DNSKEY"]);

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
 * e412.in is bifrost.e412.in), but rdata is parsed by ToRR with no origin
 * to resolve against (see buildZoneRecord in
 * internal/api/zonerecords_handlers.go), so a dotless target there is
 * already absolute — "nas" is the name `nas.`, not nas.e412.in. Cloudflare
 * and Route 53 accept the dotless spelling and resolve it against the zone,
 * so that is the habit users arrive with.
 *
 * The one exception is a bare `@`, which BuildRecord resolves to the zone's
 * apex before parsing — the same thing it means in an imported zone file, so
 * a hand-written record and an imported one store the same value. `@` is
 * therefore worth typing, and the apex does not have to be spelled out.
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

/** One declaration of the column geometry, shared by the records header,
 * the create/edit row and every record row — matching the artboard's grid
 * exactly. TTL and Actions are the two right-aligned columns; Data is 1fr. */
const GRID = "grid grid-cols-[224px_96px_84px_1fr_92px] items-center gap-3.5 px-4";

/** RFC 2181 §8: the largest TTL a resolver reads back as typed, mirroring
 * maxRecordTTL in internal/api/zonerecords_handlers.go. */
const MAX_TTL = 2147483647;
/** The top of a uint32, mirroring zonePatch's SOA fields (internal/api/zones_handlers.go). */
const MAX_UINT32 = 4294967295;

/**
 * The bulk-TTL dialog's one field, spelled as `name` because that is the key
 * the shared one-field dialog carries (RenameDialog, pages/dialogs.tsx). The
 * rule is recordFormSchema's ttl rule verbatim — one number, one ceiling —
 * because it is the same value being written by a different route, and two
 * spellings of RFC 2181 §8 in one file is one too many.
 */
const bulkTTLSchema = z.object({
  name: z
    .string()
    .trim()
    .regex(/^\d+$/, "TTL must be a whole number of seconds")
    .refine((v) => Number(v) <= MAX_TTL, "TTL must not exceed 2147483647"),
});

const recordFormSchema = z.object({
  // Blank is a legitimate value — the server folds "" and the zone's own
  // name to "@" (normalizeRecordName) — so this is deliberately
  // unconstrained rather than "required".
  name: z.string(),
  // Free-form rather than z.enum(RECORD_TYPES): the select's own options are
  // what constrain this, and an enum would reject the passthrough option
  // recordTypeOptions adds for a stored type this build doesn't list —
  // turning "edit this record's TTL" into a form that cannot be submitted.
  type: z.string().min(1),
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
    type: record?.type ?? "A",
    ttl: String(record?.ttl ?? 300),
    rdata: record?.rdata ?? "",
  };
}

/** Mono, with `@` and any wildcard name (`*`, `*.nexus`) picked out in
 * primary/600 — the two shapes worth scanning for in a long record list.
 * A disabled record's name is muted instead: the highlight says "scan for
 * this", and a record that is not being served is not what to scan for. */
function RecordName({ name, dimmed = false }: { name: string; dimmed?: boolean }) {
  const highlighted = name === "@" || name.startsWith("*");
  return (
    <span
      className={cn(
        "truncate font-mono text-sm",
        highlighted ? "font-semibold" : "font-normal",
        dimmed ? "text-muted-foreground" : highlighted ? "text-primary" : "text-foreground",
      )}
      title={name}
    >
      {name}
    </span>
  );
}

/**
 * The rejected rdata, in the DNS parser's own words — mono, with an
 * alert-circle, rather than the plain FormMessage every other column gets.
 *
 * Its own component only so it can read `useFormField()` from *inside* the
 * FormItem: the id FormControl points `aria-describedby` at is generated by
 * that FormItem, and a message rendered anywhere else carries a different one
 * (or none), which is a message no screen reader ever reaches.
 */
function RdataErrorLine({ message }: { message: string }) {
  const { formMessageId } = useFormField();
  return (
    <span
      id={formMessageId}
      className="flex items-start gap-1.5 text-xs text-pretty text-destructive-foreground"
    >
      <AlertCircle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span className="font-mono">{message}</span>
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

  const type = useWatch({ control: form.control, name: "type" });

  // At "@" the zone *is* the name, so the chip carries the apex whole and
  // the typed text goes muted — the field is saying "there is nothing left
  // for you to add". Anywhere else it carries the apex with its joining
  // dot, and what's typed reads as the subdomain in front of it. A wildcard
  // ("*", "*.nexus") is an ordinary relative name and gets no special case.
  const atApex = (useWatch({ control: form.control, name: "name" }) ?? "").trim() === "@";
  const suffixFull = atApex ? apex : `.${apex}`;
  const suffixShown = atApex ? clipApex(apex) : `.${clipApex(apex)}`;
  const dataIsName = NAME_VALUED_TYPES.has(type as RecordType);
  const longRdata = LONG_RDATA_TYPES.has(type);

  function onSubmit(values: RecordFormValues) {
    const payload = {
      name: values.name.trim(),
      type: values.type,
      ttl: Number(values.ttl.trim()),
      rdata: values.rdata.trim(),
      // PUT is a full replace and BuildRecord defaults an absent `enabled`
      // to true and an absent `comment` to "", so an edit that carried
      // neither would re-enable a disabled record and drop its comment.
      // Neither is editable here; both are carried through untouched.
      ...(editing !== null ? { enabled: editing.enabled, comment: editing.comment } : {}),
    };
    // The server's own text (dns.NewRR's parser message, or an RFC-conflict
    // message) is more precise than anything this form could invent — it
    // lands on the rdata field itself rather than a toast, aria-invalid and
    // all, via setError below.
    const onError = (err: unknown) => {
      // A 404 is not a complaint about the value that was typed: the record
      // this row is bound to has been deleted from somewhere else, so there
      // is nothing to correct and the form has nothing left to edit. The
      // mutation has already asked for the list again (see
      // refetchIfGone in use-zones.ts).
      if (isEdit && err instanceof ApiError && err.status === 404) {
        toast.error("That record no longer exists");
        onDone();
        return;
      }
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
          // Only what identifies the record just written. Adding one record
          // is usually adding several of the same kind, and type and TTL are
          // exactly the two that repeat.
          form.reset({ ...values, name: "", rdata: "" });
          form.setFocus("name");
        },
        onError,
      },
    );
  }

  // ttl is the one field carrying a client-side rule (see recordFormSchema's
  // own comments), and a zod failure there must never be a silent no-op: the
  // button blocking submit with nothing on screen to say why. rdata's
  // *server* error gets its own distinct rendering below (mono,
  // alert-circle) rather than a plain FormMessage, since that one is the DNS
  // parser's own text, not a client-authored message.
  const rdataError = form.formState.errors.rdata?.message;

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
        {/* One grid, and every column's message sits inside that column's own
            FormItem rather than on a second grid row of its own. A message in
            a separate FormItem gets a separate generated id, so the
            `aria-describedby` FormControl writes onto the input pointed at
            nothing and a screen reader heard no error at all. `items-start`
            in place of the shared GRID's `items-center` is what keeps every
            control on one line while a message grows the row below it. */}
        <div className={cn(GRID, "items-start py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem className="gap-1">
                {/* The shell sits inside FormItem rather than replacing it,
                    so FormControl still slots onto the input itself — the
                    id, the label association and aria-invalid all stay
                    where a form control's belong. */}
                <div className={FIELD_SHELL}>
                  <FormControl>
                    <Input
                      {...field}
                      aria-label="Zone record name"
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
                  {/* The suffix is context, not part of the value being
                      typed, so it reaches assistive tech as this field's
                      *description* — the accessible name is untouched. A
                      FormDescription rather than a hand-written id, so it is
                      the one FormControl's own describedby already points
                      at. */}
                  <FormDescription className="sr-only">{suffixFull}</FormDescription>
                </div>
                <FormMessage className="text-xs" />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="type"
            render={({ field }) => (
              <FormItem className="gap-1">
                <FormControl>
                  <NativeSelect {...field} aria-label="Record type">
                    {recordTypeOptions(editing?.type).map((t) => (
                      <NativeSelectOption key={t} value={t}>
                        {t}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
                <FormMessage className="text-xs" />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="ttl"
            render={({ field }) => (
              <FormItem className="gap-1">
                <FormControl>
                  <Input {...field} aria-label="TTL" inputMode="numeric" />
                </FormControl>
                <FormMessage className="text-xs" />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="rdata"
            render={({ field }) => (
              <FormItem className="gap-1">
                <div
                  className={cn(
                    FIELD_SHELL,
                    longRdata && "h-auto",
                    rdataError && FIELD_SHELL_INVALID,
                  )}
                >
                  <FormControl>
                    {longRdata ? (
                      <Textarea
                        {...field}
                        rows={1}
                        onChange={(e) => {
                          field.onChange(e);
                          if (form.formState.errors.rdata) form.clearErrors("rdata");
                        }}
                        aria-label="Data"
                        placeholder={DATA_PLACEHOLDER[type as RecordType]}
                        autoComplete="off"
                        spellCheck={false}
                        // field-sizing-content comes with rnui's Textarea, so
                        // the box grows with a DKIM key instead of scrolling
                        // it; min-h-16 does not belong on a grid row.
                        className={cn(FIELD_INPUT, "min-h-8 resize-none py-1.5 text-sm")}
                      />
                    ) : (
                      <Input
                        {...field}
                        onChange={(e) => {
                          field.onChange(e);
                          // A fresh attempt deserves a fresh read of the parser,
                          // not last submit's stale rejection sitting under it.
                          if (form.formState.errors.rdata) form.clearErrors("rdata");
                        }}
                        aria-label="Data"
                        placeholder={DATA_PLACEHOLDER[type as RecordType]}
                        autoComplete="off"
                        className={FIELD_INPUT}
                      />
                    )}
                  </FormControl>
                  {dataIsName && (
                    // Described by the chip itself, not a hidden twin: this
                    // one is never clipped, so its own text is the whole of
                    // what it says. Types without the chip fall back to no
                    // description at all.
                    <FormDescription
                      data-testid="record-data-suffix"
                      className={cn(FIELD_CHIP, "text-xs tracking-widest uppercase")}
                    >
                      Full name
                    </FormDescription>
                  )}
                </div>
                {/* Data's error is the DNS parser's own output (or the
                    server's RFC-conflict text) — rendered distinctly, mono
                    with an alert-circle icon, rather than as a plain
                    FormMessage; see the artboard's own note on not rewriting
                    parser errors. It still carries FormMessage's own id, so
                    the input announces it. */}
                {rdataError && <RdataErrorLine message={rdataError} />}
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
  /**
   * A disabled record is dropped when the zone's served snapshot is built
   * (NewZone), so it is not being answered — indistinguishable from never
   * having been written. Drawn like every other row it claimed the opposite,
   * and nothing on the grid said otherwise. The word carries it; the fade is
   * only what stops the row competing with the ones that are live.
   */
  const off = !record.enabled;
  return (
    <div
      data-testid="zone-record-row"
      data-enabled={record.enabled ? "true" : "false"}
      className={cn(GRID, "border-b border-border-muted py-2", off && "bg-muted/30")}
    >
      <span className="flex min-w-0 items-center gap-2">
        <RecordName name={record.name} dimmed={off} />
        {off && (
          <span className="shrink-0 font-mono text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
            Disabled
          </span>
        )}
      </span>
      <span>
        <Badge variant={off ? "secondary" : variant}>{record.type}</Badge>
      </span>
      <span className="text-right font-mono text-sm text-muted-foreground">
        {record.ttl.toLocaleString()}
      </span>
      <span
        className={cn("truncate font-mono text-sm", off && "text-muted-foreground")}
        title={record.rdata}
      >
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
 * One editable SOA field: its caption, its control and its message inside a
 * single FormItem, so the id FormControl writes into `aria-describedby` is
 * the id the message on screen actually carries. Rendered outside FormItem —
 * as all seven of these were — the input was never marked invalid and the
 * message it pointed at did not exist.
 *
 * `contents` on the FormItem so SoaFieldShell keeps owning the layout; the
 * FormItem is here for its context, not for a box.
 */
function SoaField({
  control,
  name,
  label,
  unit,
  copyable = false,
}: {
  control: Control<SoaFormValues>;
  name: keyof SoaFormValues;
  label: string;
  /** Present on the four interval fields, all of which are seconds — which
   * is also what makes them the numeric ones. */
  unit?: string;
  /** Whether the field's current value gets a copy button beside it.
   *
   * The two name fields only. They are what gets pasted into somebody else's
   * config — a registrar's NS form, another server's zone — while the four
   * intervals are numbers read off the screen and typed, and four more
   * buttons in this grid would bury the two that matter. */
  copyable?: boolean;
}) {
  return (
    <FormField
      control={control}
      name={name}
      render={({ field }) => (
        <FormItem className="contents">
          <SoaFieldShell label={label} unit={unit}>
            <div className="flex min-w-0 items-center gap-1">
              <FormControl>
                <Input
                  {...field}
                  aria-label={label}
                  inputMode={unit === undefined ? undefined : "numeric"}
                  className="font-mono"
                />
              </FormControl>
              {/* The value being edited, not the saved one: what is on screen
                  is what the button should hand over, and an unsaved edit is
                  still the thing the operator is looking at. */}
              {copyable && field.value !== "" && (
                <CopyButton value={field.value} label={`Copy ${label.toLowerCase()}`} />
              )}
            </div>
            <FormMessage />
          </SoaFieldShell>
        </FormItem>
      )}
    />
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
  const managedBy = useManagedBy();
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

  // A primary zone's AllowTransferBand rides immediately beneath this one,
  // sharing this same 152px gutter (see that component's own comment) — it
  // supplies its own top rule and the group's trailing one, so a bottom
  // rule here too would double it. Every other zone type that reaches this
  // band (internal, stub, forwarder) has nothing following it, and keeps
  // the rule so the band still closes itself off from the filter bar below.
  const closesGroup = zone.type !== "primary";

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        className={cn("shrink-0 bg-card", closesGroup && "border-b border-border")}
      >
        <Collapsible open={open} onOpenChange={setOpen}>
          {/* 152px/1fr: the label column AllowTransferBand's own row shares
              below, so the two captions ("SOA" here, "TRANSFERS OUT" there)
              land on one edge. items-start on the label cell pins the
              chevron and caption to the top rather than centering them
              against the taller, open state's height — the grid's default
              stretch is what makes the border-right span that full height. */}
          <div className="grid grid-cols-[152px_1fr]">
            <div className="flex items-start border-r border-border-muted px-3.5 py-[11px]">
              <CollapsibleTrigger
                type="button"
                // cursor-pointer explicitly: Tailwind v4 preflight sets no
                // cursor on <button>.
                className="flex min-w-0 cursor-pointer items-center gap-1 text-left"
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
              </CollapsibleTrigger>
            </div>
            <div className="min-w-0 px-4 py-[10px]">
              {!open && (
                <span className="truncate font-mono text-xs text-muted-foreground">{summary}</span>
              )}
              <CollapsibleContent>
                <div className="flex items-center justify-end pb-2.5">
                  <Button
                    type="submit"
                    size="sm"
                    variant="ghost"
                    disabled={!form.formState.isDirty || updateZone.isPending || managedBy !== ""}
                  >
                    {updateZone.isPending ? "Saving…" : "Save SOA"}
                  </Button>
                </div>
                <div className="grid grid-cols-4 gap-x-[18px] gap-y-4 pb-4">
                  <SoaField control={form.control} name="soa_ns" label="Primary NS" copyable />
                  <SoaField control={form.control} name="soa_mbox" label="Responsible" copyable />
                  {/* Not editable, but the value IS shown (Task 12 fix round 1:
                  an earlier draft of the artboard notes said only "AUTO",
                  omitting the number — corrected). The serial itself is the
                  box's content; AUTO is pushed to the right edge as an
                  annotation that the server, not this form, owns the number. */}
                  <SoaFieldShell label="Serial">
                    <div className="flex h-8 items-center rounded-lg border border-input bg-muted px-2.5">
                      <span className="font-mono text-sm text-muted-foreground">
                        {zone.soa_serial}
                      </span>
                      <span className="ml-auto font-mono text-[9.5px] tracking-[0.14em] text-muted-foreground/70 uppercase">
                        AUTO
                      </span>
                      {/* The one SOA value an operator reads *out* of this
                          page rather than into it: it is what a `dig` against
                          a secondary is compared with. */}
                      <CopyButton value={String(zone.soa_serial)} label="Copy serial" />
                    </div>
                  </SoaFieldShell>
                  <SoaField control={form.control} name="soa_refresh" label="Refresh" unit="s" />
                  <SoaField control={form.control} name="soa_retry" label="Retry" unit="s" />
                  <SoaField control={form.control} name="soa_expire" label="Expire" unit="s" />
                  <SoaField control={form.control} name="soa_minimum" label="Minimum" unit="s" />
                </div>
              </CollapsibleContent>
            </div>
          </div>
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
function TransferBand({ zone, asOf }: { zone: Zone; asOf: number }) {
  const state = transferState(zone);
  const failure = lastTransferError(zone);
  /**
   * What the scheduler in the running process says about this zone: when its
   * next attempt falls, and how many have failed in a row. Empty for a
   * healthy zone, and empty after a restart even for one that is not — see
   * schedulerNote. It is shown beside the dated failure below rather than
   * instead of it, for exactly that reason.
   *
   * asOf rather than Date.now(), like the countdown above: render reads no
   * clock, and the number is anchored to the answer it came with.
   */
  const scheduled = schedulerNote(zone, asOf);
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
   * Its transfer state is still true and still shown: the serial and the
   * last refresh are facts, and a recorded failure is a dated account of the
   * last thing that actually happened. What changes is the two
   * forward-looking claims, which are consequences of the switch rather than
   * of the transfer.
   */
  const disabled = !zone.enabled;
  const serving = !disabled && state !== "never" && state !== "expired";

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
    // asOf, not Date.now(): render reads no clock (react(purity)), and the
    // countdown is anchored to the answer it describes rather than to
    // whenever React last happened to re-render this band. useZone polls
    // while a zone watches transfers — which is every zone that gets one of
    // these — so it advances on the same schedule the rest of the band does.
    nextRefresh = `in ${formatDuration(nextRefreshAt(zone) - asOf)}`;
  } else {
    nextRefresh = `retrying every ${formatDuration(retryIntervalMs(zone))}`;
  }

  // A disabled zone's transfer state is a consequence of the switch, so it is
  // reported plainly rather than in the colours of a problem to solve — the
  // same reasoning that makes the zones list show it as a muted "Disabled"
  // instead of "Expired".
  const bad = !disabled && (state === "never" || state === "expired");
  const warn = !disabled && (state === "failing" || state === "overdue");

  // Where it pulls from and what it signs with are not here: they are
  // editable, and live in the row below this band (UpstreamRow) rather than
  // being printed twice.
  const fields: { label: string; value: string; strong?: boolean; tone?: string }[] = [
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
      // not have: Index.Find skips it entirely (internal/zones/zone.go), so
      // names under it fall through upstream. What that costs a
      // split-horizon zone is docs/dashboard.md's to explain.
      "Nothing transfers while this zone is disabled."
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
  // "nothing is happening here" is the one thing the fields above cannot say
  // on their own.
  const showNote = disabled || state !== "fresh" || scheduled !== "";
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
      // No trailing border: the primaries row always follows a secondary's
      // TransferBand and the allow-transfer band follows that, so the rule
      // that closes the group is theirs — the same reason SoaBand's own
      // border is conditional.
      className={cn(
        "shrink-0",
        bad
          ? "bg-destructive/5 shadow-[inset_3px_0_0_var(--destructive)]"
          : warn
            ? "bg-warning/5 shadow-[inset_3px_0_0_var(--warning)]"
            : "bg-card",
      )}
    >
      <div className="grid grid-cols-3 gap-[18px] px-4 py-3">
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
          grid off rather than letting the error look like one more
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
              {/* The scheduler's own two facts, last on the line because
                  they are the only ones on it that a restart forgets. */}
              {scheduled !== "" && (
                <span
                  data-testid="scheduler-note"
                  className="shrink-0 font-mono text-[11px] text-muted-foreground"
                >
                  {scheduled}
                </span>
              )}
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

// ── The inline value rows ─────────────────────────────────────────────────

/**
 * Three rows on this page — who may transfer this zone out, where it pulls
 * from or forwards to, and who it notifies — are the same machine with a
 * different caption, a different PATCH key and a different thing to say
 * while not being edited.
 *
 * That machine is: hold one string, reset it when the *saved* value changes
 * (never on an unrelated refetch, which would wipe an edit in progress),
 * focus the field the moment editing opens, PATCH the trimmed value, toast
 * either way, and close on success. Written out three times it drifted three
 * ways and carried the same accessibility gap three times over.
 *
 * The field is called `value` whatever the column is, so one schema shape
 * and one `<FormField>` serve all three; the caller says which key the value
 * is PATCHed under.
 */
interface InlineFormValues {
  value: string;
}

interface InlineValueEdit {
  form: UseFormReturn<InlineFormValues>;
  editing: boolean;
  pending: boolean;
  start: () => void;
  cancel: () => void;
  onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
}

function useInlineValueEdit({
  zoneId,
  saved,
  schema,
  payload,
  savedMessage,
  failedMessage,
}: {
  zoneId: number;
  /** The stored value this row edits — the one the form resets to. */
  saved: string;
  schema: z.ZodType<InlineFormValues, InlineFormValues>;
  /** What a successful Save sends, built from the trimmed field. Called at
   * submit time, so a row carrying a second control (the key select beside
   * a secondary's primaries) can fold that control's value in here. */
  payload: (value: string) => ZoneUpdateInput;
  savedMessage: string;
  failedMessage: string;
}): InlineValueEdit {
  const [editing, setEditing] = useState(false);
  const updateZone = useUpdateZone();
  const form = useForm<InlineFormValues>({
    resolver: zodResolver(schema),
    defaultValues: { value: saved },
  });

  // Reset only when the *saved* value changes — initial load, or the refetch
  // that follows a successful save. An unrelated refetch (the transfer poll,
  // a record write bumping the serial) must not wipe an unsaved edit.
  useEffect(() => {
    form.reset({ value: saved });
  }, [saved, form]);

  // Focus the field the moment editing opens — RecordFormRow's own habit for
  // a form that has just appeared.
  useEffect(() => {
    if (editing) form.setFocus("value");
  }, [editing, form]);

  function start() {
    // Always the saved value, never a previous attempt's abandoned draft —
    // the pencil opens onto what is actually set, every time.
    form.reset({ value: saved });
    setEditing(true);
  }

  function cancel() {
    form.reset({ value: saved });
    setEditing(false);
  }

  function submit(values: InlineFormValues) {
    updateZone.mutate(
      { id: zoneId, ...payload(values.value.trim()) },
      {
        onSuccess: () => {
          toast.success(savedMessage);
          setEditing(false);
        },
        // The server's own words wherever it has any: for the values these
        // rows carry it is the only account of what was wrong with one.
        onError: (err) => toast.error(err instanceof ApiError ? err.message : failedMessage),
      },
    );
  }

  return {
    form,
    editing,
    pending: updateZone.isPending,
    start,
    cancel,
    onSubmit: (event) => void form.handleSubmit(submit)(event),
  };
}

/**
 * The shell those three rows share: SoaBand's own 152px label gutter, a
 * caption in it, and one line that is either the saved value with a pencil
 * or the field with Save and X.
 *
 * The gutter is what makes every caption on this page ("SOA", "Transfers
 * out", "Primaries", "Notify out") land on one edge; the blank first cell of
 * the caption's inner grid is what lines its text up with SOA's chevron.
 *
 * `read` is the row's own answer to "what is set here"; `footer` is whatever
 * it has to show underneath in both states (a failed fetch, the delivery
 * state of each notify target). The FormItem spans the field *and* the
 * message, which is what makes FormControl's `aria-describedby` point at the
 * message actually on screen — three separately hand-written copies of this
 * row each linked nothing.
 */
function InlineValueRow({
  edit,
  caption,
  inputLabel,
  editLabel,
  cancelLabel,
  placeholder,
  inputClassName,
  read,
  editExtra,
  footer,
  topRule = false,
  closesGroup = true,
}: {
  edit: InlineValueEdit;
  caption: ReactNode;
  inputLabel: string;
  editLabel: string;
  cancelLabel: string;
  placeholder: string;
  inputClassName?: string;
  read: ReactNode;
  /** A second control beside the field, saved by the same button. */
  editExtra?: ReactNode;
  footer?: ReactNode;
  /** An internal divider from the band above, where one sits above. */
  topRule?: boolean;
  /** Whether this row carries the header group's bottom rule — false when
   * the row below it supplies one, which would otherwise double it. */
  closesGroup?: boolean;
}) {
  // Every row built on this — the transfer ACL, a copy's primaries, the
  // notify targets — is a field on the zone, and a zone belongs to the main
  // on a replica. One gate here rather than three at the call sites.
  const managedBy = useManagedBy();
  return (
    <Form {...edit.form}>
      <form
        onSubmit={edit.onSubmit}
        noValidate
        className={cn(
          "grid shrink-0 grid-cols-[152px_1fr] bg-card",
          topRule && "border-t border-border-muted",
          closesGroup && "border-b border-border",
        )}
      >
        <div className="flex items-start border-r border-border-muted px-3.5 py-[9px]">
          <span className="grid grid-cols-[12px_auto] items-center gap-[7px] font-mono text-[9.5px] leading-[1.3] font-semibold tracking-[0.14em] text-muted-foreground uppercase">
            <span aria-hidden="true" />
            <span>{caption}</span>
          </span>
        </div>
        <FormField
          control={edit.form.control}
          name="value"
          render={({ field }) => (
            <div className="min-w-0">
              {/* `contents` so the FormItem provides its id to both the
                  control and the message without adding a box between them
                  and this column's own layout. */}
              <FormItem className="contents">
                <div className="flex min-w-0 items-center gap-2.5 px-4 py-2">
                  {edit.editing ? (
                    <>
                      <FormControl>
                        <Input
                          {...field}
                          aria-label={inputLabel}
                          placeholder={placeholder}
                          autoComplete="off"
                          spellCheck={false}
                          className={cn("h-7 font-mono", inputClassName)}
                        />
                      </FormControl>
                      {editExtra}
                      <div className="ml-auto flex shrink-0 items-center gap-1.5">
                        <Button type="submit" size="sm" disabled={edit.pending}>
                          {edit.pending ? "Saving…" : "Save"}
                        </Button>
                        <Button
                          type="button"
                          size="icon-sm"
                          variant="ghost"
                          aria-label={cancelLabel}
                          onClick={edit.cancel}
                        >
                          <X />
                        </Button>
                      </div>
                    </>
                  ) : (
                    <>
                      {read}
                      <Button
                        type="button"
                        size="icon-sm"
                        variant="ghost"
                        title={editLabel}
                        aria-label={editLabel}
                        className="ml-auto shrink-0"
                        disabled={managedBy !== ""}
                        onClick={edit.start}
                      >
                        <Pencil />
                      </Button>
                    </>
                  )}
                </div>
                {/* The artboard has no slot for this — a malformed entry has
                    to say where it was typed regardless of the one-line
                    layout above, so it gets its own line under the row. */}
                {edit.editing && (
                  <div className="px-4 pb-2">
                    <FormMessage />
                  </div>
                )}
              </FormItem>
              {footer}
            </div>
          )}
        />
      </form>
    </Form>
  );
}

// ── The allow-transfer band ───────────────────────────────────────────────

const allowTransferSchema = z.object({
  // No static shape (regex, length, …): "valid" here means "parses as an
  // ACL", which only parseACL itself can answer — the same reason
  // recordFormSchema's rdata carries no client-side rule (see that field's
  // comment) does not apply, because unlike a DNS RR's rdata the ACL
  // grammar is small and stable enough to be worth the client-side port
  // (see lib/acl.ts's own comment on why, and on the server remaining the
  // one that actually decides).
  value: z.string().superRefine((value, ctx) => {
    const result = parseACL(value);
    if (!result.ok) ctx.addIssue({ code: "custom", message: result.error });
  }),
});

/**
 * What the last inbound transfer *request* did, as one line — this band's
 * own dated fact, on the same terms TransferBand documents for
 * `last_error`: read off the zone row rather than remembered from a request
 * this page happened to make, so it is current whether the last attempt was
 * a minute ago or came in while nobody had this page open.
 *
 * `last_xfr_error` is shown verbatim, the way TransferBand already shows
 * `last_error` verbatim — it is the server's own reason, not this band's
 * words for it.
 */
function xfrServedLine(zone: Zone): string {
  if (zone.last_xfr_at === 0) return "Never asked for.";
  if (zone.last_xfr_error === "") {
    return `Last served ${relativeTime(zone.last_xfr_at)} to ${zone.last_xfr_peer}`;
  }
  return `Refused ${zone.last_xfr_peer} — ${zone.last_xfr_error}`;
}

/**
 * Who may take this zone from here, and who last did.
 *
 * Serving transfers applies to both a primary and a secondary — a secondary
 * re-serves what it pulled (§9.5.3) — so it cannot live inside SoaBand
 * (primary-only) or TransferBand (secondary-only, and about the transfers
 * *this* zone pulls, not the ones it serves).
 *
 * Mounted only for a primary or a secondary — never internal, stub or
 * forwarder. §9.5.3 refuses those with NOTAUTH regardless of their ACL, and
 * a built-in zone's every PATCH 409s outright (handleZonePatch), so an
 * editable field here for either would be exactly the kind of control the
 * header above already omits rather than lets fail on click. See the call
 * site's own comment on why the gate is the positive set.
 *
 * `allow_transfer` is default-deny: a zone created with none set answers
 * every transfer request REFUSED, and this row is the only place in the app
 * that opens it up. The field's own client-side check is a convenience, not
 * the gate — the server validates independently and is what a real request
 * is ever matched against (see lib/acl.ts's own comment).
 */
function AllowTransferBand({ zone }: { zone: Zone }) {
  const edit = useInlineValueEdit({
    zoneId: zone.id,
    saved: zone.allow_transfer,
    schema: allowTransferSchema,
    payload: (value) => ({ allow_transfer: value }),
    savedMessage: "Allow transfer saved",
    failedMessage: "Couldn't save allow transfer",
  });
  const hasAcl = zone.allow_transfer !== "";

  return (
    <InlineValueRow
      edit={edit}
      caption={
        <>
          Transfers
          <br />
          out
        </>
      }
      inputLabel="Allow transfer"
      editLabel="Edit allow transfer"
      cancelLabel="Cancel editing allow transfer"
      placeholder="10.0.0.0/24, key:ns2"
      inputClassName="w-[236px] shrink-0"
      topRule
      read={
        <>
          {/* The saved value, mono — muted when there is none, so an empty
              ACL reads as a stated fact rather than a blank field waiting to
              be filled in. */}
          <span
            className={cn(
              "min-w-0 shrink truncate font-mono text-[12.5px]",
              hasAcl ? "text-foreground" : "text-muted-foreground",
            )}
            title={hasAcl ? zone.allow_transfer : undefined}
          >
            {hasAcl ? zone.allow_transfer : "No peer may transfer this zone."}
          </span>
          {/* Only with something to copy — an empty ACL is a stated fact, not
              a value. Same rule the primaries row follows. */}
          {hasAcl && <CopyButton value={zone.allow_transfer} label="Copy allow transfer" />}
          {/* What the last inbound *request* did — independent of the ACL
              beside it: a peer can be refused today under a value that used
              to admit it, so the history stays on screen either way. */}
          <span
            className="min-w-0 shrink truncate font-mono text-[11px] text-muted-foreground"
            title={xfrServedLine(zone)}
          >
            {xfrServedLine(zone)}
          </span>
        </>
      }
    />
  );
}

// ── The upstream row: PRIMARIES for a pulling zone, FORWARD TO for a
//    forwarder ─────────────────────────────────────────────────────────────

/**
 * Where this zone's queries go, and — for one that pulls — what it signs
 * the asking with.
 *
 * A forwarder, a stub and a secondary are one question with three sources
 * for the addresses. A forwarder's targets are typed into `forward_to` by
 * the operator and every query beneath its suffix is routed to them; a
 * stub's are *derived from an NS set it fetches* from the masters in
 * `primaries` (an SOA and an NS query with glue, deliberately not an AXFR —
 * see internal/zones/stub.go); a secondary pulls the whole zone from them
 * over AXFR. One row rather than three, because the question it answers is
 * one question: where does this zone go to get what it serves.
 *
 * The grammar is not checked here at all: `zones.ValidateForwardTo` and
 * `ValidatePrimaries` are the real checks and a second implementation of
 * `host[:port]` in this file would only drift from them, so a malformed
 * entry gets the server's own message. The one client-side rule is that a
 * zone that pulls must name somewhere to pull from, which exists only to
 * save a round trip that would always 400.
 */
function upstreamSchema(required: boolean, message: string) {
  return z.object({
    value: z.string().superRefine((value, ctx) => {
      if (required && value.trim() === "") ctx.addIssue({ code: "custom", message });
    }),
  });
}

function UpstreamRow({ zone }: { zone: Zone }) {
  const forwarder = zone.type === "forwarder";
  /**
   * Whether this zone signs what it asks for, and so whether the row carries
   * a key select beside the addresses. `tsig_key_id` is accepted on exactly
   * the two types that pull (checkZoneTransferConfig), and refused on a
   * forwarder, which signs nothing.
   */
  const signs = zone.type === "secondary" || zone.type === "stub";
  const keys = useTSIGKeys();
  /**
   * The key while the row is being edited. Held here rather than in the form
   * because it is a choice over a fixed set with nothing to validate; the
   * value that is saved is read at submit time.
   */
  const [keyId, setKeyId] = useState(zone.tsig_key_id);
  // Reset during render rather than from an effect — React's own recipe for
  // adjusting state when a prop changes. The effect this replaces rendered
  // the stale key once, committed it, and only then corrected itself; this
  // re-runs the component before anything is shown. The reset still fires on
  // exactly the same occasions: the saved key changing, which is the initial
  // load and the refetch after a save, and never an unrelated poll.
  const [keyIdFor, setKeyIdFor] = useState(zone.tsig_key_id);
  if (keyIdFor !== zone.tsig_key_id) {
    setKeyIdFor(zone.tsig_key_id);
    setKeyId(zone.tsig_key_id);
  }

  // The field this row edits, and the only difference between the shapes
  // that reaches the wire. They are separate columns because they are
  // separate things: the server 400s `primaries` on a forwarder and
  // `forward_to` on anything else, so one shared column would be refused by
  // whichever type it was not written for.
  const saved = forwarder ? zone.forward_to : zone.primaries;
  const label = forwarder ? "Forward to" : "Primaries";
  // A zone that pulls and names nowhere to pull from can never fetch, so it
  // would claim its suffix and SERVFAIL it forever — checkZoneTransferConfig
  // refuses it, and this mirrors that refusal rather than inventing one. A
  // forwarder's empty `forward_to` is the opposite case: the server accepts
  // it deliberately (the zone still claims the suffix, and every query
  // beneath it becomes a SERVFAIL rather than a fall-through), so there is
  // no rule to mirror.
  const schema = useMemo(
    () => upstreamSchema(!forwarder, "Where to fetch from, e.g. 192.168.150.1"),
    [forwarder],
  );

  const edit = useInlineValueEdit({
    zoneId: zone.id,
    saved,
    schema,
    payload: (value) =>
      forwarder ? { forward_to: value } : { primaries: value, tsig_key_id: keyId },
    savedMessage: forwarder ? "Upstreams saved" : "Primaries saved",
    failedMessage: forwarder ? "Couldn't save the upstreams" : "Couldn't save the primaries",
  });

  /**
   * The note beside the value, and for a zone that pulls it is the state of
   * the last fetch.
   *
   * **Never a date-less state where a date exists to be shown.** Two of the
   * three carry one: the set it holds and when it arrived, and — under a
   * failure — the age of what it is still serving. The third, "nothing
   * fetched yet", carries none and must not be given one: nothing has ever
   * landed for a date to be about.
   *
   * Deliberately **not** derived through transferState: that function's
   * `expired` branch reads `expires_at`, which a stub is never given
   * (§9.11.8) but a row retyped from secondary can still carry — running one
   * through it would put a zone that is routing perfectly well on screen as
   * expired.
   */
  const failure = forwarder ? null : lastTransferError(zone);
  // See TransferBand's own `scheduled`: process-local, so it sits beside the
  // dated failure below rather than standing in for it.
  //
  // A stub only. A forwarder has no schedule at all, and a secondary's
  // TransferBand sits directly above this row and already carries it — two
  // copies of one countdown in one header group is the duplication every
  // other band here is careful to avoid.
  const scheduled = zone.type === "stub" ? schedulerNote(zone) : "";
  const upstreams = forwardTargets(saved);
  let note = "";
  if (forwarder) {
    note =
      upstreams.length === 0
        ? ""
        : `${upstreams.length} upstream${upstreams.length === 1 ? "" : "s"}`;
  } else if (zone.refreshed_at === 0) {
    note = zone.type === "stub" ? "no NS set yet" : "nothing pulled yet";
  } else if (failure) {
    // The age of what is being served, not of the fetch that failed to
    // replace it: the failure's own date is on the line below.
    note = `from ${relativeTime(zone.refreshed_at)}`;
  } else {
    note = `fetched ${relativeTime(zone.refreshed_at)}`;
  }

  return (
    <InlineValueRow
      edit={edit}
      caption={label}
      inputLabel={label}
      editLabel={forwarder ? "Edit upstreams" : "Edit primaries"}
      cancelLabel={forwarder ? "Cancel editing upstreams" : "Cancel editing primaries"}
      placeholder={forwarder ? "10.0.0.1, 10.0.0.2:5353" : "192.168.150.1:53"}
      inputClassName="w-[280px] shrink-0"
      // A secondary's row is followed by the allow-transfer band, which
      // supplies the group's bottom rule itself; for the two routing types
      // this row is the last of the header group and carries it.
      closesGroup={zone.type !== "secondary"}
      editExtra={signs && <TSIGKeySelect keys={keys} value={keyId} onChange={setKeyId} />}
      read={
        <>
          <span
            className={cn(
              "min-w-0 shrink truncate font-mono text-[12.5px]",
              saved === "" ? "text-muted-foreground" : "text-foreground",
            )}
            title={saved === "" ? undefined : saved}
          >
            {saved === "" ? "none" : saved}
          </span>
          {saved !== "" && <CopyButton value={saved} label={`Copy ${label.toLowerCase()}`} />}
          {signs && <TSIGKeyReadout keys={keys} keyId={zone.tsig_key_id} />}
          {note !== "" && (
            <span
              data-testid="upstream-note"
              className={cn(
                "shrink-0 font-mono text-[11px]",
                // A stale set is a warning about what is being routed to,
                // not a neutral fact about it.
                failure && zone.refreshed_at !== 0
                  ? "text-warning-foreground"
                  : "text-muted-foreground",
              )}
            >
              {note}
            </span>
          )}
        </>
      }
      footer={
        /* The failed fetch, in the fetcher's own words, with the date of the
           attempt that produced it and the age of what is still being
           served. All three or none: an error with no date is a claim about
           the present made by an unknown past, and a failure with no account
           of what is still being served does not say whether the suffix is
           down. */
        failure && !edit.editing ? (
          <div className="grid grid-cols-[13px_1fr] gap-x-2 border-t border-border-muted px-4 pt-[7px] pb-2">
            <AlertCircle aria-hidden="true" className="mt-0.5 size-3.5 text-warning-foreground" />
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2.5">
              <span className="shrink-0 font-mono text-[9.5px] font-semibold tracking-[0.14em] text-warning-foreground uppercase">
                Last fetch · {relativeTime(failure.at)}
              </span>
              <span
                data-testid="stub-fetch-error"
                title={failure.message}
                className="min-w-0 truncate font-mono text-[12.5px] text-warning-foreground"
              >
                {failure.message}
              </span>
              <span className="shrink-0 font-mono text-[11.5px] text-muted-foreground">
                {zone.refreshed_at === 0
                  ? "Answering nothing until the first fetch succeeds."
                  : `serving what arrived ${relativeTime(zone.refreshed_at)}`}
              </span>
              {/* The running scheduler's account of the retrying — the one
                  thing the dated error beside it cannot say. A stub has no
                  transfer band, so this row is where it belongs for one. */}
              {scheduled !== "" && (
                <span
                  data-testid="scheduler-note"
                  className="shrink-0 font-mono text-[11px] text-muted-foreground"
                >
                  {scheduled}
                </span>
              )}
            </div>
          </div>
        ) : null
      }
    />
  );
}

/**
 * The key a pulling zone signs with, at rest.
 *
 * The key's own *name* is what the peer's config calls it and so the only
 * useful thing to show. An id this list cannot name is only evidence of a
 * key deleted out from under the zone — which the API refuses, but a
 * hand-written row could still produce — when the list actually arrived. A
 * request that failed, or one still in flight, says nothing about the store,
 * so it gets the bare id rather than an accusation.
 */
function TSIGKeyReadout({ keys, keyId }: { keys: ReturnType<typeof useTSIGKeys>; keyId: number }) {
  if (keyId === 0) return <span className="shrink-0 font-mono text-[11px]">unsigned</span>;
  const named = keys.data?.find((k) => k.id === keyId);
  const label = named ? named.name : keys.isSuccess ? `#${keyId} (missing)` : `#${keyId}`;
  return (
    <span className="flex min-w-0 shrink items-center gap-1.5">
      <span className="min-w-0 truncate font-mono text-[11px]" title={label}>
        {label}
      </span>
      {named && <CopyButton value={named.name} label="Copy the TSIG key name" />}
    </span>
  );
}

/** The same choice as the zones list's create row: every key by name, plus
 * "unsigned". A stored id the list cannot name keeps an option of its own,
 * so saving an unrelated edit never silently unsigns the zone. */
function TSIGKeySelect({
  keys,
  value,
  onChange,
}: {
  keys: ReturnType<typeof useTSIGKeys>;
  value: number;
  onChange: (id: number) => void;
}) {
  const known = keys.data ?? [];
  const unlisted = value !== 0 && !known.some((k) => k.id === value);
  return (
    <NativeSelect
      aria-label="TSIG key"
      value={String(value)}
      onChange={(e) => onChange(Number(e.target.value))}
      className="h-7 w-[172px] shrink-0 font-mono"
    >
      <NativeSelectOption value="0">unsigned</NativeSelectOption>
      {known.map((k) => (
        <NativeSelectOption key={k.id} value={String(k.id)}>
          {k.name}
        </NativeSelectOption>
      ))}
      {unlisted && <NativeSelectOption value={String(value)}>#{value}</NativeSelectOption>}
    </NativeSelect>
  );
}

/**
 * The one thing a forwarder's page has to say out loud, and the reason a page
 * with a header, one row and two buttons does not read as one that failed to
 * load.
 *
 * A claimed suffix is claimed *outright*: §9.11.5 makes a query beneath it
 * with no reachable upstream a SERVFAIL rather than a fall-through past
 * dnsaur to the default resolvers. Why that is so is docs/dashboard.md's
 * job; that it is so belongs on screen.
 */
function ForwarderConsequence({ zone }: { zone: Zone }) {
  return (
    <div className="grid shrink-0 grid-cols-[13px_1fr] gap-x-2 px-4 py-2.5">
      <AlertCircle aria-hidden="true" className="mt-0.5 size-3.5 text-muted-foreground" />
      <span className="text-[12.5px] leading-normal text-pretty text-muted-foreground">
        <span className="font-mono text-foreground">{zone.name}</span> is claimed outright: with no
        upstream reachable, queries for it get SERVFAIL.
      </span>
    </div>
  );
}

// ── The notify-out band ───────────────────────────────────────────────────

const notifyToSchema = z.object({
  // Same reasoning as allow_transfer's own schema above: "valid" means
  // "parses as notify_to", which only parseNotifyTo can answer — see
  // lib/notify.ts's own top comment on why the client-side port is worth
  // having and why it is never the last word.
  value: z.string().superRefine((value, ctx) => {
    const result = parseNotifyTo(value);
    if (!result.ok) ctx.addIssue({ code: "custom", message: result.error });
  }),
});

/**
 * Presentation for one target's delivery state — the state column is the
 * only column carrying colour, so this table is the whole of the row's
 * colour budget and the reason a glance down it is the scan path. Serial,
 * when and target stay neutral; the error column borrows the state's own
 * tone only because it is that state's own explanation, not a second signal
 * competing with it.
 *
 * `gave_up` is drawn amber-on-hollow (a ring, no fill) — distinguishable
 * from `retrying`'s filled amber dot without reading as a failure. It isn't
 * one: a round rests after MaxNotifyAttempts and starts fresh on the zone's
 * next edit, no operator action required, which is also why its label below
 * is "not acknowledged" rather than "gave up".
 */
const NOTIFY_STATE_STYLES: Record<
  ZoneNotify["state"],
  { text: string; weight: string; dot: string }
> = {
  current: {
    text: "text-muted-foreground",
    weight: "font-normal",
    dot: "border-success bg-success",
  },
  never: {
    text: "text-muted-foreground",
    weight: "font-normal",
    dot: "border-border bg-transparent",
  },
  retrying: {
    text: "text-warning-foreground",
    weight: "font-semibold",
    dot: "border-warning bg-warning",
  },
  gave_up: {
    text: "text-warning-foreground",
    weight: "font-semibold",
    dot: "border-warning bg-transparent",
  },
};

/** The word a target's row shows for its state — never "gave up" (see
 * NOTIFY_STATE_STYLES's own comment on why). `retrying` carries its
 * attempt count rather than hardcoding the budget, so a future change to
 * MaxNotifyAttempts on the server shows up here without a redeploy — except
 * at `attempts === 0`, which is not a retry that has happened yet. The API
 * derives `retrying` from `notified_at != 0, attempts == 0` for any target
 * that is behind, which is also the state of every previously-current
 * target in the window between a serial bump and the pass's first attempt
 * (the mutation's response returns before the async pass runs). "try 0/5"
 * in that window reads as nonsense, so the counter is shown only once an
 * attempt has actually been made. */
function notifyStateLabel(row: ZoneNotify): string {
  switch (row.state) {
    case "current":
      return "current";
    case "never":
      return "never notified";
    case "retrying":
      return row.attempts > 0 ? `retrying · try ${row.attempts}/${row.max_attempts}` : "retrying";
    case "gave_up":
      return "not acknowledged";
  }
}

/** "—" for a target that has never been delivered — there is no serial to
 * show — the last serial it actually acknowledged otherwise. */
function notifySerial(row: ZoneNotify): string {
  return row.state === "never" ? "—" : String(row.notified_serial);
}

/** What a target's row is dated by: `notified_at` for anything that has
 * ever landed, `created_at` — when the target was added — for one that
 * hasn't. `never`'s `notified_at` is always 0, and dating the row by it
 * would read as "0 ago" rather than what it actually means. */
function notifyWhen(row: ZoneNotify): string {
  if (row.state === "never") return `added ${relativeTime(row.created_at)}`;
  return relativeTime(row.notified_at);
}

/** One target's delivery state, in the grid AllowTransferBand's sibling row
 * hands down — target, state, serial, when, error, exactly the artboard's
 * five columns. */
function NotifyTargetRow({ row }: { row: ZoneNotify }) {
  const style = NOTIFY_STATE_STYLES[row.state];
  return (
    <div
      data-testid="notify-target-row"
      className="grid grid-cols-[minmax(0,1fr)_176px_104px_116px_minmax(0,150px)] items-baseline gap-3 py-[3px]"
    >
      <span
        title={row.target}
        className={cn(
          "min-w-0 truncate font-mono text-xs",
          row.state === "never" ? "text-muted-foreground" : "text-foreground",
        )}
      >
        {row.target}
      </span>
      <span className="flex min-w-0 items-center gap-1.5">
        <span aria-hidden="true" className={cn("size-1.5 shrink-0 border", style.dot)} />
        <span className={cn("min-w-0 truncate font-mono text-[11.5px]", style.text, style.weight)}>
          {notifyStateLabel(row)}
        </span>
      </span>
      <span className="font-mono text-[11.5px] whitespace-nowrap text-muted-foreground">
        {notifySerial(row)}
      </span>
      <span className="truncate font-mono text-[11px] text-muted-foreground">
        {notifyWhen(row)}
      </span>
      <span
        title={row.last_error || undefined}
        className={cn("min-w-0 truncate text-right font-mono text-[11.5px]", style.text)}
      >
        {row.last_error}
      </span>
    </div>
  );
}

/**
 * Who this zone tells when it changes, and how each of them is doing — the
 * outbound twin of AllowTransferBand's row, sharing its 152px label gutter
 * so the two captions ("Transfers out" there, "Notify out" here) land on
 * one edge.
 *
 * The read line is the saved `notify_to` value plus a roll-up — "no
 * targets" / "all 4 current" / "3 of 4 current, 1 never notified" / "2 of 4
 * behind" (`notifyRollup`, lib/notify.ts) — rather than one row per target:
 * four identical "current" rows say nothing four times. Only a target that
 * is *behind* (`retrying` or `gave_up` — deliberately not `never`, see
 * `isNotifyBehind`'s own comment) earns a row without asking; the roll-up
 * doubles as the disclosure that expands the rest, the same "click the
 * summary to see everything" shape BuiltinsDisclosure already uses for the
 * built-in zones row (list.tsx). A never-notified target is named rather
 * than folded into "current" even when nothing is behind — see
 * `notifyRollup`'s own comment for why, and `notifyRestLabel`'s for the
 * collapsed "+N" hint below the row list, which has the identical risk.
 *
 * Mounted only for a primary or a secondary — the same *positive* set
 * AllowTransferBand's own call site gates on, and for the same reason: a
 * stub or a forwarder reaching this control would be a control that fails
 * on click (notify_to is refused with 400 on either — see
 * checkZoneTransferConfig, internal/api/zones_handlers.go) rather than one
 * the screen declined to draw. The artboard gates this row on `!builtIn`
 * alone, which would still draw it for those two types; the positive set is
 * the deliberate correction, not a reading of the artboard.
 */
function NotifyBand({ zone }: { zone: Zone }) {
  const [showAll, setShowAll] = useState(false);
  const notifies = useZoneNotifies(zone.id);
  const edit = useInlineValueEdit({
    zoneId: zone.id,
    saved: zone.notify_to,
    schema: notifyToSchema,
    payload: (value) => ({ notify_to: value }),
    savedMessage: "Notify targets saved",
    failedMessage: "Couldn't save notify targets",
  });

  const hasValue = zone.notify_to !== "";
  const rows = notifies.data ?? [];
  const rollup = notifyRollup(rows);
  // A query that hasn't landed yet (or never will) reads as zero targets
  // above, which would otherwise flash "no targets" ahead of a value this
  // page already knows is set — so pending/error get their own words
  // instead of borrowing the empty state's.
  const summaryLabel = notifies.isPending
    ? "loading…"
    : notifies.isError
      ? "couldn't load targets"
      : rollup.label;
  const behindRows = rows.filter((r) => isNotifyBehind(r.state));
  const restRows = rows.filter((r) => !isNotifyBehind(r.state));

  return (
    <InlineValueRow
      edit={edit}
      caption={
        <>
          Notify
          <br />
          out
        </>
      }
      inputLabel="Notify to"
      editLabel="Edit notify targets"
      cancelLabel="Cancel editing notify targets"
      placeholder="10.0.0.2, 10.0.0.3 key:ns2"
      inputClassName="flex-1"
      topRule
      read={
        <>
          <span
            className={cn(
              "min-w-0 shrink truncate font-mono text-[12.5px]",
              hasValue ? "text-foreground" : "text-muted-foreground",
            )}
            title={hasValue ? zone.notify_to : undefined}
          >
            {hasValue ? zone.notify_to : "No targets are notified."}
          </span>
          {hasValue && <CopyButton value={zone.notify_to} label="Copy notify targets" />}
          {/* Doubles as the disclosure: with anything to expand it is a real
              button (Enter/Space-operable, aria-expanded carries the state);
              with nothing to show it is inert text rather than a control
              that does nothing on click. */}
          {rollup.total > 0 ? (
            <button
              type="button"
              onClick={() => setShowAll((v) => !v)}
              aria-expanded={showAll}
              title={showAll ? "Show only targets behind" : "Show every target"}
              className={cn(
                "flex shrink-0 cursor-pointer items-center gap-1 font-mono text-[11px] whitespace-nowrap hover:text-foreground",
                rollup.behind > 0 ? "text-warning-foreground" : "text-muted-foreground",
              )}
            >
              <ChevronRight
                aria-hidden="true"
                className={cn("size-2.5 shrink-0 transition-transform", showAll && "rotate-90")}
              />
              {summaryLabel}
            </button>
          ) : (
            <span className="shrink-0 font-mono text-[11px] whitespace-nowrap text-muted-foreground">
              {summaryLabel}
            </span>
          )}
        </>
      }
      footer={
        /* Behind targets always show; the rest sit behind the caret above.
           Independent of read/edit — the delivery state of the *saved*
           targets stays worth showing while a new value is being typed. */
        <div className="px-4">
          {behindRows.length > 0 && (
            <div className="mt-1.5 border-t border-border-muted pt-1 pb-2">
              {behindRows.map((row) => (
                <NotifyTargetRow key={row.target} row={row} />
              ))}
              {!showAll && restRows.length > 0 && (
                <button
                  type="button"
                  onClick={() => setShowAll(true)}
                  className="cursor-pointer py-[3px] font-mono text-[11px] text-muted-foreground hover:text-foreground"
                >
                  {notifyRestLabel(restRows)}
                </button>
              )}
            </div>
          )}
          {showAll && restRows.length > 0 && (
            <div
              className={cn(
                "pb-2",
                behindRows.length === 0 && "mt-1.5 border-t border-border-muted pt-1",
              )}
            >
              {restRows.map((row) => (
                <NotifyTargetRow key={row.target} row={row} />
              ))}
            </div>
          )}
        </div>
      }
    />
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
  /**
   * `/zones/wat` never named a zone, so it is the catch-all's question and
   * not a zone that failed to load. Answered before any hook runs, which is
   * also what keeps `GET /zones/NaN` — a 400 with nothing useful to render —
   * off the wire.
   */
  if (!Number.isInteger(zoneId) || zoneId <= 0) return <NotFound />;
  return <ZoneDetailFor zoneId={zoneId} />;
}

/**
 * Whether this page may write records into a zone of this type — the whole
 * of `recordsReadOnly` below, as a function, because the keyboard shortcut
 * that opens the add band has to ask it before the loaded/unloaded branch
 * the rest of the type flags live behind. One spelling for one rule: two
 * would be two chances to disagree about whether a control exists and
 * whether the key that opens it works.
 *
 * `primary` is the only type left once the four that own their contents
 * elsewhere are taken out — see recordsReadOnly for which and why — and an
 * unloaded zone is read-only, because nothing is known about it yet.
 *
 * `managed` is the fifth way records stop being this box's to write: a
 * replica's records come from the main by AXFR, and a write to one is a 409
 * before the store is touched (spec §7).
 */
function recordsAreReadOnly(type: Zone["type"] | undefined, managed: boolean): boolean {
  return managed || type !== "primary";
}

function ZoneDetailFor({ zoneId }: { zoneId: number }) {
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
  // Zone definitions and their records are the main's on a replica (spec
  // §7). Reads, the filter, export and "Refresh now" all stay live —
  // fetching the current version of a copy is this box's own act.
  const managedBy = useManagedBy();

  /** The filter field, so `/` can put the caret in it. A ref rather than an
   * id lookup: the bar is not rendered for a routing type, and a ref that is
   * null says so without a query that finds nothing. */
  const filterRef = useRef<HTMLInputElement>(null);
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
  /** Whether the bulk-TTL prompt is open. It has no target of its own: what
   * it applies to is the filtered set on screen when it was opened. */
  const [ttlOpen, setTTLOpen] = useState(false);
  const [deleteZoneOpen, setDeleteZoneOpen] = useState(false);
  const [deleteRecordTarget, setDeleteRecordTarget] = useState<ZoneRecord | null>(null);
  const refreshZone = useRefreshZone();
  const setRecordsTTL = useSetZoneRecordsTTL();

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
    // A stub does not transfer, and calling what it does a transfer would be
    // wrong in the one way the type exists to be right about: it asks its
    // master two ordinary questions (SOA and NS with glue) precisely so it
    // needs no allow_transfer permission on the far end. The endpoint is
    // shared — POST /zones/{id}/refresh serves both, and refuses every other
    // type with 400 — but the word is not.
    const fetches = zone.data?.type === "stub";
    refreshZone.mutate(zoneId, {
      onSuccess: (result) =>
        toast.success(
          `${fetches ? "Fetched" : "Transferred"} ${result.records} records from ${result.primary}`,
        ),
      // A short toast to close the interaction; the row carries the detail.
      onError: () => toast.error(fetches ? "The fetch failed" : "The transfer failed"),
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
  /** Whether either filter is narrowing the list — what makes "the filtered
   * records" a set the user chose rather than the whole zone. */
  const filtered = nameFilter.trim() !== "" || typeFilter !== "";

  /** One form at a time — see `addOpen`'s comment.
   *
   * useCallback because the keyboard listener below names it: setState
   * functions are stable, so this is too, and the listener is installed once
   * rather than torn down and re-added on every render. */
  const openAdd = useCallback(() => {
    setEditingId(null);
    setAddOpen(true);
    setAddCue((c) => c + 1);
  }, []);

  function openEdit(target: ZoneRecord) {
    setAddOpen(false);
    setEditingId(target.id);
  }

  /** The `n` shortcut's gate, asked before the branch below that would make
   * it unreachable from an effect. Same rule as `recordsReadOnly`. */
  const readOnly = recordsAreReadOnly(zone.data?.type, managedBy !== "");

  /**
   * The three keys the record grid is worth having: `/` to filter, `n` to
   * add, Esc to close whatever is open.
   *
   * On the window, like the command palette's own listener, because they are
   * about the page rather than about whichever element happens to hold focus.
   *
   * **`/` and `n` are inert while focus is in a field**, and that is not
   * politeness: "n" belongs in a record name and "/" in a filter, so a
   * shortcut that fired there would eat the text being typed. Esc is
   * deliberately *not* gated that way — the form it closes is where the focus
   * already is, so a rule that skipped fields would make it close nothing.
   *
   * Esc also stands down while a dialog is open. The dialog closes itself on
   * that key, and taking the record form down behind it would leave the page
   * a step further back than one press asked for.
   */
  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.metaKey || event.ctrlKey || event.altKey) return;

      if (event.key === "Escape") {
        if (document.querySelector("[role='dialog'],[role='alertdialog']")) return;
        setAddOpen(false);
        setEditingId(null);
        return;
      }

      const target = event.target as HTMLElement | null;
      if (
        target instanceof HTMLInputElement ||
        target instanceof HTMLTextAreaElement ||
        target instanceof HTMLSelectElement ||
        target?.isContentEditable === true
      ) {
        return;
      }
      if (event.key === "/" && filterRef.current) {
        event.preventDefault();
        filterRef.current.focus();
        return;
      }
      if (event.key === "n" && !readOnly) {
        event.preventDefault();
        openAdd();
      }
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [openAdd, readOnly]);

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

  /**
   * Retune the filtered set to one TTL.
   *
   * The ids are read at submit time from what is on screen, so the request
   * says exactly what the button said — and the server's own refusal (the
   * RRSet rule, a TTL past the RFC 2181 ceiling) is shown in its words rather
   * than pre-empted by a second copy of the rule here.
   */
  function onConfirmTTL(value: string) {
    setRecordsTTL.mutate(
      { zoneId, ids: shownRecords.map((r) => r.id), ttl: Number(value) },
      {
        onSuccess: () => {
          toast.success(
            `TTL set on ${shownRecords.length} ${shownRecords.length === 1 ? "record" : "records"}`,
          );
          setTTLOpen(false);
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't set the TTL"),
      },
    );
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
        onError: (err) => {
          // Already gone — deleted from somewhere else. The row is stale
          // rather than the delete being wrong, and the mutation has already
          // asked for the list again (refetchIfGone, use-zones.ts).
          if (err instanceof ApiError && err.status === 404) {
            toast.error("That record no longer exists");
            if (editingId === target.id) setEditingId(null);
            setDeleteRecordTarget(null);
            return;
          }
          toast.error(`Couldn't delete ${target.name}`);
        },
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
    // A 404 is a zone that was deleted — from another tab, or by another
    // admin — rather than a page that failed to load, and "try refreshing"
    // is advice that cannot work: the refresh 404s again. The way out is
    // the list, so that is what this offers.
    const deleted = zone.error instanceof ApiError && zone.error.status === 404;
    return (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>
            {deleted ? "This zone no longer exists" : "Couldn't load this zone"}
          </AlertTitle>
          <AlertDescription>
            {deleted ? (
              <Link to="/zones" className="underline underline-offset-2">
                Back to zones
              </Link>
            ) : (
              "Try refreshing the page."
            )}
          </AlertDescription>
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
  /**
   * The two types Milestone D6 adds, which are the same shape on this page:
   * neither holds authored data, both claim a suffix and route it, and
   * neither has an SOA anybody here may write.
   *
   * `isRouting` is the artboard's `nameless`, and it is what every dropped
   * band is gated on — the SOA band, the create row, the record filter,
   * allow-transfer and notify-out. They differ in exactly one place: a stub
   * has an NS set worth *seeing*, so it keeps the records grid (read-only),
   * while a forwarder has no records at all and drops the grid with them.
   */
  const isForwarder = z.type === "forwarder";
  const isStub = z.type === "stub";
  const isRouting = isForwarder || isStub;
  /**
   * Both routing types join the built-in and the secondary here — the
   * artboard's `isEditable` negated exactly.
   *
   * A stub's records are the NS set it fetched, and the next fetch replaces
   * the whole set through the same DiffRecords the transfer uses, so a
   * hand-written record survives only until the schedule comes round — the
   * trap the secondary's own read-only-ness exists to close. A forwarder's
   * case is simpler and stronger: it answers from no records at all
   * (Zone.Answer returns handled=false for the type), so a record written
   * into one would never be served by anything.
   *
   * The server refuses a stub's writes too (recordWriteRefusal names
   * `internal`, `secondary` and `stub`), so hiding the controls there is the
   * UI agreeing with the API rather than standing in for it. A **forwarder**
   * is the one case where the server would still accept the write, and
   * deliberately: nothing overwrites a forwarder's records, so a write into
   * one is inert rather than lost, which is not a 409's complaint to make.
   * Hiding the control is the whole of the guard there.
   */
  const recordsReadOnly = recordsAreReadOnly(z.type, managedBy !== "");

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
      ? "Nothing transferred yet."
      : isStub
        ? // Present tense only while it is true. A stub with a recorded
          // failure is not fetching, it has failed — and the primaries row
          // above already carries that error with its date, so this line says
          // the one thing left: there is no set.
          lastTransferError(z)
          ? "No NS set yet."
          : `Fetching the NS set from ${z.primaries}`
        : // Not "add one above": the add band is closed until Add record
          // opens it, so there is nothing above to point at.
          "No records yet.";
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
      {managedBy !== "" && <ManagedNotice peer={managedBy} />}

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
        {/* Fetched rather than pulled, because the difference is the whole
            type: a stub asks two ordinary questions instead of transferring,
            and so needs no allow_transfer permission on the master. A
            forwarder gets no marker of its own — it has no records for the
            word "read-only" to be about. */}
        {isStub && (
          <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-[9.5px] tracking-[0.12em] text-muted-foreground uppercase">
            <ArrowDownToLine className="size-3" aria-hidden="true" />
            Fetched · Read-only
          </span>
        )}
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {!recordsReadOnly && (
            <Button type="button" size="sm" onClick={openAdd}>
              <Plus />
              Add record
            </Button>
          )}
          {/* The primary action for a zone that pulls: not "write a record",
              which it cannot do, but "go and get the current version now".

              `secondary || stub` — the API's own gate, verbatim
              (pullsFromAMaster in zones_handlers.go, which answers everything
              else 400 "only secondary and stub zones pull from a master"). A
              forwarder is deliberately outside it: it has no master and
              nothing to fetch, so the button would be a no-op that returns an
              error. */}
          {(isSecondary || isStub) && (
            <Button type="button" size="sm" onClick={onRefreshNow} disabled={refreshZone.isPending}>
              <RotateCw />
              {refreshZone.isPending ? (isStub ? "Fetching…" : "Transferring…") : "Refresh now"}
            </Button>
          )}
          {/* Export reads rather than writes, so it renders for the zones
              every other control here omits: a built-in, a secondary and a
              stub are all exportable. The one exception is a forwarder, which
              holds no records to render into a file. Import and the rest stay
              behind their own guards — see ZoneFileActions, which owns both
              gates. */}
          <ZoneFileActions zone={z} />
          {!isInternal && (
            <>
              <span aria-hidden="true" className="h-5 w-px bg-border" />
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={onToggleEnabled}
                disabled={updateZone.isPending || managedBy !== ""}
              >
                {z.enabled ? "Disable zone" : "Enable zone"}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={managedBy !== ""}
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
          TransferBand's own comment.

          A forwarder and a stub get **neither**, and that is the correction
          this branch carries: as `isSecondary ? … : …` alone it put both of
          them in the else branch and offered an editable SOA form for an SOA
          that is not theirs to author. A forwarder's is a formality nothing
          reads (it answers from no records at all); a stub's arrives with
          every fetch and is overwritten by the next one, exactly as a
          secondary's is. What they get instead is the upstream row below. */}
      {isSecondary ? (
        <TransferBand zone={z} asOf={zone.dataUpdatedAt} />
      ) : isRouting ? null : (
        <SoaBand zone={z} />
      )}

      {/* Where this zone goes to get what it serves, and what it signs the
          asking with — one row for all three types that have somewhere to
          go, its caption switching between FORWARD TO and PRIMARIES. A
          secondary's belongs here rather than as read-only cells in the band
          above: PATCH /zones/{id} takes `primaries` and `tsig_key_id` on
          one, and a primary that changes address otherwise meant deleting
          the secondary and rebuilding it, losing allow_transfer and
          notify_to with it. See UpstreamRow's own comment. */}
      {(isRouting || isSecondary) && <UpstreamRow zone={z} />}

      {/* …and, for a forwarder, what happens when none of them answers. Not
          decoration: see ForwarderConsequence. */}
      {isForwarder && <ForwarderConsequence zone={z} />}

      {/* Who may transfer this zone, and who last did — applies to both a
          primary and a secondary (a secondary re-serves what it pulled), so
          it is mounted for both rather than folded into either band above.
          Gated on the *positive* set, not just !isInternal: §9.5.3 refuses
          internal/stub/forwarder zones with NOTAUTH regardless of their ACL,
          and handleZonePatch 409s every PATCH to a built-in zone outright —
          an editable field here for one of those would be exactly the kind
          of control this header already omits rather than lets fail on
          click (see isInternal's own comment above). D6 is what made that
          gating load-bearing rather than merely careful: CREATABLE_TYPES
          (list.tsx) is now all four types and the create row renders a
          select over it, so stub and forwarder zones arrive here through
          the ordinary path. Gating on the allowed set is what kept this
          correct across that change without an edit. See
          AllowTransferBand's own comment. */}
      {(z.type === "primary" || isSecondary) && <AllowTransferBand zone={z} />}

      {/* Who this zone tells when it changes, and how each of them is
          doing. Same positive gate as AllowTransferBand immediately above,
          and for the same reason — see NotifyBand's own comment on why
          that is a deliberate correction of the artboard's `!builtIn`. */}
      {(z.type === "primary" || isSecondary) && <NotifyBand zone={z} />}

      {/* Filter bar — dropped for both routing types, a stub included. A
          stub's grid is its fetched NS set: three or four rows, all named
          "@", all of type NS. There is nothing to filter, and a filter over
          nothing is a control that only ever hides the whole list. */}
      {!isRouting && (
        <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
          <Input
            ref={filterRef}
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
          {/* Plain, and only for the keys that actually do something here: a
              zone whose records are read-only has no add band for `n` to
              open. */}
          <span
            data-testid="record-shortcuts"
            className="ml-auto flex shrink-0 items-center gap-1.5 text-[10.5px] text-muted-foreground"
          >
            <Kbd>/</Kbd> filter
            {!recordsReadOnly && (
              <>
                <Kbd>n</Kbd> add record
              </>
            )}
          </span>
          <span className="font-mono text-[10.5px] text-muted-foreground">
            {shownRecords.length} {shownRecords.length === 1 ? "record" : "records"}
          </span>
          {/* Only while a filter is on, and only where records may be
              written. With no filter the button would read as "retune the
              whole zone", which is a much larger claim than one press has any
              way to confirm — and the set it names has to be the one on
              screen for the count in its own label to mean anything. */}
          {!recordsReadOnly && filtered && shownRecords.length > 0 && (
            <Button type="button" size="sm" variant="ghost" onClick={() => setTTLOpen(true)}>
              Set TTL for {shownRecords.length} filtered{" "}
              {shownRecords.length === 1 ? "record" : "records"}
            </Button>
          )}
        </div>
      )}

      {/* Everything from here down is about records, so a forwarder — which
          has none, and can never have one — takes none of it: not the grid,
          not the header above it, not the stale-records alert, and not the
          add band. Its page ends at the consequence line above, and that
          shortness is the design rather than a load that failed. A stub keeps
          all of it except the add band, because a fetched NS set is worth
          seeing. */}
      {!isForwarder && (
        <>
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
        </>
      )}

      <RenameDialog
        targetId={ttlOpen ? z.id : null}
        title="Set TTL"
        description={`Every one of the ${shownRecords.length} records this filter shows gets this TTL. Records outside the filter keep theirs.`}
        label="TTL (seconds)"
        initialName=""
        placeholder="300"
        schema={bulkTTLSchema}
        submitLabel="Set TTL"
        pendingLabel="Saving…"
        isPending={setRecordsTTL.isPending}
        onSubmit={onConfirmTTL}
        onClose={() => setTTLOpen(false)}
      />

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
