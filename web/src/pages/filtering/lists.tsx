import { useState, type ReactNode } from "react";
import {
  CircleCheck,
  CircleX,
  Clock,
  FileWarning,
  ListChecks,
  Pencil,
  Plus,
  RefreshCw,
  ShieldBan,
  ShieldCheck,
  Trash2,
  TriangleAlert,
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
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
  Skeleton,
  Switch,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { List, ListStatus } from "../../api/types";
import {
  useAddList,
  useDeleteList,
  useLists,
  useRefreshFilters,
  useRenameList,
  useToggleList,
} from "../../hooks/use-filters";
import { relativeTime } from "../../lib/format";
import { deriveListName } from "../../lib/list-name";
import { StaleDataAlert } from "../../components/stale-data-alert";

// Block/allow badge treatment — same red/green thread as the query log's
// decision badges (see pages/queries.tsx's DECISION_BADGE), so a list's
// kind reads consistently with the rest of the app wherever it shows up.
const KIND_META: Record<
  List["kind"],
  { label: string; variant: NonNullable<BadgeProps["variant"]>; icon: typeof ShieldBan }
> = {
  block: { label: "Block", variant: "destructive-light", icon: ShieldBan },
  allow: { label: "Allow", variant: "success-light", icon: ShieldCheck },
};

// `items` maps each value to its display label — without it, SelectValue
// renders the raw stored value ("block") instead of the option's label
// (Task 12's finding; see settings.tsx/account.tsx for the same fix).
const LIST_KIND_ITEMS: Record<List["kind"], string> = {
  block: KIND_META.block.label,
  allow: KIND_META.allow.label,
};

/** Mirrors the server's own check (net/url.Parse + scheme/host, see
 * internal/api/filters_handlers.go's handleListCreate) so a bad URL never
 * even reaches the network — the 400 the server would return is instead
 * caught inline, before submit. One refinement rather than a chain of
 * them: each step depends on the previous one having parsed, and each has
 * its own specific message to hand back. */
const listUrlSchema = z
  .string()
  .trim()
  .superRefine((value, ctx) => {
    const fail = (message: string) => ctx.addIssue({ code: "custom", message });
    if (!value) return fail("URL is required");
    let parsed: URL;
    try {
      parsed = new URL(value);
    } catch {
      return fail("Enter a valid URL, e.g. https://example.com/hosts");
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
      return fail("URL must start with http:// or https://");
    }
    if (!parsed.host) return fail("URL must include a host");
  });

const addListSchema = z.object({
  url: listUrlSchema,
  kind: z.enum(["block", "allow"]),
  // Optional: blank means "use the server's URL-derived default", which the
  // field shows live as its placeholder. Bounded to match the API's own cap
  // (internal/api/filters_handlers.go's maxListNameLen).
  name: z.string().trim().max(120, "Name is too long (max 120)"),
});

type AddListValues = z.infer<typeof addListSchema>;

const ADD_LIST_DEFAULTS: AddListValues = { url: "", kind: "block", name: "" };

/** Rename-only form, sharing the same name rule as the Add dialog. */
const renameListSchema = z.object({
  name: z.string().trim().max(120, "Name is too long (max 120)"),
});
type RenameListValues = z.infer<typeof renameListSchema>;

/**
 * Refresh-outcome vocabulary. The colours are the app's existing ones —
 * `destructive` for the two states where a list is enforcing nothing,
 * `warning` for the one where it's still working off an older copy — so a
 * broken list reads the same way a blocked query or a stale panel does
 * elsewhere.
 *
 * `failed` and `empty` are both solid (not `-light`) because they are the
 * states the owner was blind to: a list contributing zero entries is
 * silently blocking nothing, and that has to out-shout every other badge on
 * the row. They differ in label, not volume, because the cause differs and
 * the whole point is being able to tell "it 404'd" from "the parser
 * rejected it".
 */
const STATUS_META: Record<
  ListStatus,
  { label: string; variant: NonNullable<BadgeProps["variant"]>; icon: typeof CircleCheck }
> = {
  pending: { label: "Pending", variant: "secondary", icon: Clock },
  ok: { label: "OK", variant: "success-light", icon: CircleCheck },
  stale: { label: "Stale", variant: "warning-light", icon: TriangleAlert },
  failed: { label: "Failed", variant: "destructive", icon: CircleX },
  empty: { label: "No entries", variant: "destructive", icon: FileWarning },
};

/** The two states in which a list is enforcing nothing at all. Grouped
 * because that — not the cause — is what makes them worth interrupting the
 * page for. */
function isBroken(list: List): boolean {
  return list.last_status === "failed" || list.last_status === "empty";
}

/** Falls back to `pending` for a server that predates last_status, so an
 * older API can never make the row render `undefined`. */
function statusMeta(list: List) {
  return STATUS_META[list.last_status] ?? STATUS_META.pending;
}

/** "3 lists · 142,340 entries · refreshed 4m ago" — the same shape as the
 * dashboard health strip's own list summary (see hooks/use-filters.ts's
 * useLists() doc comment and pages/dashboard.tsx's HealthStrip), but owned
 * here since this tab has the authoritative, full list data rather than
 * just a count. Tells the admin something real about their filtering setup
 * instead of a static, ever-true sentence.
 *
 * `refreshed …` counts only lists that actually refreshed successfully.
 * Taking the max over every list let one healthy list's timestamp report the
 * whole set as fresh while another was 404ing — the summary line agreeing
 * with a row that says "Failed" is how the failure stayed invisible. */
function listsSummary(lists: List[]): string {
  const totalEntries = lists.reduce((sum, l) => sum + l.entry_count, 0);
  const newestRefresh = lists.reduce((max, l) => Math.max(max, l.last_refreshed), 0);
  const broken = lists.filter(isBroken).length;
  const summary = `${lists.length} ${lists.length === 1 ? "list" : "lists"} · ${totalEntries.toLocaleString()} entries · refreshed ${relativeTime(newestRefresh)}`;
  if (broken === 0) return summary;
  return `${summary} · ${broken} not working`;
}

/**
 * The status cell's second line: what this state means for the admin, in
 * their terms.
 *
 * `never` is reserved for `pending` and nothing else. Letting it stand in
 * for a failure is the original bug — a 404 and a list that simply hadn't
 * run yet rendered identically.
 */
function StatusDetail({ list }: { list: List }) {
  switch (list.last_status) {
    case "ok":
      return <>refreshed {relativeTime(list.last_refreshed)}</>;
    case "stale":
      return (
        <>
          serving a copy from {relativeTime(list.last_refreshed)} — last try{" "}
          {relativeTime(list.last_attempt)} failed: {list.last_error}
        </>
      );
    case "failed":
    case "empty":
      return (
        <>
          blocking nothing — {list.last_error} ({relativeTime(list.last_attempt)})
        </>
      );
    default:
      return <>never refreshed — the first fetch hasn&apos;t run yet</>;
  }
}

/**
 * Page-level interruption for lists that are enforcing nothing. The row
 * badge alone is not enough: the table scrolls, a homelab can carry a dozen
 * lists, and "it should tell me if any list fetch fails" is a question about
 * the page, not about one row.
 */
function BrokenListsAlert({ lists }: { lists: List[] }) {
  const broken = lists.filter(isBroken);
  const stale = lists.filter((l) => l.last_status === "stale");

  return (
    <>
      {broken.length > 0 && (
        <Alert variant="destructive">
          <CircleX />
          <AlertTitle>
            {broken.length === 1
              ? "A filter list is blocking nothing"
              : `${broken.length} filter lists are blocking nothing`}
          </AlertTitle>
          <AlertDescription>
            <ul className="flex flex-col gap-1">
              {broken.map((l) => (
                <li key={l.id}>
                  <span className="font-medium">{l.name}</span> — {l.last_error}
                  <br />
                  <code className="font-mono break-all text-xs">{l.url}</code>
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}
      {stale.length > 0 && (
        <Alert variant="warning">
          <TriangleAlert />
          <AlertTitle>
            {stale.length === 1
              ? "A filter list is serving an older copy"
              : `${stale.length} filter lists are serving older copies`}
          </AlertTitle>
          <AlertDescription>
            Still enforcing, but the latest fetch failed.{" "}
            {stale.length === 1
              ? `${stale[0].name} — ${stale[0].last_error}`
              : "See the table below."}
          </AlertDescription>
        </Alert>
      )}
    </>
  );
}

function AddListDialog() {
  const [open, setOpen] = useState(false);
  const addList = useAddList();
  const form = useForm<AddListValues>({
    resolver: zodResolver(addListSchema),
    defaultValues: ADD_LIST_DEFAULTS,
  });

  function onOpenChange(next: boolean) {
    setOpen(next);
    if (!next) form.reset(ADD_LIST_DEFAULTS);
  }

  // Recomputed as the URL field changes, so the Name placeholder always
  // shows the default that blank would actually produce.
  const urlSoFar = form.watch("url");
  const derivedNamePreview = urlSoFar.trim() ? deriveListName(urlSoFar) : "a name from the URL";

  function onSubmit(values: AddListValues) {
    addList.mutate(
      { url: values.url.trim(), kind: values.kind, name: values.name.trim() },
      {
        onSuccess: () => {
          toast.success("List added — refreshing now");
          onOpenChange(false);
        },
        onError: (err) => {
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the list");
        },
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger render={<Button type="button" size="sm" />}>
        <Plus />
        Add list
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add a filter list</DialogTitle>
          <DialogDescription>
            dnsaur fetches this URL and refreshes it automatically going forward.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            <FormField
              control={form.control}
              name="url"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>URL</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder="https://example.com/hosts" autoComplete="off" />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            {/* The placeholder is the live derivation, so it is obvious what
                the list will be called if this is left blank — and equally
                obvious that the name is derived, not authoritative. */}
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      placeholder={derivedNamePreview}
                      autoComplete="off"
                      aria-describedby={undefined}
                    />
                  </FormControl>
                  <FormDescription>
                    Optional. Leave blank to use {derivedNamePreview}.
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="kind"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Kind</FormLabel>
                  <Select
                    items={LIST_KIND_ITEMS}
                    value={field.value}
                    onValueChange={field.onChange}
                  >
                    <SelectTrigger aria-label="List kind">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="block">Block</SelectItem>
                      <SelectItem value="allow">Allow</SelectItem>
                    </SelectContent>
                  </Select>
                  <FormDescription>
                    Allow lists are checked before block lists, so an allow entry always wins.
                  </FormDescription>
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={addList.isPending}>
                {addList.isPending ? "Adding…" : "Add list"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Rename an existing list. Blank is a legitimate submission — the server
 * reads it as "go back to the URL-derived default" — so the field is never
 * required, and the placeholder shows what blank would produce.
 */
function RenameListDialog({ list, onClose }: { list: List | null; onClose: () => void }) {
  const renameList = useRenameList();
  const form = useForm<RenameListValues>({
    resolver: zodResolver(renameListSchema),
    defaultValues: { name: "" },
    // The dialog is keyed on the list below, so it remounts per target and
    // this default is re-read each time rather than going stale.
  });

  if (!list) return null;

  function onSubmit(values: RenameListValues) {
    if (!list) return;
    const target = list;
    renameList.mutate(
      { id: target.id, name: values.name.trim() },
      {
        onSuccess: () => {
          toast.success("List renamed");
          onClose();
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : `Couldn't rename ${target.name}`),
      },
    );
  }

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rename this list</DialogTitle>
          <DialogDescription>
            Used everywhere dnsaur refers to this list instead of its URL.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder={list.name} autoComplete="off" />
                  </FormControl>
                  <FormDescription>
                    Leave blank to go back to {deriveListName(list.url)}.
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={renameList.isPending}>
                {renameList.isPending ? "Saving…" : "Save"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function ListsTable({
  lists,
  onToggle,
  togglingId,
  onDeleteRequest,
  onRenameRequest,
}: {
  lists: List[];
  onToggle: (list: List, enabled: boolean) => void;
  togglingId: number | null;
  onDeleteRequest: (list: List) => void;
  onRenameRequest: (list: List) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>List</TableHead>
          <TableHead>Kind</TableHead>
          <TableHead>Enabled</TableHead>
          <TableHead className="text-right">Entries</TableHead>
          <TableHead>Status</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {lists.map((list) => {
          const meta = KIND_META[list.kind];
          const Icon = meta.icon;
          const status = statusMeta(list);
          const StatusIcon = status.icon;
          return (
            <TableRow
              key={list.id}
              // A tint across the whole row, not just the badge: a list
              // enforcing nothing should be findable by scanning the table,
              // not by reading the last column of every row.
              className={isBroken(list) ? "bg-destructive/5" : undefined}
            >
              {/* Name primary, URL secondary. The URL stays on screen (and
                  in the title) rather than being hidden behind the name:
                  the name is derived unless the admin set one, so it has to
                  stay checkable against its source. */}
              <TableCell className="max-w-72 whitespace-normal" title={list.url}>
                <div className="flex flex-col">
                  <span className="font-medium">{list.name}</span>
                  <span className="truncate font-mono text-xs text-muted-foreground">
                    {list.url}
                  </span>
                </div>
              </TableCell>
              <TableCell>
                <Badge variant={meta.variant}>
                  <Icon />
                  {meta.label}
                </Badge>
              </TableCell>
              <TableCell>
                <Switch
                  checked={list.enabled}
                  disabled={togglingId === list.id}
                  onCheckedChange={(checked) => onToggle(list, checked)}
                  aria-label={`${list.enabled ? "Disable" : "Enable"} ${list.name}`}
                />
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {list.entry_count.toLocaleString()}
              </TableCell>
              {/* whitespace-normal because the reason wraps; the table's
                  cells are nowrap by default, which would push a "404 Not
                  Found — blocking nothing" line off the right edge. */}
              <TableCell className="max-w-80 whitespace-normal">
                <div className="flex flex-col gap-1">
                  <Badge variant={status.variant}>
                    <StatusIcon />
                    {status.label}
                  </Badge>
                  <span
                    className={
                      isBroken(list)
                        ? "text-xs text-destructive-foreground"
                        : "text-xs text-muted-foreground"
                    }
                  >
                    <StatusDetail list={list} />
                  </span>
                </div>
              </TableCell>
              <TableCell className="text-right">
                <div className="flex justify-end gap-1">
                  <Button
                    type="button"
                    size="icon-sm"
                    variant="ghost"
                    aria-label={`Rename ${list.name}`}
                    onClick={() => onRenameRequest(list)}
                  >
                    <Pencil />
                  </Button>
                  <Button
                    type="button"
                    size="icon-sm"
                    variant="ghost"
                    aria-label={`Delete ${list.name}`}
                    onClick={() => onDeleteRequest(list)}
                  >
                    <Trash2 />
                  </Button>
                </div>
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

/**
 * Filtering › Lists — block/allow list CRUD. GET/POST/PATCH/DELETE
 * /filters/lists via use-filters.ts's canonical hooks (shared with the
 * dashboard's health strip and the setup wizard's starter blocklists), plus
 * a manual "Refresh now" (POST /filters/refresh, fire-and-forget 202).
 */
export function ListsTab() {
  const lists = useLists();
  const toggleList = useToggleList();
  const deleteList = useDeleteList();
  const refresh = useRefreshFilters();

  const [togglingId, setTogglingId] = useState<number | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<List | null>(null);
  const [renameTarget, setRenameTarget] = useState<List | null>(null);

  function onToggle(list: List, enabled: boolean) {
    setTogglingId(list.id);
    toggleList.mutate(
      { id: list.id, enabled },
      {
        onError: () => toast.error(`Couldn't ${enabled ? "enable" : "disable"} ${list.name}`),
        onSettled: () => setTogglingId(null),
      },
    );
  }

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteList.mutate(target.id, {
      onSuccess: () => {
        toast.success("List deleted");
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  function onRefresh() {
    refresh.mutate(undefined, {
      onSuccess: () => toast.success("Refreshing filter lists…"),
      onError: () => toast.error("Couldn't start a refresh — try again"),
    });
  }

  const isEmpty = lists.data?.length === 0;

  let body: ReactNode;
  if (lists.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (lists.data === undefined) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load filter lists</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (isEmpty) {
    body = (
      <EmptyState
        icon={<ListChecks />}
        title="No filter lists yet"
        description="Add a blocklist or allowlist URL to start filtering DNS queries."
        action={<AddListDialog />}
      />
    );
  } else {
    body = (
      <>
        <BrokenListsAlert lists={lists.data} />
        <ListsTable
          lists={lists.data}
          onToggle={onToggle}
          togglingId={togglingId}
          onDeleteRequest={setDeleteTarget}
          onRenameRequest={setRenameTarget}
        />
      </>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          {lists.data && lists.data.length > 0
            ? listsSummary(lists.data)
            : "Blocklists and allowlists dnsaur fetches and refreshes automatically."}
        </p>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onRefresh}
            disabled={refresh.isPending}
          >
            <RefreshCw className={refresh.isPending ? "animate-spin" : undefined} />
            {refresh.isPending ? "Refreshing…" : "Refresh now"}
          </Button>
          {!isEmpty && <AddListDialog />}
        </div>
      </div>

      {lists.isError && lists.data !== undefined && (
        <StaleDataAlert
          what="filter lists"
          onRetry={() => void lists.refetch()}
          isRetrying={lists.isFetching}
        />
      )}

      {body}

      <RenameListDialog list={renameTarget} onClose={() => setRenameTarget(null)} />

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this list?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <span className="font-medium text-foreground">{deleteTarget.name}</span> (
                  <code className="font-mono break-all">{deleteTarget.url}</code>) will stop being
                  fetched, and its entries will no longer be enforced.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteList.isPending}
            >
              {deleteList.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
