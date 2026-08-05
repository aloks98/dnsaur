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
function recordValueError(type: LocalRecord["type"], value: string): string | null {
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
  const form = useForm<RecordFormValues>({
    resolver: zodResolver(recordFormSchema),
    defaultValues: recordFormDefaults(record),
  });
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
                        // The value field's rule depends on the current type
                        // (the resolver re-runs recordFormSchema against the
                        // live form values, so it sees the type just set
                        // here rather than a stale closure), so a shown error
                        // should re-evaluate immediately against the newly
                        // selected type rather than waiting for the next
                        // submit attempt.
                        //
                        // getFieldState() rather than formState.errors:
                        // `form.formState` is a snapshot of the last render
                        // this component actually did, and it doesn't
                        // re-render on every error change (it never reads
                        // .errors while rendering, so it isn't subscribed to
                        // them) — so mid-event it can still say "no error"
                        // while the field is visibly showing one, and the
                        // re-check would silently never happen.
                        // getFieldState reads the form's live state instead.
                        if (form.getFieldState("value").invalid) void form.trigger("value");
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
          <StaleDataAlert
            what="local DNS records"
            onRetry={() => void records.refetch()}
            isRetrying={records.isFetching}
          />
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
