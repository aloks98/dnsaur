import { useState, type ReactNode } from "react";
import {
  ListChecks,
  Plus,
  RefreshCw,
  ShieldBan,
  ShieldCheck,
  Trash2,
  TriangleAlert,
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
import type { List } from "../../api/types";
import {
  useAddList,
  useDeleteList,
  useLists,
  useRefreshFilters,
  useToggleList,
} from "../../hooks/use-filters";
import { relativeTime } from "../../lib/format";

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

interface AddListValues {
  url: string;
  kind: "block" | "allow";
}

const ADD_LIST_DEFAULTS: AddListValues = { url: "", kind: "block" };

/** Mirrors the server's own check (net/url.Parse + scheme/host, see
 * internal/api/filters_handlers.go's handleListCreate) so a bad URL never
 * even reaches the network — the 400 the server would return is instead
 * caught inline, before submit. */
function validateListUrl(value: string): string | true {
  const trimmed = value.trim();
  if (!trimmed) return "URL is required";
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return "Enter a valid URL, e.g. https://example.com/hosts";
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    return "URL must start with http:// or https://";
  }
  if (!parsed.host) return "URL must include a host";
  return true;
}

/** "3 lists · 142,340 entries · refreshed 4m ago" — the same shape as the
 * dashboard health strip's own list summary (see hooks/use-filters.ts's
 * useLists() doc comment and pages/dashboard.tsx's HealthStrip), but owned
 * here since this tab has the authoritative, full list data rather than
 * just a count. Tells the admin something real about their filtering setup
 * instead of a static, ever-true sentence. */
function listsSummary(lists: List[]): string {
  const totalEntries = lists.reduce((sum, l) => sum + l.entry_count, 0);
  const newestRefresh = lists.reduce((max, l) => Math.max(max, l.last_refreshed), 0);
  return `${lists.length} ${lists.length === 1 ? "list" : "lists"} · ${totalEntries.toLocaleString()} entries · refreshed ${relativeTime(newestRefresh)}`;
}

function AddListDialog() {
  const [open, setOpen] = useState(false);
  const addList = useAddList();
  const form = useForm<AddListValues>({ defaultValues: ADD_LIST_DEFAULTS });

  function onOpenChange(next: boolean) {
    setOpen(next);
    if (!next) form.reset(ADD_LIST_DEFAULTS);
  }

  function onSubmit(values: AddListValues) {
    addList.mutate(
      { url: values.url.trim(), kind: values.kind },
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
              rules={{ validate: validateListUrl }}
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

function ListsTable({
  lists,
  onToggle,
  togglingId,
  onDeleteRequest,
}: {
  lists: List[];
  onToggle: (list: List, enabled: boolean) => void;
  togglingId: number | null;
  onDeleteRequest: (list: List) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>URL</TableHead>
          <TableHead>Kind</TableHead>
          <TableHead>Enabled</TableHead>
          <TableHead className="text-right">Entries</TableHead>
          <TableHead>Last refreshed</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {lists.map((list) => {
          const meta = KIND_META[list.kind];
          const Icon = meta.icon;
          return (
            <TableRow key={list.id}>
              <TableCell className="max-w-72 truncate font-mono text-xs" title={list.url}>
                {list.url}
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
                  aria-label={`${list.enabled ? "Disable" : "Enable"} ${list.url}`}
                />
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {list.entry_count.toLocaleString()}
              </TableCell>
              <TableCell className="text-xs text-muted-foreground">
                {relativeTime(list.last_refreshed)}
              </TableCell>
              <TableCell className="text-right">
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Delete ${list.url}`}
                  onClick={() => onDeleteRequest(list)}
                >
                  <Trash2 />
                </Button>
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

  function onToggle(list: List, enabled: boolean) {
    setTogglingId(list.id);
    toggleList.mutate(
      { id: list.id, enabled },
      {
        onError: () => toast.error(`Couldn't ${enabled ? "enable" : "disable"} ${list.url}`),
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
      onError: () => toast.error(`Couldn't delete ${target.url}`),
    });
  }

  function onRefresh() {
    refresh.mutate(undefined, {
      onSuccess: () => toast.success("Refreshing filter lists…"),
      onError: () => toast.error("Couldn't start a refresh — try again"),
    });
  }

  const isEmpty = lists.isSuccess && lists.data.length === 0;

  let body: ReactNode;
  if (lists.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (lists.isError) {
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
      <ListsTable
        lists={lists.data}
        onToggle={onToggle}
        togglingId={togglingId}
        onDeleteRequest={setDeleteTarget}
      />
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          {lists.isSuccess && lists.data.length > 0
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

      {body}

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
                  <code className="font-mono break-all text-foreground">{deleteTarget.url}</code>{" "}
                  will stop being fetched, and its entries will no longer be enforced.
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
