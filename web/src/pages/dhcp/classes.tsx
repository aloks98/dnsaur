import { Fragment, useState, type ReactNode } from "react";
import { Plus, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import {
  useFieldArray,
  useForm,
  useWatch,
  type FieldErrors,
  type UseFormReturn,
} from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  cn,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  Form,
  FormField,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { DHCPClass, DHCPScope } from "../../api/types";
import {
  useClasses,
  useCreateClass,
  useDeleteClass,
  useScopes,
  useUpdateClass,
  type ClassInput,
} from "../../hooks/use-dhcp";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { ConfirmDeleteDialog } from "../dialogs";
import { requiredText } from "../../lib/schemas";
import {
  ClientOptionSections,
  clientOptionsShape,
  clientOptionsToForm,
  clientOptionsToInput,
  Field,
  MiniTable,
  useRowErrorIds,
  OptionsSection,
  type ClientOptionFields,
} from "./scopes";

/** The board's five columns, shared by the head and every row. */
const GRID = "grid grid-cols-[1fr_300px_1fr_96px_104px] items-center gap-3.5 px-4";

// --- pure helpers ------------------------------------------------------------

type MatcherKind = "vendor" | "mac";

interface MatcherRow {
  kind: MatcherKind;
  value: string;
}

const PLACEHOLDER: Record<MatcherKind, string> = {
  vendor: "PXEClient:Arch:00007",
  mac: "a4:cf:12",
};

/** 1–6 whole octets, colon-separated: what the server takes as a prefix. */
const MAC_PREFIX = /^[0-9a-f]{2}(:[0-9a-f]{2}){0,5}$/i;

/**
 * What is wrong with each matcher row as typed, or undefined. A blank row is
 * not yet wrong, and a vendor prefix is the server's to refuse (its text
 * lands on the row): only the MAC grammar is checked here.
 */
export function matcherRowErrors(rows: readonly MatcherRow[]): (string | undefined)[] {
  return rows.map((row) => {
    const value = row.value.trim();
    if (value === "" || row.kind !== "mac" || MAC_PREFIX.test(value)) return undefined;
    return `Not a MAC prefix: ${value}`;
  });
}

/** A stored `kind:value`, split on the first colon — a mac value has more. */
function toRow(matcher: string): MatcherRow {
  const at = matcher.indexOf(":");
  const kind = matcher.slice(0, at);
  return { kind: kind === "mac" ? "mac" : "vendor", value: matcher.slice(at + 1) };
}

function toMatcher(row: MatcherRow): string {
  const value = row.value.trim();
  return `${row.kind}:${row.kind === "mac" ? value.toLowerCase() : value}`;
}

/**
 * The OPTIONS cell: the set fields in a fixed order, or "" when the class
 * sets nothing and inherits everything from the scope.
 */
export function classSummary(c: DHCPClass): string {
  const dns = c.dns_servers
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
  const parts: string[] = [];
  if (dns.length > 0) parts.push(`DNS ${dns[0]}${dns.length > 1 ? ` +${dns.length - 1}` : ""}`);
  if (c.domain !== "") parts.push(`suffix ${c.domain}`);
  if (c.domain_search !== "") parts.push("search");
  if (c.ntp_servers !== "") parts.push("NTP");
  if (c.static_routes.length > 0) parts.push(`routes ${c.static_routes.length}`);
  if (c.next_server !== "" || c.server_hostname !== "" || c.boot_file !== "") parts.push("PXE");
  if (c.options.length > 0) parts.push(`opt ${c.options.length}`);
  return parts.join(" · ");
}

interface Usage {
  pools: number;
  scopes: string[];
}

/** How many pools name each class, and in which scopes, from the scope list. */
function classUsage(scopes: DHCPScope[] | undefined): Map<number, Usage> {
  const usage = new Map<number, Usage>();
  for (const scope of scopes ?? []) {
    for (const pool of scope.pools) {
      if (pool.class_id === 0) continue;
      const u = usage.get(pool.class_id) ?? { pools: 0, scopes: [] };
      u.pools++;
      if (!u.scopes.includes(scope.name)) u.scopes.push(scope.name);
      usage.set(pool.class_id, u);
    }
  }
  return usage;
}

function poolsLabel(n: number): string {
  return n === 1 ? "1 pool" : `${n} pools`;
}

// --- the class form ----------------------------------------------------------

interface ClassFormValues extends ClientOptionFields {
  name: string;
  matchers: MatcherRow[];
  dns_servers: string;
  domain: string;
}

const TAB_MATCHERS = "matchers";
const TAB_OPTIONS = "options";

const classFormSchema = z
  .object({
    name: requiredText("Name is required"),
    matchers: z.array(z.object({ kind: z.enum(["vendor", "mac"]), value: z.string() })),
    dns_servers: z.string(),
    domain: z.string(),
    ...clientOptionsShape,
  })
  .superRefine((v, ctx) => {
    if (v.matchers.every((m) => m.value.trim() === "")) {
      ctx.addIssue({ code: "custom", path: ["matchers"], message: "Add a matcher" });
    }
    matcherRowErrors(v.matchers).forEach((message, index) => {
      if (message !== undefined) {
        ctx.addIssue({ code: "custom", path: ["matchers", index, "value"], message });
      }
    });
  });

function toFormValues(c: DHCPClass | null): ClassFormValues {
  return {
    name: c?.name ?? "",
    matchers: c ? c.matchers.map(toRow) : [{ kind: "vendor", value: "" }],
    dns_servers: c?.dns_servers ?? "",
    domain: c?.domain ?? "",
    ...clientOptionsToForm(c),
  };
}

/** Blank matcher rows are dropped, like the other tables' blank rows. */
function sentRows(values: ClassFormValues): number[] {
  return values.matchers.flatMap((m, i) => (m.value.trim() === "" ? [] : [i]));
}

function toInput(values: ClassFormValues): ClassInput {
  return {
    name: values.name.trim(),
    matchers: sentRows(values).map((i) => toMatcher(values.matchers[i])),
    dns_servers: values.dns_servers.trim(),
    domain: values.domain.trim(),
    ...clientOptionsToInput(values),
  };
}

function MatchersTable({
  form,
  live,
}: {
  form: UseFormReturn<ClassFormValues>;
  live: (string | undefined)[];
}) {
  const matchers = useFieldArray({ control: form.control, name: "matchers" });
  const kinds = useWatch({ control: form.control, name: "matchers" });
  const errors = form.formState.errors.matchers;
  const tableError = errors?.message ?? errors?.root?.message;
  const rowError = (index: number) => live[index] ?? errors?.[index]?.value?.message;
  const { errorId, describedBy } = useRowErrorIds(rowError);

  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <span className="text-[12.5px] leading-none font-medium">Matchers</span>
      <MiniTable
        columns={["Kind", "Value"]}
        template="grid-cols-[120px_1fr_64px]"
        noun="matcher"
        rowError={rowError}
        errorId={errorId}
        rows={matchers.fields.map((row, index) => (
          <Fragment key={row.id}>
            <FormField
              control={form.control}
              name={`matchers.${index}.kind`}
              render={({ field }) => (
                <NativeSelect
                  {...field}
                  size="sm"
                  aria-label={`Matcher ${index + 1} kind`}
                  aria-describedby={describedBy(index)}
                >
                  <NativeSelectOption value="vendor">vendor</NativeSelectOption>
                  <NativeSelectOption value="mac">mac</NativeSelectOption>
                </NativeSelect>
              )}
            />
            <FormField
              control={form.control}
              name={`matchers.${index}.value`}
              render={({ field, fieldState }) => (
                <Input
                  {...field}
                  placeholder={PLACEHOLDER[kinds[index]?.kind ?? "vendor"]}
                  autoComplete="off"
                  aria-label={`Matcher ${index + 1} value`}
                  aria-invalid={live[index] !== undefined || fieldState.error !== undefined}
                  aria-describedby={describedBy(index)}
                  className="font-mono"
                />
              )}
            />
          </Fragment>
        ))}
        onRemove={(index) => matchers.remove(index)}
        onAdd={() => matchers.append({ kind: "vendor", value: "" })}
        addLabel="Add matcher"
      />
      {tableError !== undefined && (
        <p role="alert" className="font-mono text-[11px] text-destructive-foreground">
          {tableError}
        </p>
      )}
      <p className="text-[11px] text-muted-foreground">A client matches when any row matches.</p>
    </div>
  );
}

/** Create and edit in one dialog, the scope dialog's shell. `target` null is
 * the create case. */
function ClassDialog({ target, onClose }: { target: DHCPClass | null; onClose: () => void }) {
  const create = useCreateClass();
  const update = useUpdateClass();
  const form = useForm<ClassFormValues>({
    resolver: zodResolver(classFormSchema),
    defaultValues: toFormValues(target),
  });
  const isPending = create.isPending || update.isPending;
  const live = matcherRowErrors(useWatch({ control: form.control, name: "matchers" }));
  const [tab, setTab] = useState<string>(TAB_MATCHERS);

  function onSubmit(values: ClassFormValues) {
    const sent = sentRows(values);
    const onError = (err: unknown) => {
      // `matchers[i]: …` belongs on the row it came from, in the server's words.
      const match = err instanceof ApiError ? /^matchers\[(\d+)\]/.exec(err.message) : null;
      const row = match ? sent[Number(match[1])] : undefined;
      if (err instanceof ApiError && row !== undefined) {
        form.setError(`matchers.${row}.value`, { type: "server", message: err.message });
        setTab(TAB_MATCHERS);
        return;
      }
      toast.error(err instanceof ApiError ? err.message : "Couldn't save the class");
    };
    const body = toInput(values);
    if (target) {
      update.mutate({ id: target.id, ...body }, { onSuccess: onClose, onError });
    } else {
      create.mutate(body, { onSuccess: onClose, onError });
    }
  }

  function onInvalid(errors: FieldErrors<ClassFormValues>) {
    const fields = Object.keys(errors);
    if (fields.length === 0) return;
    setTab(fields.some((f) => f === "name" || f === "matchers") ? TAB_MATCHERS : TAB_OPTIONS);
  }

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="sm:max-w-[720px]">
        <DialogHeader>
          <DialogTitle>{target ? `Edit class · ${target.name}` : "New class"}</DialogTitle>
          <DialogDescription className="sr-only">
            Who the class matches on Matchers, what they are handed on Client options.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form
            onSubmit={(e) => void form.handleSubmit(onSubmit, onInvalid)(e)}
            noValidate
            className="flex flex-col gap-3.5"
          >
            <Tabs value={tab} onValueChange={setTab}>
              <TabsList>
                <TabsTrigger value={TAB_MATCHERS}>Matchers</TabsTrigger>
                <TabsTrigger value={TAB_OPTIONS}>Client options</TabsTrigger>
              </TabsList>
              <TabsContent value={TAB_MATCHERS} className="flex flex-col gap-3.5 pt-3.5">
                <div className="max-w-[320px]">
                  <FormField
                    control={form.control}
                    name="name"
                    render={({ field }) => (
                      <Field label="Name">
                        <Input {...field} placeholder="iot" autoComplete="off" />
                      </Field>
                    )}
                  />
                </div>
                <MatchersTable form={form} live={live} />
              </TabsContent>
              <TabsContent value={TAB_OPTIONS} className="flex flex-col gap-5 pt-3.5">
                <OptionsSection head="Names &amp; DNS">
                  <div className="grid gap-3.5 gap-x-6 sm:grid-cols-2">
                    <FormField
                      control={form.control}
                      name="dns_servers"
                      render={({ field }) => (
                        <Field label="DNS servers" hint="addresses">
                          <Input
                            {...field}
                            placeholder="inherit"
                            autoComplete="off"
                            className="font-mono"
                          />
                        </Field>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="domain"
                      render={({ field }) => (
                        <Field label="DNS suffix">
                          <Input
                            {...field}
                            placeholder="inherit"
                            autoComplete="off"
                            className="font-mono"
                          />
                        </Field>
                      )}
                    />
                  </div>
                </OptionsSection>
                <ClientOptionSections inherit />
              </TabsContent>
            </Tabs>

            <DialogFooter>
              <DialogClose render={<Button type="button" size="sm" variant="ghost" />}>
                Cancel
              </DialogClose>
              <Button
                type="submit"
                size="sm"
                disabled={isPending || live.some((e) => e !== undefined)}
              >
                {isPending ? "Saving…" : "Save"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

// --- the table -----------------------------------------------------------------

function ClassRow({
  c,
  pools,
  managed,
  onEdit,
  onDelete,
}: {
  c: DHCPClass;
  /** undefined while the scopes are not loaded: no count is shown then, so
   * a failed load never reads as "0 pools". */
  pools: number | undefined;
  managed: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const summary = classSummary(c);
  return (
    <div data-testid="class-row" className={cn(GRID, "border-b border-border-muted py-2")}>
      <span className="truncate font-mono text-[12.5px] font-medium whitespace-nowrap">
        {c.name}
      </span>
      <span className="flex min-w-0 font-mono text-xs" title={c.matchers.join(", ")}>
        <span className="min-w-0 truncate">{c.matchers.slice(0, 2).join(", ")}</span>
        {c.matchers.length > 2 && (
          <span className="ml-2 shrink-0 text-[10.5px] text-muted-foreground">
            +{c.matchers.length - 2}
          </span>
        )}
      </span>
      <span className={cn("truncate font-mono text-xs", summary === "" && "text-muted-foreground")}>
        {summary === "" ? "inherits scope" : summary}
      </span>
      <span className={cn("font-mono text-xs", !pools && "text-muted-foreground")}>
        {pools === undefined ? "—" : poolsLabel(pools)}
      </span>
      <span className="flex items-center justify-end gap-0.5">
        <Button type="button" size="sm" variant="ghost" disabled={managed} onClick={onEdit}>
          Edit
        </Button>
        <Button type="button" size="sm" variant="ghost" disabled={managed} onClick={onDelete}>
          Delete
        </Button>
      </span>
    </div>
  );
}

/** DHCP › Classes. One row per class, the dialog that writes one, and the
 * delete that refuses while a pool names it. */
export function DHCPClasses() {
  const classes = useClasses();
  const scopes = useScopes().data;
  const usage = classUsage(scopes);
  const managed = useManagedBy() !== "";
  const deleteClass = useDeleteClass();

  const [dialogFor, setDialogFor] = useState<DHCPClass | null | undefined>(undefined);
  const [deleteTarget, setDeleteTarget] = useState<DHCPClass | null>(null);
  // The server's 409, when the scope list here was stale.
  const [refusal, setRefusal] = useState<string | null>(null);

  const rows = classes.data ?? [];
  const inUse = deleteTarget ? usage.get(deleteTarget.id) : undefined;
  const blocked = inUse !== undefined || refusal !== null;

  const newButton = (
    <Button
      type="button"
      size="sm"
      variant="outline"
      disabled={managed}
      onClick={() => setDialogFor(null)}
    >
      <Plus />
      New class
    </Button>
  );

  let body: ReactNode;
  if (classes.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (classes.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load classes</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (rows.length === 0) {
    body = (
      <div className="flex flex-col items-start gap-2 py-12 pr-4 pl-8">
        <p className="font-heading text-sm font-semibold">No classes yet</p>
        <p className="text-xs text-muted-foreground">
          A class matches clients by vendor or MAC prefix and gives them their own options.
        </p>
        {newButton}
      </div>
    );
  } else {
    body = rows.map((c) => (
      <ClassRow
        key={c.id}
        c={c}
        pools={scopes === undefined ? undefined : (usage.get(c.id)?.pools ?? 0)}
        managed={managed}
        onEdit={() => setDialogFor(c)}
        onDelete={() => setDeleteTarget(c)}
      />
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-x-auto">
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2.5">
        <span className="font-heading text-[13.5px] leading-none font-semibold tracking-tight">
          Classes
        </span>
        <div className="ml-auto flex items-center gap-3">
          {managed && <ManagedNotice />}
          {newButton}
        </div>
      </div>

      {classes.isError && classes.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="classes"
            onRetry={() => void classes.refetch()}
            isRetrying={classes.isFetching}
          />
        </div>
      )}

      <div
        className={cn(
          GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-[9.5px] leading-none font-medium tracking-[0.14em] text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Matchers</span>
        <span>Options</span>
        <span>Pools</span>
        <span className="text-right">Actions</span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      {dialogFor !== undefined && (
        <ClassDialog target={dialogFor} onClose={() => setDialogFor(undefined)} />
      )}

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (open) return;
          setDeleteTarget(null);
          setRefusal(null);
        }}
        title={
          blocked && deleteTarget ? `Delete class · ${deleteTarget.name}` : "Delete this class?"
        }
        description={
          inUse
            ? `In use by ${poolsLabel(inUse.pools)} in ${inUse.scopes.join(" and ")}. Remove it from those pools first.`
            : (refusal ??
              (deleteTarget && (
                <>
                  <span className="font-medium text-foreground">{deleteTarget.name}</span> stops
                  matching clients.
                </>
              )))
        }
        isPending={deleteClass.isPending}
        disabled={blocked}
        onConfirm={() =>
          deleteTarget &&
          deleteClass.mutate(deleteTarget.id, {
            onSuccess: () => setDeleteTarget(null),
            onError: (err) => {
              if (err instanceof ApiError && err.status === 409) setRefusal(err.message);
              else toast.error(err instanceof ApiError ? err.message : "Couldn't delete the class");
            },
          })
        }
      />
    </div>
  );
}
