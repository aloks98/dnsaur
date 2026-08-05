import { useEffect, useState, type ReactNode } from "react";
import {
  ArrowRightLeft,
  FileText,
  Globe,
  PencilLine,
  Plus,
  Route,
  Trash2,
  TriangleAlert,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
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
  EmptyState,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Separator,
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import type { LocalRecord } from "../api/types";
import { useAddRecord, useDeleteRecord, useRecords, useUpdateRecord } from "../hooks/use-records";

// --- validation --------------------------------------------------------
// Mirrors internal/api/records_handlers.go's normalizeRecord closely enough
// that a record accepted here is one the API/DNS layer accepts — the
// server is the one source of truth for what's a valid record; this is
// only a client-side echo of it so typos surface before a round trip
// instead of after. Per Task 10's lesson (an over-strict IPv6 check
// wrongly blocked a valid zone-id matcher), the bias throughout is toward
// *not* rejecting something the server would accept.

const RECORD_TYPES: LocalRecord["type"][] = ["A", "AAAA", "CNAME", "TXT"];

/** The server lowercases the name and trims a trailing dot itself before
 * validating (and re-derives the wildcard/base-domain split from that
 * normalized form) — so this check must accept mixed case and a trailing
 * dot rather than rejecting them, matching normalizeRecord exactly:
 * TrimSuffix(TrimSpace(name), "."), then strip a leading "*." wildcard,
 * then require the remainder to be non-empty, contain a dot, and be free
 * of spaces/slashes/backslashes. */
function validateRecordName(value: string): string | true {
  const withoutTrailingDot = value.trim().replace(/\.$/, "");
  const name = withoutTrailingDot.replace(/^\*\./, "");
  if (!name || !name.includes(".") || /[ /\\]/.test(name)) {
    return "Enter a domain, e.g. nas.home.lan (a *.parent wildcard is allowed)";
  }
  return true;
}

function isValidIPv4(value: string): boolean {
  const match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(value);
  if (!match) return false;
  return match.slice(1).every((octet) => {
    if (octet.length > 1 && octet.startsWith("0")) return false;
    const n = Number(octet);
    return n >= 0 && n <= 255;
  });
}

/** The server parses AAAA values with Go's net.ParseIP (not netip.ParseAddr
 * — unlike the client-matcher validator in groups-clients.tsx), which has
 * no concept of an RFC 4007 zone id, so a "%eth0" suffix isn't accepted
 * here either.
 *
 * A literal "." is NOT rejected: RFC 4291 §2.2 defines a dotted-quad tail
 * form, and real, useful addresses use it — e.g. the NAT64 well-known
 * prefix "64:ff9b::192.0.2.1" or "2001:db8::192.168.1.1" both parse fine
 * on the server (net.ParseIP succeeds, To4() == nil, so the AAAA check
 * accepts them). An earlier version of this validator excluded any "."
 * outright on the theory that "real IPv6 literals never contain a dot" —
 * that premise was wrong and blocked exactly these valid addresses from
 * ever reaching the server (caught in review). The one case a dot *should*
 * disqualify — an IPv4-mapped address like "::ffff:192.168.1.1", where
 * Go's To4() returns non-nil and the server 400s it as AAAA — isn't worth
 * special-casing here: reliably distinguishing "mapped" from "embedded"
 * dotted-quad forms needs a real IPv6 parser (bit-level, not textual), and
 * getting that narrowing wrong in either direction repeats the same
 * mistake. Left to the server: a false accept here just surfaces as a 400
 * toast, which is strictly better than a false reject that silently blocks
 * a legitimate record. */
function isValidIPv6(value: string): boolean {
  if (!value.includes(":")) return false;
  try {
    // The URL host parser validates bracketed IPv6 syntax for us — a
    // pragmatic stand-in for a real IPv6 parser that's good enough to
    // catch typos before the round trip.
    new URL(`http://[${value}]`);
    return true;
  } catch {
    return false;
  }
}

function validateRecordValue(type: LocalRecord["type"], value: string): string | true {
  const trimmed = value.trim();
  switch (type) {
    case "A":
      return isValidIPv4(trimmed) ? true : "Enter a valid IPv4 address, e.g. 192.168.1.10";
    case "AAAA":
      return isValidIPv6(trimmed) ? true : "Enter a valid IPv6 address, e.g. 2001:db8::1";
    case "CNAME": {
      const target = trimmed.replace(/\.$/, "");
      return target.includes(".")
        ? true
        : "Enter the domain this name points to, e.g. target.example.com";
    }
    case "TXT":
      return trimmed ? true : "Enter the text value to return";
    default:
      return true;
  }
}

function validateTtl(value: string): string | true {
  const trimmed = value.trim();
  if (!/^\d+$/.test(trimmed)) return "TTL must be a whole number of seconds";
  const n = Number(trimmed);
  return n >= 1 && n <= 86400 ? true : "TTL must be between 1 and 86400 seconds";
}

// --- type badge ----------------------------------------------------------
// One neutral variant for all four types — this is a category tag, not a
// verdict (contrast the block/allow/decision badges elsewhere in the app,
// which spend color on a policy axis), so the icon shape alone carries the
// distinction, the same "second channel beyond color" idea as
// pages/queries.tsx's DECISION_BADGE.
const RECORD_TYPE_ICON: Record<LocalRecord["type"], LucideIcon> = {
  A: Globe,
  AAAA: Waypoints,
  CNAME: ArrowRightLeft,
  TXT: FileText,
};

function RecordTypeBadge({ type }: { type: LocalRecord["type"] }) {
  const Icon = RECORD_TYPE_ICON[type];
  return (
    <Badge variant="secondary">
      <Icon />
      {type}
    </Badge>
  );
}

// --- add/edit sheet --------------------------------------------------------

interface RecordFormValues {
  name: string;
  type: LocalRecord["type"];
  value: string;
  ttl: string;
}

const DEFAULT_TTL = 300;

function recordFormDefaults(record: LocalRecord | null): RecordFormValues {
  return {
    name: record?.name ?? "",
    type: record?.type ?? "A",
    value: record?.value ?? "",
    ttl: String(record?.ttl ?? DEFAULT_TTL),
  };
}

// What the Value field means — and how to describe it — depends entirely
// on the selected type, so its label/placeholder/help text switch live as
// the Select changes, turning one generic "Value" input into something
// that documents itself for whichever record kind is currently selected.
const VALUE_FIELD_META: Record<
  LocalRecord["type"],
  { label: string; placeholder: string; description: string }
> = {
  A: {
    label: "IPv4 address",
    placeholder: "192.168.1.10",
    description: "The IPv4 address this name resolves to.",
  },
  AAAA: {
    label: "IPv6 address",
    placeholder: "2001:db8::1",
    description: "The IPv6 address this name resolves to.",
  },
  CNAME: {
    label: "Target domain",
    placeholder: "target.example.com",
    description: "Another domain name this name points to, instead of an address.",
  },
  TXT: {
    label: "Text value",
    placeholder: "Any text",
    description: "Free-form text returned for this name.",
  },
};

/**
 * Add/edit — a side Sheet rather than the Dialog Filtering's Lists/Clients
 * tabs use (Task 9), per the brief: this is the first CRUD page to use the
 * pattern, meant to be copied by later ones. Layout is a fixed
 * header/footer with the field stack as the only scrolling region in
 * between, so the sheet never feels cramped regardless of viewport height
 * — the same shape as pages/queries.tsx's "why?" Drawer, just editable.
 */
function RecordFormSheet({
  record,
  open,
  onOpenChange,
}: {
  /** `null` renders the "Add record" copy; a LocalRecord renders "Edit record". */
  record: LocalRecord | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const addRecord = useAddRecord();
  const updateRecord = useUpdateRecord();
  const isEdit = record !== null;
  const form = useForm<RecordFormValues>({ defaultValues: recordFormDefaults(record) });
  const type = form.watch("type");

  useEffect(() => {
    if (open) form.reset(recordFormDefaults(record));
  }, [open, record, form]);

  function onSubmit(values: RecordFormValues) {
    const payload = {
      name: values.name.trim(),
      type: values.type,
      value: values.value.trim(),
      ttl: Number(values.ttl.trim()),
    };
    const onSuccess = () => {
      toast.success(isEdit ? "Record updated" : "Record added");
      onOpenChange(false);
    };
    const onError = (err: unknown) =>
      toast.error(
        err instanceof ApiError ? err.message : `Couldn't ${isEdit ? "update" : "add"} the record`,
      );
    if (isEdit && record) {
      updateRecord.mutate({ id: record.id, ...payload }, { onSuccess, onError });
    } else {
      addRecord.mutate(payload, { onSuccess, onError });
    }
  }

  const busy = addRecord.isPending || updateRecord.isPending;
  const valueMeta = VALUE_FIELD_META[type];

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent>
        <Form {...form}>
          <form
            className="flex h-full flex-col overflow-hidden"
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            <SheetHeader>
              <SheetTitle>{isEdit ? "Edit record" : "Add record"}</SheetTitle>
              <SheetDescription>
                dnsaur answers this name directly, without forwarding it upstream.
              </SheetDescription>
            </SheetHeader>

            <div className="flex flex-1 flex-col gap-5 overflow-y-auto px-4">
              <FormField
                control={form.control}
                name="name"
                rules={{ validate: validateRecordName }}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Name</FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        placeholder="nas.home.lan"
                        autoComplete="off"
                        className="font-mono"
                      />
                    </FormControl>
                    <FormDescription>
                      Prefix with *. to match any subdomain, e.g. *.iot.home.lan.
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name="type"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Type</FormLabel>
                    <Select
                      // `items` maps each value to its display label —
                      // without it, SelectValue renders the raw stored value
                      // instead of the label (Task 12's finding; see
                      // settings.tsx/account.tsx for the same fix). Harmless
                      // today since RECORD_TYPES' values and labels are
                      // identical strings, but silently regresses the moment
                      // a friendlier label is introduced.
                      items={Object.fromEntries(RECORD_TYPES.map((t) => [t, t]))}
                      value={field.value}
                      onValueChange={(next) => {
                        field.onChange(next);
                        // The value field's rules depend on the current type
                        // (react-hook-form passes the live form values into
                        // `validate`, not a stale closure — see
                        // validateRecordValue's call site below), so a
                        // shown error should re-evaluate immediately against
                        // the newly selected type rather than waiting for
                        // the next submit attempt.
                        if (form.formState.errors.value) void form.trigger("value");
                      }}
                    >
                      <SelectTrigger aria-label="Record type">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {RECORD_TYPES.map((t) => (
                          <SelectItem key={t} value={t}>
                            {t}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name="value"
                rules={{
                  validate: (value: string, formValues) =>
                    validateRecordValue(formValues.type, value),
                }}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{valueMeta.label}</FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        placeholder={valueMeta.placeholder}
                        autoComplete="off"
                        className="font-mono"
                      />
                    </FormControl>
                    <FormDescription>{valueMeta.description}</FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name="ttl"
                rules={{ validate: validateTtl }}
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>TTL (seconds)</FormLabel>
                    <FormControl>
                      <Input {...field} type="number" min={1} max={86400} className="w-32" />
                    </FormControl>
                    <FormDescription>
                      How long resolvers may cache this answer, from 1 second up to a day.
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>

            <SheetFooter>
              <SheetClose render={<Button type="button" variant="outline" />}>Cancel</SheetClose>
              <Button type="submit" disabled={busy}>
                {busy ? "Saving…" : isEdit ? "Save" : "Add record"}
              </Button>
            </SheetFooter>
          </form>
        </Form>
      </SheetContent>
    </Sheet>
  );
}

// --- background-refetch failure -------------------------------------------

/**
 * A *background* refetch failed while data from an earlier successful fetch
 * is still in hand. Every mutation here invalidates the records query, which
 * refetches immediately; query-core flips `status` to "error" if that
 * refetch fails, even though `data` is intact — so gating the destructive
 * "couldn't load" Alert on `isError` alone would swap a populated, still
 * correct table for an error card right after a successful save
 * (refetchOnReconnect, on by default, is a second trigger). The destructive
 * Alert is reserved for `isError && data === undefined` — genuinely nothing
 * to show — and this quiet banner covers the rest, above the table.
 */
function StaleDataAlert({ onRetry, isRetrying }: { onRetry: () => void; isRetrying: boolean }) {
  return (
    <Alert variant="warning">
      <TriangleAlert />
      <AlertTitle>Couldn&apos;t refresh local DNS records</AlertTitle>
      <AlertDescription>
        <p>Showing what last loaded successfully.</p>
        <Button type="button" variant="outline" size="sm" onClick={onRetry} disabled={isRetrying}>
          {isRetrying ? "Retrying…" : "Try again"}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

// --- table -----------------------------------------------------------------

function RecordsTable({
  records,
  onEdit,
  onDeleteRequest,
}: {
  records: LocalRecord[];
  onEdit: (record: LocalRecord) => void;
  onDeleteRequest: (record: LocalRecord) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Type</TableHead>
          <TableHead>Value</TableHead>
          <TableHead className="text-right">TTL</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {records.map((record) => (
          <TableRow key={record.id}>
            <TableCell className="max-w-56 truncate font-mono text-xs" title={record.name}>
              {record.name}
            </TableCell>
            <TableCell>
              <RecordTypeBadge type={record.type} />
            </TableCell>
            <TableCell className="max-w-72 truncate font-mono text-xs" title={record.value}>
              {record.value}
            </TableCell>
            <TableCell className="text-right tabular-nums text-muted-foreground">
              {record.ttl}
            </TableCell>
            <TableCell className="text-right">
              <div className="flex justify-end gap-1">
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Edit ${record.name}`}
                  onClick={() => onEdit(record)}
                >
                  <PencilLine />
                </Button>
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Delete ${record.name}`}
                  onClick={() => onDeleteRequest(record)}
                >
                  <Trash2 />
                </Button>
              </div>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// --- page ------------------------------------------------------------------

/**
 * Local DNS — custom A/AAAA/CNAME/TXT records dnsaur answers directly
 * instead of forwarding upstream (Task 11). GET/POST/PUT/DELETE /records
 * via use-records.ts's hooks. Establishes the table + side-sheet CRUD
 * pattern (see RecordFormSheet) later CRUD pages are expected to reuse.
 */
export function LocalDns() {
  const records = useRecords();
  const deleteRecord = useDeleteRecord();

  const [addOpen, setAddOpen] = useState(false);
  // Open state is deliberately separate from the target rather than derived
  // from `editTarget !== null`: the sheet stays mounted through base-ui's
  // exit transition, so nulling the target on close would re-render the
  // still-visible panel as the *add* variant — retitling "Edit record" to
  // "Add record" and "Save" to "Add record" on the way out, including
  // straight after a successful save. The target is replaced on the next
  // open instead of cleared on close.
  const [editTarget, setEditTarget] = useState<LocalRecord | null>(null);
  const [editOpen, setEditOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<LocalRecord | null>(null);

  function onEditRequest(record: LocalRecord) {
    setEditTarget(record);
    setEditOpen(true);
  }

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteRecord.mutate(target.id, {
      onSuccess: () => {
        toast.success("Record deleted");
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  const isEmpty = records.data?.length === 0;

  let body: ReactNode;
  if (records.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (records.data === undefined) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load local DNS records</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (isEmpty) {
    body = (
      <EmptyState
        icon={<Route />}
        title="No local DNS records yet"
        description="Add an A, AAAA, CNAME, or TXT record for dnsaur to answer directly."
        action={
          <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
            <Plus />
            Add record
          </Button>
        }
      />
    );
  } else {
    body = (
      <RecordsTable
        records={records.data}
        onEdit={onEditRequest}
        onDeleteRequest={setDeleteTarget}
      />
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-3">
        <div>
          <h1 className="text-2xl font-heading font-semibold text-foreground">Local DNS</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Custom A/AAAA/CNAME/TXT records dnsaur answers directly, without forwarding upstream.
          </p>
        </div>
      </div>

      <Separator />

      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-muted-foreground">
            {records.data && records.data.length > 0
              ? `${records.data.length} ${records.data.length === 1 ? "record" : "records"}`
              : "Checked first, ahead of filter rules and upstream forwarding."}
          </p>
          {!isEmpty && (
            <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
              <Plus />
              Add record
            </Button>
          )}
        </div>

        {records.isError && records.data !== undefined && (
          <StaleDataAlert onRetry={() => void records.refetch()} isRetrying={records.isFetching} />
        )}

        {body}
      </div>

      <RecordFormSheet record={null} open={addOpen} onOpenChange={setAddOpen} />
      <RecordFormSheet record={editTarget} open={editOpen} onOpenChange={setEditOpen} />

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
