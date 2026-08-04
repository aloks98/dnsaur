import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  FolderTree,
  ListChecks,
  PencilLine,
  Plus,
  Trash2,
  TriangleAlert,
  Users,
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
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  EmptyState,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemTitle,
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
} from "@e412/rnui-react";
import type { UseQueryResult } from "@tanstack/react-query";
import { ApiError } from "../../api/client";
import type { Client, Group, List } from "../../api/types";
import { useLists } from "../../hooks/use-filters";
import {
  useAddClient,
  useClients,
  useDeleteClient,
  useUpdateClient,
} from "../../hooks/use-clients";
import {
  useAddGroup,
  useDeleteGroup,
  useGroupLists,
  useGroups,
  useRenameGroup,
  useSetGroupLists,
  useToggleGroup,
} from "../../hooks/use-groups";
import { PauseControl } from "../../components/pause-control";

// The seeded, structural group — the server refuses to delete it
// (internal/store/crud.go's DeleteGroup treats id 1 specially), so its row
// disables the delete control up front instead of letting the click round
// trip to a 409.
const DEFAULT_GROUP_ID = 1;

function friendlyDeleteError(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 409) return fallback;
  return err instanceof ApiError ? err.message : fallback;
}

// --- Groups panel --------------------------------------------------------

interface GroupFormValues {
  name: string;
}

function validateName(value: string): string | true {
  return value.trim() ? true : "Name is required";
}

function AddGroupDialog() {
  const [open, setOpen] = useState(false);
  const addGroup = useAddGroup();
  const form = useForm<GroupFormValues>({ defaultValues: { name: "" } });

  function onOpenChange(next: boolean) {
    setOpen(next);
    if (!next) form.reset({ name: "" });
  }

  function onSubmit(values: GroupFormValues) {
    addGroup.mutate(values.name.trim(), {
      onSuccess: () => {
        toast.success("Group added");
        onOpenChange(false);
      },
      onError: (err) =>
        toast.error(err instanceof ApiError ? err.message : "Couldn't add the group"),
    });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger render={<Button type="button" size="sm" />}>
        <Plus />
        New group
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New group</DialogTitle>
          <DialogDescription>
            Groups scope rules, filter lists, and blocking pauses to a set of clients.
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
              rules={{ validate: validateName }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder="Kids' devices" autoComplete="off" />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={addGroup.isPending}>
                {addGroup.isPending ? "Adding…" : "Add group"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function RenameGroupDialog({
  group,
  open,
  onOpenChange,
}: {
  group: Group;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const renameGroup = useRenameGroup();
  const form = useForm<GroupFormValues>({ defaultValues: { name: group.name } });

  // Re-sync whenever the dialog opens (rather than on every group.name
  // change) — externally triggered opens (the row's edit button) don't run
  // through this Dialog's own onOpenChange, so a plain reset-on-close
  // wouldn't catch them.
  useEffect(() => {
    if (open) form.reset({ name: group.name });
  }, [open, group.name, form]);

  function onSubmit(values: GroupFormValues) {
    renameGroup.mutate(
      { id: group.id, name: values.name.trim() },
      {
        onSuccess: () => {
          toast.success("Group renamed");
          onOpenChange(false);
        },
        onError: () => toast.error(`Couldn't rename ${group.name}`),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rename group</DialogTitle>
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
              rules={{ validate: validateName }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input {...field} autoComplete="off" />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={renameGroup.isPending}>
                {renameGroup.isPending ? "Saving…" : "Save"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function GroupListsMenu({ group, allLists }: { group: Group; allLists: List[] }) {
  const groupLists = useGroupLists(group.id);
  const setGroupLists = useSetGroupLists();
  const assignedIds = useMemo(
    () => new Set(groupLists.data?.map((l) => l.id) ?? []),
    [groupLists.data],
  );

  function onToggle(listId: number, checked: boolean) {
    const current = groupLists.data?.map((l) => l.id) ?? [];
    const next = checked ? [...current, listId] : current.filter((id) => id !== listId);
    setGroupLists.mutate(
      { groupId: group.id, listIds: next },
      { onError: () => toast.error(`Couldn't update lists for ${group.name}`) },
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<Button type="button" variant="outline" size="sm" />}>
        <ListChecks />
        {/* A single text node, not "Lists" + a sibling element — the ARIA
            name-from-content algorithm trims each child node's own
            contribution before concatenating with no separator, so
            splitting the count into a sibling <span> silently loses the
            space in the *computed accessible name* even though
            textContent looks right. */}
        {groupLists.isSuccess ? `Lists (${groupLists.data.length})` : "Lists"}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuGroup>
          <DropdownMenuLabel>Lists applied to {group.name}</DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        {allLists.length === 0 ? (
          <DropdownMenuItem disabled>No filter lists yet</DropdownMenuItem>
        ) : (
          allLists.map((list) => (
            <DropdownMenuCheckboxItem
              key={list.id}
              checked={assignedIds.has(list.id)}
              disabled={setGroupLists.isPending || groupLists.isPending}
              onCheckedChange={(checked) => onToggle(list.id, checked)}
            >
              {list.url}
            </DropdownMenuCheckboxItem>
          ))
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function GroupRow({ group, allLists }: { group: Group; allLists: List[] }) {
  const toggleGroup = useToggleGroup();
  const deleteGroup = useDeleteGroup();
  const [renameOpen, setRenameOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const isDefault = group.id === DEFAULT_GROUP_ID;

  function onToggle(enabled: boolean) {
    toggleGroup.mutate(
      { id: group.id, enabled },
      { onError: () => toast.error(`Couldn't ${enabled ? "enable" : "disable"} ${group.name}`) },
    );
  }

  function onConfirmDelete() {
    deleteGroup.mutate(group.id, {
      onSuccess: () => {
        toast.success(`${group.name} deleted`);
        setDeleteOpen(false);
      },
      onError: (err) => {
        toast.error(friendlyDeleteError(err, `Can't delete ${group.name} — it's still in use`));
      },
    });
  }

  return (
    <Item variant="outline" className="flex-wrap items-center gap-x-4 gap-y-3">
      <ItemContent className="min-w-40">
        <ItemTitle className="flex items-center gap-2">
          <span className="min-w-0 truncate" title={group.name}>
            {group.name}
          </span>
          {/* "Built-in", not "Default" — the seeded group is itself
              typically *named* "default" (see internal/app/app.go), so a
              same-named badge would just repeat the row's own title. */}
          {isDefault && (
            <Badge variant="secondary" className="shrink-0">
              Built-in
            </Badge>
          )}
        </ItemTitle>
        <ItemDescription>{group.enabled ? "Enforcing" : "Disabled"}</ItemDescription>
      </ItemContent>
      <ItemActions className="flex flex-wrap items-center gap-2">
        <PauseControl groupId={group.id} />
        <GroupListsMenu group={group} allLists={allLists} />
        <Switch
          checked={group.enabled}
          disabled={toggleGroup.isPending}
          onCheckedChange={onToggle}
          aria-label={`${group.enabled ? "Disable" : "Enable"} ${group.name}`}
        />
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Rename ${group.name}`}
          onClick={() => setRenameOpen(true)}
        >
          <PencilLine />
        </Button>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Delete ${group.name}`}
          disabled={isDefault}
          title={isDefault ? "The default group can't be deleted" : undefined}
          onClick={() => setDeleteOpen(true)}
        >
          <Trash2 />
        </Button>
      </ItemActions>

      <RenameGroupDialog group={group} open={renameOpen} onOpenChange={setRenameOpen} />
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this group?</AlertDialogTitle>
            <AlertDialogDescription>
              <code className="font-mono break-all text-foreground">{group.name}</code>, its rules,
              and its list assignments will be removed. Clients assigned to it must be moved first.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteGroup.isPending}
            >
              {deleteGroup.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Item>
  );
}

function GroupsPanel({ groupsQuery }: { groupsQuery: UseQueryResult<Group[]> }) {
  const lists = useLists();
  const allLists = lists.data ?? [];

  let body: ReactNode;
  if (groupsQuery.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-16 w-full" />
        ))}
      </div>
    );
  } else if (groupsQuery.isError) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load groups</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (groupsQuery.data.length === 0) {
    body = (
      <EmptyState
        icon={<FolderTree />}
        title="No groups yet"
        description="Create a group to scope rules and filter lists to specific clients."
        action={<AddGroupDialog />}
      />
    );
  } else {
    body = (
      <div className="flex flex-col gap-2">
        {groupsQuery.data.map((group) => (
          <GroupRow key={group.id} group={group} allLists={allLists} />
        ))}
      </div>
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-foreground">Groups</h2>
          <p className="text-sm text-muted-foreground">Policy scopes clients are assigned to.</p>
        </div>
        {groupsQuery.isSuccess && groupsQuery.data.length > 0 && <AddGroupDialog />}
      </div>
      {body}
    </section>
  );
}

// --- Clients panel ---------------------------------------------------------

interface ClientFormValues {
  name: string;
  matcher: string;
  groupId: string;
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

function isValidIPv6(value: string): boolean {
  if (!value.includes(":")) return false;
  try {
    // The URL parser validates bracketed IPv6 host syntax for us — a
    // pragmatic stand-in for a real IPv6 parser (Go's net/netip on the
    // server, see internal/api/clients_handlers.go's validMatcher) that's
    // good enough to catch typos before the round trip.
    new URL(`http://[${value}]`);
    return true;
  } catch {
    return false;
  }
}

/** Mirrors the server's own validMatcher check (net/netip.ParseAddr or
 * ParsePrefix, see internal/api/clients_handlers.go) closely enough to
 * catch typos before they round-trip as a 400. */
function validateMatcher(value: string): string | true {
  const trimmed = value.trim();
  if (!trimmed) return "Matcher is required";
  const parts = trimmed.split("/");
  if (parts.length > 2) return "Enter a single IP address or CIDR range, e.g. 192.168.1.0/24";
  const [addr, prefix] = parts;
  const isV4 = isValidIPv4(addr);
  const isV6 = !isV4 && isValidIPv6(addr);
  if (!isV4 && !isV6) {
    return "Enter a valid IP address or CIDR range, e.g. 192.168.1.10 or 192.168.1.0/24";
  }
  if (prefix !== undefined) {
    if (!/^\d+$/.test(prefix)) return "CIDR prefix must be a number";
    const prefixNum = Number(prefix);
    const max = isV4 ? 32 : 128;
    if (prefixNum > max) return `CIDR prefix must be between 0 and ${max}`;
  }
  return true;
}

function clientFormDefaults(client: Client | null, fallbackGroupId: number): ClientFormValues {
  return {
    name: client?.name ?? "",
    matcher: client?.matcher ?? "",
    groupId: String(client?.group_id ?? fallbackGroupId),
  };
}

function ClientFormDialog({
  client,
  groups,
  open,
  onOpenChange,
}: {
  /** `null` renders the "Add client" copy; a Client renders "Edit client". */
  client: Client | null;
  groups: Group[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const addClient = useAddClient();
  const updateClient = useUpdateClient();
  const isEdit = client !== null;
  const fallbackGroupId = groups[0]?.id ?? DEFAULT_GROUP_ID;
  const form = useForm<ClientFormValues>({
    defaultValues: clientFormDefaults(client, fallbackGroupId),
  });

  useEffect(() => {
    if (open) form.reset(clientFormDefaults(client, fallbackGroupId));
  }, [open, client, fallbackGroupId, form]);

  function onSubmit(values: ClientFormValues) {
    const payload = {
      name: values.name.trim(),
      matcher: values.matcher.trim(),
      group_id: Number(values.groupId),
    };
    const onSuccess = () => {
      toast.success(isEdit ? "Client updated" : "Client added");
      onOpenChange(false);
    };
    const onError = (err: unknown) =>
      toast.error(
        err instanceof ApiError ? err.message : `Couldn't ${isEdit ? "update" : "add"} the client`,
      );
    if (isEdit && client) {
      updateClient.mutate({ id: client.id, ...payload }, { onSuccess, onError });
    } else {
      addClient.mutate(payload, { onSuccess, onError });
    }
  }

  const busy = addClient.isPending || updateClient.isPending;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{isEdit ? "Edit client" : "Add client"}</DialogTitle>
          <DialogDescription>
            Match a device by its IP address or a CIDR range, then assign it to a group.
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
              rules={{ validate: validateName }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder="Kid's laptop" autoComplete="off" />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="matcher"
              rules={{ validate: validateMatcher }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Matcher</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      placeholder="192.168.1.42 or 192.168.1.0/24"
                      autoComplete="off"
                      className="font-mono"
                    />
                  </FormControl>
                  <FormDescription>
                    An IP address or CIDR range to match incoming queries against.
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="groupId"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Group</FormLabel>
                  <Select value={field.value} onValueChange={field.onChange}>
                    <SelectTrigger aria-label="Group">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {groups.map((g) => (
                        <SelectItem key={g.id} value={String(g.id)}>
                          {g.name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={busy}>
                {busy ? "Saving…" : isEdit ? "Save" : "Add client"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function ClientsTable({
  clients,
  groupsById,
  onEdit,
  onDeleteRequest,
}: {
  clients: Client[];
  groupsById: Map<number, Group>;
  onEdit: (client: Client) => void;
  onDeleteRequest: (client: Client) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Matcher</TableHead>
          <TableHead>Group</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {clients.map((client) => (
          <TableRow key={client.id}>
            <TableCell className="max-w-56 truncate" title={client.name}>
              {client.name}
            </TableCell>
            <TableCell className="font-mono text-xs">{client.matcher}</TableCell>
            <TableCell>
              <Badge variant="secondary">
                {groupsById.get(client.group_id)?.name ?? `Group ${client.group_id}`}
              </Badge>
            </TableCell>
            <TableCell className="text-right">
              <div className="flex justify-end gap-1">
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Edit ${client.name}`}
                  onClick={() => onEdit(client)}
                >
                  <PencilLine />
                </Button>
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Delete ${client.name}`}
                  onClick={() => onDeleteRequest(client)}
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

function ClientsPanel({ groups, groupsLoading }: { groups: Group[]; groupsLoading: boolean }) {
  const clients = useClients();
  const deleteClient = useDeleteClient();
  const [addOpen, setAddOpen] = useState(false);
  const [editTarget, setEditTarget] = useState<Client | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Client | null>(null);

  const groupsById = useMemo(() => new Map(groups.map((g) => [g.id, g])), [groups]);
  const noGroups = !groupsLoading && groups.length === 0;

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteClient.mutate(target.id, {
      onSuccess: () => {
        toast.success("Client deleted");
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  const isEmpty = clients.isSuccess && clients.data.length === 0;

  let body: ReactNode;
  if (clients.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (clients.isError) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load clients</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (isEmpty) {
    body = (
      <EmptyState
        icon={<Users />}
        title="No clients yet"
        description={
          noGroups
            ? "Create a group first, then add a client and match it by IP or CIDR range."
            : "Add a client and match it by IP address or CIDR range."
        }
        action={
          <Button type="button" size="sm" disabled={noGroups} onClick={() => setAddOpen(true)}>
            <Plus />
            Add client
          </Button>
        }
      />
    );
  } else {
    body = (
      <ClientsTable
        clients={clients.data}
        groupsById={groupsById}
        onEdit={setEditTarget}
        onDeleteRequest={setDeleteTarget}
      />
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h2 className="text-sm font-semibold text-foreground">Clients</h2>
          <p className="text-sm text-muted-foreground">
            Devices matched by IP or CIDR, assigned to a group.
          </p>
        </div>
        {!isEmpty && (
          <Button
            type="button"
            size="sm"
            disabled={noGroups}
            title={noGroups ? "Create a group first" : undefined}
            onClick={() => setAddOpen(true)}
          >
            <Plus />
            Add client
          </Button>
        )}
      </div>

      {body}

      <ClientFormDialog client={null} groups={groups} open={addOpen} onOpenChange={setAddOpen} />
      <ClientFormDialog
        client={editTarget}
        groups={groups}
        open={editTarget !== null}
        onOpenChange={(next) => !next && setEditTarget(null)}
      />

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this client?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <code className="font-mono break-all text-foreground">{deleteTarget.name}</code>{" "}
                  will no longer be matched or assigned to a group.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteClient.isPending}
            >
              {deleteClient.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}

/**
 * Filtering › Groups & Clients — two related panels side by side on wide
 * screens (groups are few and structural; clients are the denser, more
 * numerous list that references them), stacked on narrow ones. Groups own
 * enable/pause/rename/delete plus which filter lists apply to them; Clients
 * own the IP/CIDR-to-group assignment.
 */
export function GroupsClientsTab() {
  const groupsQuery = useGroups();

  return (
    <div className="grid grid-cols-1 gap-8 lg:grid-cols-[minmax(0,380px)_1fr] lg:items-start">
      <GroupsPanel groupsQuery={groupsQuery} />
      <ClientsPanel groups={groupsQuery.data ?? []} groupsLoading={groupsQuery.isPending} />
    </div>
  );
}
