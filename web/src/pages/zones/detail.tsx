import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import {
  AlertCircle,
  ChevronLeft,
  ChevronRight,
  Pencil,
  Plus,
  Trash2,
  TriangleAlert,
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
  useUpdateZone,
  useUpdateZoneRecord,
  useZone,
  useZoneRecords,
} from "../../hooks/use-zones";
import { StaleDataAlert } from "../../components/stale-data-alert";

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
 * The create/edit row: one persistent band, not a dialog — mirroring
 * dns.tsx's RecordFormRow. `editing` binds it to an existing record (Save,
 * PUT); `null` is the add case (Add, POST, and the row re-clears itself for
 * the next one — adding one record is often adding several).
 *
 * `focusCue` is a bump-to-refocus signal: the header's "Add record" button
 * needs to both cancel any in-progress edit *and* focus Name even when the
 * row is already in add mode (where `editing` alone wouldn't change and so
 * wouldn't retrigger the effect below).
 */
function RecordFormRow({
  zoneId,
  editing,
  focusCue,
  onDone,
}: {
  zoneId: number;
  editing: ZoneRecord | null;
  focusCue: number;
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

  // Re-seed whenever the target or the header's focus cue changes —
  // `form.reset` also clears any server-error set on rdata by a previous
  // attempt, so switching context never carries a stale parser message.
  useEffect(() => {
    const defaults = recordFormDefaults(editing);
    form.reset(defaults);
    form.setFocus("name");
  }, [editing, focusCue, form]);

  const type = form.watch("type");

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
        className={cn(
          "shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]",
          isEdit && "bg-primary/5",
        )}
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
                    aria-label="Zone record name"
                    placeholder="@ or bifrost"
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
                    placeholder={DATA_PLACEHOLDER[type]}
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
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
              aria-label={isEdit ? "Cancel editing" : "Clear the form"}
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
            <FormField control={form.control} name="name" render={() => <FormMessage />} />
            <FormField control={form.control} name="type" render={() => <FormMessage />} />
            <FormField control={form.control} name="ttl" render={() => <FormMessage />} />
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
  onEdit,
  onDelete,
}: {
  record: ZoneRecord;
  readOnly: boolean;
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
        <div className="flex items-center justify-end gap-1">
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Edit ${label}`}
            onClick={onEdit}
          >
            <Pencil />
          </Button>
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Delete ${label}`}
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
  const updateZone = useUpdateZone();
  const deleteZone = useDeleteZone();
  const deleteRecord = useDeleteZoneRecord();

  const [nameFilter, setNameFilter] = useState("");
  const [typeFilter, setTypeFilter] = useState<"" | RecordType>("");
  const [editing, setEditing] = useState<ZoneRecord | null>(null);
  const [focusCue, setFocusCue] = useState(0);
  const [deleteZoneOpen, setDeleteZoneOpen] = useState(false);
  const [deleteRecordTarget, setDeleteRecordTarget] = useState<ZoneRecord | null>(null);

  const allRecords = useMemo(() => records.data ?? [], [records.data]);
  const shownRecords = useMemo(() => {
    const q = nameFilter.trim().toLowerCase();
    return allRecords.filter(
      (r) =>
        (typeFilter === "" || r.type === typeFilter) &&
        (q === "" || r.name.toLowerCase().includes(q)),
    );
  }, [allRecords, nameFilter, typeFilter]);

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
          if (editing?.id === target.id) setEditing(null);
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
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        {allRecords.length === 0
          ? "No records yet. Add one above and dnsaur will answer for this zone directly."
          : "No records match this filter."}
      </p>
    );
  } else {
    body = shownRecords.map((record) => (
      <RecordRow
        key={record.id}
        record={record}
        readOnly={isInternal}
        onEdit={() => setEditing(record)}
        onDelete={() => setDeleteRecordTarget(record)}
      />
    ));
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
        <Badge variant={z.enabled ? "success-light" : "secondary"}>
          {z.enabled ? "Enabled" : "Disabled"}
        </Badge>
        <div className="ml-auto flex shrink-0 items-center gap-2">
          {isInternal ? (
            // Same marker, same wording as the zones list's Actions cell
            // (list.tsx's ZoneRow) — not merely omitting the buttons, but
            // saying outright why they're gone.
            <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
              Built-in
            </span>
          ) : (
            <>
              <Button
                type="button"
                size="sm"
                onClick={() => {
                  setEditing(null);
                  setFocusCue((c) => c + 1);
                }}
              >
                <Plus />
                Add record
              </Button>
              <Button
                type="button"
                size="sm"
                variant="outline"
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

      {/* SOA band */}
      <SoaBand zone={z} />

      {/* Filter bar */}
      <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
        <Input
          value={nameFilter}
          onChange={(e) => setNameFilter(e.target.value)}
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
            onChange={(e) => setTypeFilter(e.target.value as "" | RecordType)}
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

      {/* Create/edit row — omitted for a built-in zone; see isInternal's
          own comment. */}
      {!isInternal && (
        <RecordFormRow
          zoneId={z.id}
          editing={editing}
          focusCue={focusCue}
          onDone={() => setEditing(null)}
        />
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

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
