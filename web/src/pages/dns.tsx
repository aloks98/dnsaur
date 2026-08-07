import { useEffect, useMemo, useState, type ReactNode } from "react";
import { PencilLine, Trash2, TriangleAlert, X } from "lucide-react";
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
import { ApiError } from "../api/client";
import type { LocalRecord } from "../api/types";
import { useAddRecord, useDeleteRecord, useRecords, useUpdateRecord } from "../hooks/use-records";
import { StaleDataAlert } from "../components/stale-data-alert";
import { isValidIPv4, isValidIPv6 } from "../lib/schemas";

// --- validation --------------------------------------------------------
// Mirrors internal/api/records_handlers.go's normalizeRecord closely enough
// that a record accepted here is one the API/DNS layer accepts — the
// server is the one source of truth for what's a valid record; this is
// only a client-side echo of it so typos surface before a round trip
// instead of after. Per Task 10's lesson (an over-strict IPv6 check
// wrongly blocked a valid zone-id matcher), the bias throughout is toward
// *not* rejecting something the server would accept.

const RECORD_TYPES = [
  "A",
  "AAAA",
  "CNAME",
  "TXT",
] as const satisfies readonly LocalRecord["type"][];

type RecordType = LocalRecord["type"];

/** The server lowercases the name and trims a trailing dot itself before
 * validating (and re-derives the wildcard/base-domain split from that
 * normalized form) — so this check must accept mixed case and a trailing
 * dot rather than rejecting them, matching normalizeRecord exactly:
 * TrimSuffix(TrimSpace(name), "."), then strip a leading "*." wildcard,
 * then require the remainder to be non-empty, contain a dot, and be free
 * of spaces/slashes/backslashes. */
const recordNameSchema = z
  .string()
  .trim()
  .refine((value) => {
    const name = value.replace(/\.$/, "").replace(/^\*\./, "");
    return name !== "" && name.includes(".") && !/[ /\\]/.test(name);
  }, "Enter a domain, e.g. nas.home.lan (a *.parent wildcard is allowed)");

const ttlSchema = z
  .string()
  .trim()
  .regex(/^\d+$/, "TTL must be a whole number of seconds")
  .refine(
    (value) => Number(value) >= 1 && Number(value) <= 86400,
    "TTL must be between 1 and 86400 seconds",
  );

/** What the Value field must hold depends on the type selected beside it,
 * so it's checked at the object level (with an explicit `path`) rather
 * than as a standalone field schema — that's what gives the refinement
 * both fields at once.
 *
 * AAAA is checked with the no-zone form of isValidIPv6: the server parses
 * AAAA values with Go's net.ParseIP (not netip.ParseAddr, unlike the
 * client matcher in groups-clients.tsx), which has no concept of an RFC
 * 4007 zone id. The one case that *is* knowingly under-strict is the
 * IPv4-mapped address ("::ffff:192.168.1.1"), which Go's To4() catches and
 * 400s as an AAAA: telling "mapped" from the legitimate "embedded"
 * dotted-quad forms needs a real, bit-level IPv6 parser, and getting that
 * narrowing wrong in either direction is how the NAT64 false-reject bug
 * happened in the first place. Left to the server: a false accept costs
 * one 400 toast, a false reject blocks a valid record outright. */
function recordValueError(type: RecordType, value: string): string | null {
  switch (type) {
    case "A":
      return isValidIPv4(value) ? null : "Enter a valid IPv4 address, e.g. 192.168.1.10";
    case "AAAA":
      return isValidIPv6(value) ? null : "Enter a valid IPv6 address, e.g. 2001:db8::1";
    case "CNAME":
      return value.replace(/\.$/, "").includes(".")
        ? null
        : "Enter the domain this name points to, e.g. target.example.com";
    case "TXT":
      return value ? null : "Enter the text value to return";
  }
}

const recordFormSchema = z
  .object({
    name: recordNameSchema,
    type: z.enum(RECORD_TYPES),
    value: z.string().trim(),
    ttl: ttlSchema,
  })
  .superRefine((values, ctx) => {
    const error = recordValueError(values.type, values.value);
    if (error) ctx.addIssue({ code: "custom", message: error, path: ["value"] });
  });

type RecordFormValues = z.infer<typeof recordFormSchema>;

const DEFAULT_TTL = 300;

function recordFormDefaults(record: LocalRecord | null): RecordFormValues {
  return {
    name: record?.name ?? "",
    type: record?.type ?? "A",
    value: record?.value ?? "",
    ttl: String(record?.ttl ?? DEFAULT_TTL),
  };
}

/**
 * The Value column is the only per-type part of this screen — its header,
 * placeholder and hint all change together, which is what lets one form row
 * serve four record types instead of four separate forms.
 */
const VALUE_META: Record<RecordType, { header: string; placeholder: string; hint: string }> = {
  A: {
    header: "Value · IPv4",
    placeholder: "192.168.150.5",
    hint: "Dotted-quad IPv4. Stored verbatim.",
  },
  AAAA: {
    header: "Value · IPv6",
    placeholder: "fd00::1",
    hint: "IPv6 only — an IPv4-mapped form like ::ffff:192.0.2.1 is rejected.",
  },
  CNAME: {
    header: "Value · Target",
    placeholder: "nas.home.lan",
    hint: "Another domain. Lowercased on save; the trailing dot is stripped.",
  },
  TXT: {
    header: "Value · Text",
    placeholder: "v=spf1 -all",
    hint: "Any non-empty string. Not chunked at 255 bytes.",
  },
};

/** A category tag, not a verdict — so the four differ by hue only to be
 * told apart at a glance, with none of them reading as good or bad the way
 * the block/allow badges elsewhere deliberately do. */
const TYPE_VARIANT: Record<RecordType, NonNullable<BadgeProps["variant"]>> = {
  A: "primary-light",
  AAAA: "info-light",
  CNAME: "secondary",
  TXT: "warning-light",
};

/** One declaration of the column geometry, shared by the header, the form
 * row and every record row. Three copies of a five-column template is how
 * they drift out of alignment. */
const GRID = "grid grid-cols-[1fr_104px_1fr_104px_96px] items-center gap-3.5 px-4";

/** Mono, with the wildcard prefix picked out. `*.` is the difference
 * between one name and every subdomain under it, and at 13px mono it is
 * two characters that otherwise disappear into the string. */
function RecordName({ name }: { name: string }) {
  const wild = name.startsWith("*.");
  return (
    <span className="truncate font-mono text-sm" title={name}>
      {wild && <span className="font-semibold text-primary">*.</span>}
      {wild ? name.slice(2) : name}
    </span>
  );
}

/**
 * The add/edit row: one persistent band at the top of the table rather than
 * a dialog or a side sheet.
 *
 * Adding a record here is a four-field act on a page whose whole content is
 * those same four fields per row, so the form is the table's first row and
 * lines up with it column for column. Editing reuses it — the alternative
 * was a second, differently-shaped form for the same four fields.
 */
function RecordFormRow({
  editing,
  onTypeChange,
  onDone,
}: {
  /** `null` is the add case; a record puts the row into edit mode. */
  editing: LocalRecord | null;
  onTypeChange: (type: RecordType) => void;
  onDone: () => void;
}) {
  const addRecord = useAddRecord();
  const updateRecord = useUpdateRecord();
  const form = useForm<RecordFormValues>({
    resolver: zodResolver(recordFormSchema),
    defaultValues: recordFormDefaults(editing),
  });
  const isEdit = editing !== null;

  // Re-seed whenever the target changes — including back to null when an
  // edit is cancelled or saved, which is what clears the row.
  useEffect(() => {
    const defaults = recordFormDefaults(editing);
    form.reset(defaults);
    onTypeChange(defaults.type);
    if (editing) form.setFocus("name");
  }, [editing, form, onTypeChange]);

  const type = form.watch("type");
  const meta = VALUE_META[type];
  const busy = addRecord.isPending || updateRecord.isPending;

  function onSubmit(values: RecordFormValues) {
    const payload = {
      name: values.name.trim(),
      type: values.type,
      value: values.value.trim(),
      ttl: Number(values.ttl.trim()),
    };
    const onError = (err: unknown) =>
      toast.error(
        err instanceof ApiError ? err.message : `Couldn't ${isEdit ? "update" : "add"} the record`,
      );
    if (isEdit) {
      updateRecord.mutate(
        { id: editing.id, ...payload },
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
    addRecord.mutate(payload, {
      onSuccess: () => {
        toast.success("Record added");
        // Straight back to an empty row, focus on the name: adding one
        // record is very often adding four.
        form.reset(recordFormDefaults(null));
        onTypeChange("A");
        form.setFocus("name");
      },
      onError,
    });
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        // The left rule marks this band as the one editable row, and turns
        // primary while editing so it is obvious the row is now bound to an
        // existing record rather than creating a new one.
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
                    aria-label="Record name"
                    placeholder="nas.home.lan"
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
                  <NativeSelect
                    {...field}
                    aria-label="Record type"
                    onChange={(e) => {
                      const next = e.target.value as RecordType;
                      field.onChange(next);
                      onTypeChange(next);
                      // The value rule depends on the type beside it, so a
                      // shown error must re-evaluate against the new type
                      // rather than waiting for the next submit.
                      //
                      // getFieldState() rather than formState.errors:
                      // formState is a snapshot of the last render, and this
                      // component never reads .errors while rendering so it
                      // isn't subscribed to them — mid-event it can still say
                      // "no error" while the field is visibly showing one.
                      if (form.getFieldState("value").invalid) void form.trigger("value");
                    }}
                  >
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
            name="value"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label={meta.header}
                    placeholder={meta.placeholder}
                    autoComplete="off"
                    className="font-mono"
                  />
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
                  <Input {...field} aria-label="TTL in seconds" inputMode="numeric" />
                </FormControl>
              </FormItem>
            )}
          />
          <div className="flex items-center justify-end gap-1.5">
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
                onTypeChange("A");
                onDone();
              }}
            >
              <X />
            </Button>
          </div>
        </div>

        {/* Hints and messages live on a second line of the same grid, so
            each one sits under the field it belongs to. A message replaces
            its hint rather than stacking, which would reflow the row. */}
        <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
          <FieldHint name="name" form={form} hint="Prefix *. for a wildcard." />
          <span />
          <FieldHint name="value" form={form} hint={meta.hint} />
          <FieldHint name="ttl" form={form} hint="Seconds, 1–86400. Required." />
          <span />
        </div>
      </form>
    </Form>
  );
}

/** A field's help text, replaced by its validation message once it has one. */
function FieldHint({
  name,
  form,
  hint,
}: {
  name: "name" | "value" | "ttl";
  form: ReturnType<typeof useForm<RecordFormValues>>;
  hint: string;
}) {
  const message = form.formState.errors[name]?.message;
  return (
    <FormField
      control={form.control}
      name={name}
      render={() =>
        message ? (
          <FormMessage />
        ) : (
          <FormItem>
            <span className="text-muted-foreground">{hint}</span>
          </FormItem>
        )
      }
    />
  );
}

// --- page ------------------------------------------------------------------

/** "6 records · 3 A · 1 AAAA · 1 CNAME · 1 TXT" — what is actually in the
 * table, by type, rather than a bare total. */
function countsLine(records: LocalRecord[]): string {
  const byType = RECORD_TYPES.filter((t) => records.some((r) => r.type === t)).map(
    (t) => `${records.filter((r) => r.type === t).length} ${t}`,
  );
  return [`${records.length} ${records.length === 1 ? "record" : "records"}`, ...byType].join(
    " · ",
  );
}

/**
 * Local DNS — custom A/AAAA/CNAME/TXT records dnsaur answers directly
 * instead of forwarding upstream. GET/POST/PUT/DELETE /records via
 * use-records.ts's hooks.
 */
export function LocalDns() {
  const records = useRecords();
  const deleteRecord = useDeleteRecord();

  const [search, setSearch] = useState("");
  const [typeFilter, setTypeFilter] = useState<"" | RecordType>("");
  const [formType, setFormType] = useState<RecordType>("A");
  const [editing, setEditing] = useState<LocalRecord | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<LocalRecord | null>(null);

  const all = useMemo(() => records.data ?? [], [records.data]);
  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return all.filter(
      (r) =>
        (typeFilter === "" || r.type === typeFilter) &&
        (q === "" || r.name.toLowerCase().includes(q) || r.value.toLowerCase().includes(q)),
    );
  }, [all, search, typeFilter]);

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteRecord.mutate(target.id, {
      onSuccess: () => {
        toast.success("Record deleted");
        // An open edit of the row being deleted would otherwise keep
        // pointing at an id the server no longer has.
        if (editing?.id === target.id) setEditing(null);
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  let body: ReactNode;
  if (records.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (records.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load local DNS records</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (all.length === 0) {
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        No local records yet. Add one above and dnsaur will answer that name itself, instead of
        forwarding it upstream.
      </p>
    );
  } else if (shown.length === 0) {
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        No records match this filter.{" "}
        <Button
          type="button"
          variant="link"
          size="sm"
          onClick={() => {
            setSearch("");
            setTypeFilter("");
          }}
        >
          Clear it
        </Button>
      </p>
    );
  } else {
    body = shown.map((record) => (
      <div
        key={record.id}
        data-slot="record-row"
        className={cn(GRID, "border-b border-border-muted py-2")}
      >
        <RecordName name={record.name} />
        <span>
          <Badge variant={TYPE_VARIANT[record.type]}>{record.type}</Badge>
        </span>
        <span className="truncate font-mono text-sm" title={record.value}>
          {record.value}
        </span>
        <span className="font-mono text-sm">
          {record.ttl.toLocaleString()}
          <span className="text-xs text-muted-foreground">s</span>
        </span>
        <div className="flex items-center justify-end gap-1">
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Edit ${record.name}`}
            onClick={() => setEditing(record)}
          >
            <PencilLine />
          </Button>
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Delete ${record.name}`}
            onClick={() => setDeleteTarget(record)}
          >
            <Trash2 />
          </Button>
        </div>
      </div>
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-3 border-b border-border px-4 py-2.5">
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="search names…"
          aria-label="Search records"
          className="w-56"
        />
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
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
      </div>

      {records.isError && records.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="local DNS records"
            onRetry={() => void records.refetch()}
            isRetrying={records.isFetching}
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
        {/* Follows the form row's type, because it labels what that row is
            asking for as much as what the column holds. */}
        <span>{VALUE_META[formType].header}</span>
        <span>TTL</span>
        <span className="text-right">Actions</span>
      </div>

      <RecordFormRow editing={editing} onTypeChange={setFormType} onDone={() => setEditing(null)} />

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <div className="flex shrink-0 items-center gap-4 border-t border-border px-4 py-2 font-mono text-xs text-muted-foreground">
        <span>{countsLine(all)}</span>
        <span className="max-md:hidden">
          duplicates are allowed — every matching row is answered
        </span>
        <span className="ml-auto max-lg:hidden">CNAME chains follow up to 8 hops</span>
      </div>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this record?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <code className="font-mono break-all text-foreground">{deleteTarget.name}</code>{" "}
                  will stop resolving locally.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
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
