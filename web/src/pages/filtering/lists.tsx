import { useMemo, useState, type ReactNode } from "react";
import { Pencil, Plus, RefreshCw, Trash2, TriangleAlert } from "lucide-react";
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  Switch,
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

/** Block/allow, as a colour pair rather than words in a sentence. */
const KIND_VARIANT: Record<List["kind"], NonNullable<BadgeProps["variant"]>> = {
  block: "destructive-light",
  allow: "primary-light",
};

/**
 * What each refresh outcome means for enforcement, which is the only
 * question this column exists to answer.
 *
 * `failed` and `empty` share a colour because they share a consequence —
 * the list is enforcing nothing — and differ in label because they need
 * different fixes. That distinction is the whole reason last_status exists:
 * `entry_count: 0` cannot tell "the download broke" from "the parser
 * rejected every line", and those send an admin looking in opposite
 * directions.
 */
const STATUS_META: Record<ListStatus, { label: string; dot: string; text: string }> = {
  pending: { label: "Pending", dot: "bg-muted-foreground", text: "text-muted-foreground" },
  ok: { label: "OK", dot: "bg-success", text: "text-success-foreground" },
  stale: { label: "Stale", dot: "bg-warning", text: "text-warning-foreground" },
  failed: { label: "Failed", dot: "bg-destructive", text: "text-destructive-foreground" },
  empty: { label: "No entries", dot: "bg-destructive", text: "text-destructive-foreground" },
};

/** The two states in which a list enforces nothing at all. Grouped because
 * that — not the cause — is what makes them worth interrupting the page for. */
function isIdle(list: List): boolean {
  return list.last_status === "failed" || list.last_status === "empty";
}

/** Falls back to `pending` for a server predating last_status, so an older
 * API can never make a row render `undefined`. */
function statusMeta(list: List) {
  return STATUS_META[list.last_status] ?? STATUS_META.pending;
}

/**
 * The status cell's second line: what this state means, in the admin's
 * terms.
 *
 * "never" belongs to `pending` and nothing else. Letting it stand in for a
 * failure was the original bug — a 404 and a list that simply had not run
 * yet rendered identically.
 */
function statusDetail(list: List): string {
  switch (list.last_status) {
    case "ok":
      return `Refreshed ${relativeTime(list.last_refreshed)}. Enforcing now.`;
    case "stale":
      return `Refresh failed — still enforcing the copy from ${relativeTime(list.last_refreshed)}. Last tried ${relativeTime(list.last_attempt)}: ${list.last_error}`;
    case "failed":
      return `Download failed — ${list.last_error}. No usable copy, so this list is blocking nothing.`;
    case "empty":
      // last_error carries *why* the parser produced nothing, which is the
      // only actionable part — without it this says "no entries" twice and
      // sends the admin looking at their network, which is not the problem.
      return `No usable entries — ${list.last_error}. The download itself worked, so this is a format problem, not a network one; the list is blocking nothing.`;
    default:
      return "Added but not yet fetched. 0 entries is expected until the first refresh lands.";
  }
}

/** One declaration of the column geometry, shared by the header and every
 * row. Two copies of a six-column template is how they drift apart. */
const GRID = "grid grid-cols-[1fr_96px_84px_96px_316px_84px] items-center gap-3.5 px-4";

/** Mirrors the server's own check (net/url.Parse + scheme/host, see
 * internal/api/filters_handlers.go's handleListCreate) so a bad URL never
 * reaches the network — the 400 the server would return is caught inline
 * instead. One refinement rather than a chain: each step depends on the
 * previous having parsed, and each has its own message. */
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
  // Optional: blank means "use the server's URL-derived default", shown live
  // as the field's placeholder. Bounded to match the API's own cap
  // (internal/api/filters_handlers.go's maxListNameLen).
  name: z.string().trim().max(120, "Name is too long (max 120)"),
});
type AddListValues = z.infer<typeof addListSchema>;
const ADD_LIST_DEFAULTS: AddListValues = { url: "", kind: "block", name: "" };

const renameListSchema = z.object({
  name: z.string().trim().max(120, "Name is too long (max 120)"),
});
type RenameListValues = z.infer<typeof renameListSchema>;

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

  // Recomputed as the URL changes, so the Name placeholder always shows the
  // default that leaving it blank would actually produce.
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
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the list"),
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
            dnsaur fetches this URL now and refreshes it automatically from then on.
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
                    <Input
                      {...field}
                      placeholder="https://example.com/hosts"
                      autoComplete="off"
                      className="font-mono"
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            {/* The placeholder is the live derivation, so it is obvious both
                what the list will be called if this is left blank, and that
                the name is derived rather than authoritative. */}
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
                  <FormDescription>Optional. Blank uses {derivedNamePreview}.</FormDescription>
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
                  <FormControl>
                    <NativeSelect {...field}>
                      <NativeSelectOption value="block">Block</NativeSelectOption>
                      <NativeSelectOption value="allow">Allow</NativeSelectOption>
                    </NativeSelect>
                  </FormControl>
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
            Used everywhere dnsaur would otherwise print the raw URL.
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
                  <FormDescription>Blank goes back to {deriveListName(list.url)}.</FormDescription>
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

type StatusFilter = "" | "attention" | ListStatus;

/**
 * Filtering › Lists — block/allow list CRUD over use-filters.ts's canonical
 * hooks, plus a manual refresh (POST /filters/refresh, fire-and-forget 202).
 */
export function ListsTab() {
  const lists = useLists();
  const toggleList = useToggleList();
  const deleteList = useDeleteList();
  const refresh = useRefreshFilters();

  const [search, setSearch] = useState("");
  const [kindFilter, setKindFilter] = useState<"" | List["kind"]>("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("");
  const [togglingId, setTogglingId] = useState<number | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<List | null>(null);
  const [renameTarget, setRenameTarget] = useState<List | null>(null);

  const all = useMemo(() => lists.data ?? [], [lists.data]);
  const idle = useMemo(() => all.filter(isIdle), [all]);
  const enforcing = useMemo(
    () => all.filter((l) => l.enabled && (l.last_status === "ok" || l.last_status === "stale")),
    [all],
  );

  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return all.filter((l) => {
      if (kindFilter && l.kind !== kindFilter) return false;
      // "Needs attention" is the union of every state that wants a human:
      // enforcing nothing, or enforcing an ageing copy.
      if (statusFilter === "attention" && !isIdle(l) && l.last_status !== "stale") return false;
      if (statusFilter && statusFilter !== "attention" && l.last_status !== statusFilter) {
        return false;
      }
      if (q && !l.name.toLowerCase().includes(q) && !l.url.toLowerCase().includes(q)) return false;
      return true;
    });
  }, [all, search, kindFilter, statusFilter]);

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

  let body: ReactNode;
  if (lists.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (lists.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load filter lists</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (all.length === 0) {
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        No lists subscribed yet. Add a blocklist or allowlist URL and dnsaur will fetch it and keep
        it refreshed.
      </p>
    );
  } else if (shown.length === 0) {
    body = (
      <p className="p-6 text-center text-sm text-muted-foreground">
        No lists match this filter.{" "}
        <Button
          type="button"
          variant="link"
          size="sm"
          onClick={() => {
            setSearch("");
            setKindFilter("");
            setStatusFilter("");
          }}
        >
          Clear it
        </Button>
      </p>
    );
  } else {
    body = shown.map((list) => {
      const status = statusMeta(list);
      const dead = isIdle(list);
      return (
        <div
          key={list.id}
          data-slot="list-row"
          className={cn(
            "border-b border-border-muted py-3",
            GRID,
            // A tint and a left rule across the whole row, not just a badge
            // in the fifth column: a list enforcing nothing has to be
            // findable by scanning, not by reading every row to the end.
            dead && "bg-destructive/5 shadow-[inset_2px_0_0_var(--destructive)]",
          )}
        >
          <span className="flex min-w-0 flex-col gap-1">
            <span className="truncate text-sm font-medium">{list.name}</span>
            {/* The URL stays on screen rather than behind the name: the name
                is derived unless the admin set one, so it has to stay
                checkable against its source. */}
            <span className="truncate font-mono text-xs text-muted-foreground" title={list.url}>
              {list.url}
            </span>
          </span>
          <span>
            <Badge variant={KIND_VARIANT[list.kind]}>{list.kind}</Badge>
          </span>
          <span>
            <Switch
              checked={list.enabled}
              disabled={togglingId === list.id}
              onCheckedChange={(checked) => onToggle(list, checked)}
              aria-label={`${list.enabled ? "Disable" : "Enable"} ${list.name}`}
            />
          </span>
          <span
            className={cn(
              "text-right font-mono text-sm",
              list.entry_count === 0 && "text-muted-foreground",
            )}
          >
            {list.last_status === "pending" ? "—" : list.entry_count.toLocaleString()}
          </span>
          <span className="flex min-w-0 flex-col gap-1.5">
            <span className="flex items-center gap-2">
              <span aria-hidden className={cn("size-1.5 shrink-0", status.dot)} />
              <span
                className={cn(
                  "font-mono text-xs font-semibold tracking-widest uppercase",
                  status.text,
                )}
              >
                {status.label}
              </span>
            </span>
            <span className="text-xs text-pretty text-muted-foreground">{statusDetail(list)}</span>
          </span>
          <span className="flex items-center justify-end gap-1">
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label={`Rename ${list.name}`}
              onClick={() => setRenameTarget(list)}
            >
              <Pencil />
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label={`Delete ${list.name}`}
              onClick={() => setDeleteTarget(list)}
            >
              <Trash2 />
            </Button>
          </span>
        </div>
      );
    });
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-3 border-b border-border px-4 py-2.5">
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="search lists…"
          aria-label="Search lists"
          className="w-60"
        />
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            Kind
          </span>
          <NativeSelect
            value={kindFilter}
            onChange={(e) => setKindFilter(e.target.value as "" | List["kind"])}
            aria-label="Filter by kind"
          >
            <NativeSelectOption value="">All kinds</NativeSelectOption>
            <NativeSelectOption value="block">block</NativeSelectOption>
            <NativeSelectOption value="allow">allow</NativeSelectOption>
          </NativeSelect>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            Status
          </span>
          <NativeSelect
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}
            aria-label="Filter by status"
          >
            <NativeSelectOption value="">Any status</NativeSelectOption>
            <NativeSelectOption value="attention">Needs attention</NativeSelectOption>
            <NativeSelectOption value="ok">ok</NativeSelectOption>
            <NativeSelectOption value="stale">stale</NativeSelectOption>
            <NativeSelectOption value="failed">failed</NativeSelectOption>
            <NativeSelectOption value="empty">empty</NativeSelectOption>
            <NativeSelectOption value="pending">pending</NativeSelectOption>
          </NativeSelect>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onRefresh}
            disabled={refresh.isPending}
          >
            <RefreshCw className={refresh.isPending ? "animate-spin" : undefined} />
            {refresh.isPending ? "Refreshing…" : "Refresh all"}
          </Button>
          <AddListDialog />
        </div>
      </div>

      {/* The row badge alone is not enough: the table scrolls, a homelab can
          carry a dozen lists, and "tell me if a list fetch failed" is a
          question about the page, not about one row. */}
      {idle.length > 0 && (
        <div className="shrink-0 border-b border-border p-3">
          <Alert variant="warning">
            <TriangleAlert />
            <AlertTitle>
              {idle.length === 1
                ? "A list is enabled but blocking nothing"
                : `${idle.length} of ${all.length} lists are enabled but blocking nothing`}
            </AlertTitle>
            {/* One per line, name first. Joined into a sentence these ran
                together into an unreadable paragraph: three names, three
                parenthesised errors, and no structure to scan. The reason a
                list is broken is the actionable part, so it gets its own
                line rather than a bracket — and the trailing explanation of
                why the two failure kinds differ is already said, better, by
                each row's own status line. */}
            <AlertDescription>
              <ul className="flex flex-col gap-1">
                {idle.map((l) => (
                  <li key={l.id} className="text-pretty">
                    <span className="font-medium text-foreground">{l.name}</span>
                    {" — "}
                    {l.last_error || l.last_status}
                  </li>
                ))}
              </ul>
            </AlertDescription>
          </Alert>
        </div>
      )}

      {lists.isError && lists.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="filter lists"
            onRetry={() => void lists.refetch()}
            isRetrying={lists.isFetching}
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
        <span>List</span>
        <span>Kind</span>
        <span>Enabled</span>
        <span className="text-right">Entries</span>
        <span>Status</span>
        <span className="text-right">Actions</span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <div className="flex shrink-0 items-center gap-4 border-t border-border px-4 py-2 font-mono text-xs text-muted-foreground">
        <span>{enforcing.length} enforcing</span>
        <span className={cn(idle.length > 0 && "text-destructive-foreground")}>
          {idle.length} enabled but idle
        </span>
        <span className="ml-auto max-lg:hidden">
          refresh is fire-and-forget — counts land after
        </span>
      </div>

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
