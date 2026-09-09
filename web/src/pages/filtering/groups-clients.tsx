import { useEffect, useMemo, useState, type ReactNode } from "react";
import { CircleAlert, CircleHelp, Pencil, Plus, Trash2, TriangleAlert, X } from "lucide-react";
import { toast } from "sonner";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  cn,
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Form,
  FormControl,
  FormField,
  FormItem,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Popover,
  PopoverContent,
  PopoverTrigger,
  Skeleton,
  Switch,
  type BadgeProps,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { Client, Group, List } from "../../api/types";
import { useLists } from "../../hooks/use-filters";
import { useAddClient, useClients, useDeleteClient } from "../../hooks/use-clients";
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
import { StaleDataAlert } from "../../components/stale-data-alert";
import { isValidIPv4, isValidIPv6, requiredText } from "../../lib/schemas";
import { DEFAULT_GROUP_ID } from "../../lib/query-rows";
import { ConfirmDeleteDialog, RenameDialog } from "../dialogs";

function friendlyDeleteError(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 409) return fallback;
  return err instanceof ApiError ? err.message : fallback;
}

const GROUP_GRID = "grid grid-cols-[1fr_78px_156px_84px_168px_76px] items-center gap-3.5 px-4";
const CLIENT_GRID = "grid grid-cols-[1fr_208px_152px_108px] items-center gap-3.5 px-4";

/** Which group a client lands in, on demand. Same treatment as the Rules
 * tab's match order: the sequence is the answer, so the popover is a list
 * and nothing else. */
function ClientMatchingPopover() {
  return (
    <Popover>
      <PopoverTrigger render={<Button type="button" size="sm" variant="outline" />}>
        <CircleHelp />
        Client matching
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 p-0">
        <p className="border-b border-border px-3 py-2.5 font-mono text-xs font-semibold tracking-widest text-muted-foreground uppercase">
          First match wins
        </p>
        <ol className="flex flex-col">
          {[
            { label: "exact IP", pinned: true },
            { label: "CIDR, longest prefix first", pinned: true },
            { label: "default", pinned: false },
          ].map((stage, i) => (
            <li
              key={stage.label}
              className={cn(
                "grid grid-cols-[22px_1fr] items-center gap-2.5 border-b border-border-muted px-3 py-1.5 last:border-b-0",
                stage.pinned && "bg-accent",
              )}
            >
              <span className="font-mono text-xs text-muted-foreground">{i + 1}.</span>
              <span
                className={cn(
                  "font-mono text-sm",
                  stage.pinned ? "font-semibold text-foreground" : "text-muted-foreground",
                )}
              >
                {stage.label}
              </span>
            </li>
          ))}
        </ol>
      </PopoverContent>
    </Popover>
  );
}

// --- groups ---------------------------------------------------------------

const nameSchema = requiredText("Name is required");

/** Rename only ever changes the name, and a group's is required and unique
 * (ui-contract §3.5) — unlike a list's, where blank has a meaning. */
const groupNameSchema = z.object({ name: nameSchema });

/**
 * Everything the add row owns, in one place.
 *
 * `listIds` is nullable rather than defaulting to an array: null means the
 * picker has not been touched, which the server reads as "every list". An
 * empty array is the different, deliberate answer of "none". Keeping that
 * distinction in the form state is what lets the submit decide whether to
 * send the field at all.
 */
const addGroupSchema = z.object({
  name: nameSchema,
  enabled: z.boolean(),
  listIds: z.array(z.number()).nullable(),
});
type AddGroupValues = z.infer<typeof addGroupSchema>;

function AddGroupRow({ allLists, onClose }: { allLists: List[]; onClose: () => void }) {
  const addGroup = useAddGroup();
  // All three fields live in the form, not beside it: they are one form,
  // and a reset or a schema change has to reach all of them.
  const form = useForm<AddGroupValues>({
    resolver: zodResolver(addGroupSchema),
    defaultValues: { name: "", enabled: true, listIds: null },
  });

  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  const listIds = useWatch({ control: form.control, name: "listIds" });
  // Untouched shows what the server would do: every list.
  const chosen = listIds ?? allLists.map((l) => l.id);

  function onSubmit(values: AddGroupValues) {
    addGroup.mutate(
      {
        name: values.name.trim(),
        enabled: values.enabled,
        // Sent only once touched. Leaving it off lets the server apply
        // "every list", which stays correct even if a list is added
        // between this page loading and the group being created.
        ...(values.listIds === null ? {} : { list_ids: values.listIds }),
      },
      {
        onSuccess: () => {
          toast.success("Group added");
          onClose();
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the group"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="add-group-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GROUP_GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Group name"
                    placeholder="Office"
                    autoComplete="off"
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="enabled"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    aria-label="Enabled"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="listIds"
            render={({ field }) => (
              <FormItem>
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={<Button type="button" variant="outline" size="sm" />}
                  >
                    {`Lists (${chosen.length})`}
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="start">
                    {allLists.length === 0 ? (
                      <DropdownMenuItem disabled>No filter lists yet</DropdownMenuItem>
                    ) : (
                      allLists.map((l) => (
                        <DropdownMenuCheckboxItem
                          key={l.id}
                          checked={chosen.includes(l.id)}
                          onCheckedChange={(checked) =>
                            field.onChange(
                              checked ? [...chosen, l.id] : chosen.filter((x) => x !== l.id),
                            )
                          }
                        >
                          {l.name}
                        </DropdownMenuCheckboxItem>
                      ))
                    )}
                  </DropdownMenuContent>
                </DropdownMenu>
              </FormItem>
            )}
          />
          <span className="text-right font-mono text-sm text-muted-foreground">0</span>
          <span className="text-sm text-muted-foreground">Active</span>
          <div className="flex items-center justify-end gap-1">
            <Button type="submit" size="sm" disabled={addGroup.isPending}>
              {addGroup.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the add-group row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>
      </form>
    </Form>
  );
}

function RenameGroupDialog({ group, onClose }: { group: Group | null; onClose: () => void }) {
  const renameGroup = useRenameGroup();

  return (
    <RenameDialog
      targetId={group?.id ?? null}
      title="Rename this group"
      initialName={group?.name ?? ""}
      schema={groupNameSchema}
      isPending={renameGroup.isPending}
      onClose={onClose}
      onSubmit={(name) => {
        if (!group) return;
        renameGroup.mutate(
          { id: group.id, name },
          {
            onSuccess: () => {
              toast.success("Group renamed");
              onClose();
            },
            onError: (err) =>
              toast.error(err instanceof ApiError ? err.message : "Couldn't rename the group"),
          },
        );
      }}
    />
  );
}

/**
 * The per-group list assignment.
 *
 * The failure case is the whole reason this has an error state. The toggle
 * builds the next assignment set from what this query returned; when the
 * query has *failed* that set is empty, so ticking one list used to PUT
 * `[thatOne]` and silently drop every other assignment the group had. The
 * trigger was disabled while pending but not while errored, so the click
 * was reachable. Now a failed read replaces the control with a retry, and
 * there is nothing to click that could write.
 */
function GroupListsCell({ group, allLists }: { group: Group; allLists: List[] }) {
  const groupLists = useGroupLists(group.id);
  const setGroupLists = useSetGroupLists();
  const assignedIds = useMemo(
    () => new Set(groupLists.data?.map((l) => l.id) ?? []),
    [groupLists.data],
  );

  if (groupLists.isError) {
    return (
      <Button
        type="button"
        size="sm"
        variant="outline"
        className="border-destructive text-destructive-foreground"
        onClick={() => void groupLists.refetch()}
        disabled={groupLists.isFetching}
      >
        <CircleAlert />
        {groupLists.isFetching ? "Retrying…" : "Can't load · retry"}
      </Button>
    );
  }

  function onToggle(listId: number, checked: boolean) {
    // Guarded as well as hidden: without data there is no safe "next set"
    // to compute, and sending one would be the wipe this exists to prevent.
    if (!groupLists.data) return;
    const current = groupLists.data.map((l) => l.id);
    const next = checked ? [...current, listId] : current.filter((id) => id !== listId);
    setGroupLists.mutate(
      { groupId: group.id, listIds: next },
      {
        // The checkbox is driven purely by server data, so nothing moves
        // until the invalidated groupLists query comes back — without this
        // the click reads as a no-op in the meantime.
        onSuccess: () =>
          toast.success(
            checked ? `List applied to ${group.name}` : `List removed from ${group.name}`,
          ),
        onError: () => toast.error(`Couldn't update lists for ${group.name}`),
      },
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={<Button type="button" variant="outline" size="sm" />}>
        {/* A single text node, not "Lists" + a sibling element — the ARIA
            name-from-content algorithm trims each child node's own
            contribution before concatenating with no separator, so splitting
            the count into a sibling <span> silently loses the space in the
            computed accessible name even though textContent looks right. */}
        {groupLists.isSuccess ? `Lists (${groupLists.data.length})` : "Lists"}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        {/* The label has to sit inside a group — base-ui's MenuGroupLabel
            reads a context only Menu.Group provides, and throws without it. */}
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
              // `isFetching` as well as `isPending`: the next set is derived
              // from this query's data, which is stale from the moment a
              // write lands until its refetch does. The mutation happens to
              // stay pending across that window today (its onSuccess returns
              // the invalidation's promise), but the set this control writes
              // must be safe on its own terms, not on the hook's.
              disabled={setGroupLists.isPending || groupLists.isFetching || !groupLists.data}
              onCheckedChange={(checked) => onToggle(list.id, checked)}
            >
              {/* The name, not the URL: a column of 90-character
                  raw.githubusercontent.com paths is unreadable, and every
                  hagezi entry looked identical. */}
              {list.name}
            </DropdownMenuCheckboxItem>
          ))
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function GroupRow({
  group,
  allLists,
  clientCount,
  selected,
  onSelect,
  onRename,
}: {
  group: Group;
  allLists: List[];
  clientCount: number;
  selected: boolean;
  onSelect: () => void;
  onRename: () => void;
}) {
  const toggleGroup = useToggleGroup();
  const deleteGroup = useDeleteGroup();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const isDefault = group.id === DEFAULT_GROUP_ID;
  // The server refuses the delete while any client still points at the group
  // (internal/store/crud.go's DeleteGroup returns ErrInUse → 409), so the
  // row says so up front instead of offering a confirm that can only fail.
  const inUse = clientCount > 0;

  return (
    <div
      data-slot="group-row"
      className={cn(
        "border-b border-border-muted",
        // A disabled group compiles no ruleset at all, so nothing is blocked
        // for any of its clients. Treated like a failed list rather than a
        // dimmed row: it is a state someone needs to notice, not a shade.
        !group.enabled && "bg-destructive/5 shadow-[inset_3px_0_0_var(--destructive)]",
      )}
    >
      <div className={cn(GROUP_GRID, "py-2.5")}>
        <button
          type="button"
          onClick={onSelect}
          aria-pressed={selected}
          className="flex min-w-0 items-center gap-2.5 text-left"
        >
          <span
            aria-hidden
            className={cn("size-1.5 shrink-0", selected ? "bg-primary" : "bg-transparent")}
          />
          <span className={cn("truncate text-sm", selected ? "font-semibold" : "font-medium")}>
            {group.name}
          </span>
        </button>
        <span>
          <Switch
            checked={group.enabled}
            disabled={toggleGroup.isPending}
            onCheckedChange={(enabled) =>
              toggleGroup.mutate(
                { id: group.id, enabled },
                {
                  onError: () =>
                    toast.error(`Couldn't ${enabled ? "enable" : "disable"} ${group.name}`),
                },
              )
            }
            aria-label={`${group.enabled ? "Disable" : "Enable"} ${group.name}`}
          />
        </span>
        <span>
          <GroupListsCell group={group} allLists={allLists} />
        </span>
        <span
          className={cn(
            "text-right font-mono text-sm",
            clientCount === 0 && "text-muted-foreground",
          )}
        >
          {clientCount}
        </span>
        <span>
          <PauseControl groupId={group.id} />
        </span>
        {/* Both icons. One icon beside one text button read as two
            different kinds of thing when they are the same kind of thing. */}
        <div className="flex items-center justify-end gap-1">
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            aria-label={`Rename ${group.name}`}
            onClick={onRename}
          >
            <Pencil />
          </Button>
          <Button
            type="button"
            size="icon-sm"
            variant="ghost"
            disabled={isDefault || inUse}
            aria-label={`Delete ${group.name}`}
            onClick={() => setDeleteOpen(true)}
          >
            <Trash2 />
          </Button>
        </div>
      </div>

      {isDefault && (
        <p className="px-4 pb-2.5 pl-8 text-xs text-muted-foreground">
          The fallback group can&apos;t be deleted.
        </p>
      )}

      {!isDefault && inUse && (
        <p className="px-4 pb-2.5 pl-8 text-xs text-muted-foreground">
          Move its {clientCount} {clientCount === 1 ? "client" : "clients"} first to delete it.
        </p>
      )}

      {!group.enabled && (
        <p className="flex items-center gap-2 px-4 pb-2.5 pl-8 text-xs text-destructive-foreground">
          <span className="font-mono font-semibold tracking-widest uppercase">Not filtering</span>
          {/* The default group governs every device that matched no client
              row (internal/clients/registry.go's Lookup), so counting only
              the clients pinned to it under-reports what has stopped being
              filtered — usually as "its 0 clients". */}
          <span className="text-pretty">
            {isDefault
              ? "Nothing is blocked for any device not pinned to another group."
              : `Nothing is blocked for its ${clientCount} ${clientCount === 1 ? "client" : "clients"}.`}
          </span>
        </p>
      )}

      <ConfirmDeleteDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={`Delete ${group.name}?`}
        description="Its rules and list assignments go with it."
        isPending={deleteGroup.isPending}
        onConfirm={() =>
          deleteGroup.mutate(group.id, {
            onSuccess: () => {
              toast.success(`${group.name} deleted`);
              setDeleteOpen(false);
            },
            onError: (err) =>
              toast.error(
                friendlyDeleteError(err, `Can't delete ${group.name} — it's still in use`),
              ),
          })
        }
      />
    </div>
  );
}

// --- clients --------------------------------------------------------------

const matcherSchema = z
  .string()
  .trim()
  .superRefine((value, ctx) => {
    const fail = (message: string) => ctx.addIssue({ code: "custom", message });
    if (!value) return fail("Matcher is required");
    const parts = value.split("/");
    if (parts.length > 2) {
      return fail("Enter a single IP address or CIDR range, e.g. 192.168.1.0/24");
    }
    const [addr, prefix] = parts;
    const isV4 = isValidIPv4(addr);
    const isV6 = !isV4 && isValidIPv6(addr, { allowZone: true });
    if (!isV4 && !isV6) {
      return fail("Enter a valid IP address or CIDR range, e.g. 192.168.1.10 or 192.168.1.0/24");
    }
    if (prefix === undefined) return;
    if (isV6 && addr.includes("%")) {
      return fail("Zone IDs can't be combined with a CIDR range — drop the /prefix, or the %zone");
    }
    if (!/^\d+$/.test(prefix)) return fail("CIDR prefix must be a number");
    // netip.ParsePrefix rejects any multi-character prefix not starting
    // 1-9, so "/024" and "/+4" are errors there even though they'd parse
    // as 24 and 4 by themselves. "/0" alone is fine.
    if (prefix.length > 1 && !/^[1-9]/.test(prefix)) {
      return fail("CIDR prefix can't have a leading zero — write /24, not /024");
    }
    const max = isV4 ? 32 : 128;
    if (Number(prefix) > max) return fail(`CIDR prefix must be between 0 and ${max}`);
  });

// Optional, matching the server (internal/store's Client.Name is not
// validated and may be empty). The old form required one, which invented a
// constraint the API does not have.
const clientFormSchema = z.object({
  name: z.string(),
  matcher: matcherSchema,
  groupId: z.string(),
});
type ClientFormValues = z.infer<typeof clientFormSchema>;

function AddClientRow({
  groups,
  defaultGroupId,
  onClose,
}: {
  groups: Group[];
  defaultGroupId: number;
  onClose: () => void;
}) {
  const addClient = useAddClient();
  const form = useForm<ClientFormValues>({
    resolver: zodResolver(clientFormSchema),
    defaultValues: { name: "", matcher: "", groupId: String(defaultGroupId) },
  });

  function onSubmit(values: ClientFormValues) {
    addClient.mutate(
      {
        name: values.name.trim(),
        matcher: values.matcher.trim(),
        group_id: Number(values.groupId),
      },
      {
        onSuccess: () => {
          toast.success("Client added");
          form.reset({ name: "", matcher: "", groupId: values.groupId });
          form.setFocus("name");
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the client"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="add-client-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(CLIENT_GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Client name"
                    placeholder="Living room TV — optional"
                    autoComplete="off"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="matcher"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Matcher"
                    placeholder="192.168.150.10 or 192.168.150.64/27"
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="groupId"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <NativeSelect {...field} aria-label="Client group">
                    {groups.map((g) => (
                      <NativeSelectOption key={g.id} value={String(g.id)}>
                        {g.name}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={addClient.isPending}>
              {addClient.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the add-client row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>
        <div className={cn(CLIENT_GRID, "items-start pb-2.5")}>
          <span />
          <span className="text-xs text-pretty text-muted-foreground">
            <FormField control={form.control} name="matcher" render={() => <FormMessage />} />
          </span>
          <span />
          <span />
        </div>
      </form>
    </Form>
  );
}

/** The default group is the one every unpinned device lands in, so its badge
 * is the quiet one; a deliberate assignment is the thing worth seeing. */
function groupBadgeVariant(groupId: number): NonNullable<BadgeProps["variant"]> {
  return groupId === DEFAULT_GROUP_ID ? "secondary" : "primary-light";
}

// --- page -----------------------------------------------------------------

/**
 * Filtering › Groups & Clients. A group is a policy bucket; a client is a
 * device pinned to exactly one of them by IP or CIDR.
 */
export function GroupsClientsTab() {
  const groups = useGroups();
  const clients = useClients();
  const lists = useLists();
  const deleteClient = useDeleteClient();

  const [selectedGroup, setSelectedGroup] = useState<number | null>(null);
  const [addGroupOpen, setAddGroupOpen] = useState(false);
  const [addClientOpen, setAddClientOpen] = useState(false);
  const [renameTarget, setRenameTarget] = useState<Group | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Client | null>(null);

  const allGroups = useMemo(() => groups.data ?? [], [groups.data]);
  const allClients = useMemo(() => clients.data ?? [], [clients.data]);
  const allLists = useMemo(() => lists.data ?? [], [lists.data]);

  const clientsByGroup = useMemo(() => {
    const counts = new Map<number, number>();
    for (const c of allClients) counts.set(c.group_id, (counts.get(c.group_id) ?? 0) + 1);
    return counts;
  }, [allClients]);

  const shownClients = useMemo(
    () =>
      selectedGroup === null ? allClients : allClients.filter((c) => c.group_id === selectedGroup),
    [allClients, selectedGroup],
  );

  const selectedName = allGroups.find((g) => g.id === selectedGroup)?.name;
  const disabledCount = allGroups.filter((g) => !g.enabled).length;

  let groupsBody: ReactNode;
  if (groups.isPending) {
    groupsBody = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (groups.data === undefined) {
    groupsBody = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load groups</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else {
    groupsBody = allGroups.map((g) => (
      <GroupRow
        key={g.id}
        group={g}
        allLists={allLists}
        clientCount={clientsByGroup.get(g.id) ?? 0}
        selected={selectedGroup === g.id}
        onSelect={() => setSelectedGroup(selectedGroup === g.id ? null : g.id)}
        onRename={() => setRenameTarget(g)}
      />
    ));
  }

  let clientsBody: ReactNode;
  if (clients.isPending) {
    clientsBody = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (clients.data === undefined) {
    clientsBody = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load clients</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (shownClients.length === 0) {
    clientsBody = (
      <div className="flex flex-col items-center gap-2 p-8 text-center">
        <p className="font-heading text-sm font-semibold">
          {selectedName ? `No clients in ${selectedName}` : "No clients yet"}
        </p>
        <p className="text-sm text-muted-foreground">Unpinned devices fall back to default.</p>
      </div>
    );
  } else {
    clientsBody = shownClients.map((client) => {
      const [addr, prefix] = client.matcher.split("/");
      return (
        <div
          key={client.id}
          data-slot="client-row"
          className={cn(CLIENT_GRID, "border-b border-border-muted py-1.5")}
        >
          <span
            className={cn(
              "truncate text-sm",
              client.name ? "font-medium" : "text-muted-foreground italic",
            )}
          >
            {client.name || "Unnamed device"}
          </span>
          <span className="truncate font-mono text-sm" title={client.matcher}>
            {addr}
            {prefix !== undefined && <span className="text-muted-foreground">/{prefix}</span>}
          </span>
          <span>
            <Badge variant={groupBadgeVariant(client.group_id)}>
              {allGroups.find((g) => g.id === client.group_id)?.name ?? "unknown"}
            </Badge>
          </span>
          <span className="flex items-center justify-end">
            <Button
              type="button"
              size="sm"
              variant="ghost"
              aria-label={`Delete ${client.name || client.matcher}`}
              onClick={() => setDeleteTarget(client)}
            >
              Delete
            </Button>
          </span>
        </div>
      );
    });
  }

  return (
    // Both grids are fixed-pixel column templates (~680px of them), and the
    // shell around this is h-screen/overflow-hidden — so on a narrow
    // viewport the right-hand columns were clipped with no way to reach
    // them. The header rows and the rows themselves have to scroll together,
    // which is why this sits on the column rather than on each body.
    <div data-slot="h-scroll" className="flex h-full min-h-0 flex-col overflow-x-auto">
      {(groups.isError && groups.data !== undefined) ||
      (clients.isError && clients.data !== undefined) ? (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="groups and clients"
            onRetry={() => {
              void groups.refetch();
              void clients.refetch();
            }}
            isRetrying={groups.isFetching || clients.isFetching}
          />
        </div>
      ) : null}

      {/* ---- groups ---------------------------------------------------- */}
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2">
        <h2 className="font-heading text-sm font-semibold">Groups</h2>
        {disabledCount > 0 && (
          <span className="font-mono text-xs text-muted-foreground">{disabledCount} disabled</span>
        )}
        <div className="ml-auto flex items-center gap-2">
          <ClientMatchingPopover />
          {!addGroupOpen && (
            <Button type="button" size="sm" variant="outline" onClick={() => setAddGroupOpen(true)}>
              <Plus />
              Add group
            </Button>
          )}
        </div>
      </div>

      <div
        className={cn(
          GROUP_GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-xs tracking-widest text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Enabled</span>
        <span>Lists</span>
        <span className="text-right">Clients</span>
        <span>Blocking</span>
        <span className="text-right">Actions</span>
      </div>

      {addGroupOpen && <AddGroupRow allLists={allLists} onClose={() => setAddGroupOpen(false)} />}

      <div className="shrink-0">{groupsBody}</div>

      {/* ---- clients --------------------------------------------------- */}
      <div className="flex shrink-0 items-center gap-2.5 border-y border-border bg-card px-4 py-2">
        <h2 className="font-heading text-sm font-semibold">Clients</h2>
        <span className="font-mono text-xs text-muted-foreground">
          {selectedName ? `in ${selectedName}` : "all groups"} · {shownClients.length}
        </span>
        {selectedGroup !== null && (
          <Button type="button" size="sm" variant="link" onClick={() => setSelectedGroup(null)}>
            Show all
          </Button>
        )}
        {!addClientOpen && allGroups.length > 0 && (
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto"
            onClick={() => setAddClientOpen(true)}
          >
            <Plus />
            Add client
          </Button>
        )}
      </div>

      <div
        className={cn(
          CLIENT_GRID,
          "shrink-0 border-b border-border py-2",
          "font-mono text-xs tracking-widest text-muted-foreground uppercase",
        )}
      >
        <span>Name</span>
        <span>Matcher</span>
        <span>Group</span>
        <span className="text-right">Actions</span>
      </div>

      {addClientOpen && allGroups.length > 0 && (
        <AddClientRow
          groups={allGroups}
          defaultGroupId={selectedGroup ?? allGroups[0]?.id ?? DEFAULT_GROUP_ID}
          onClose={() => setAddClientOpen(false)}
        />
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">{clientsBody}</div>

      <RenameGroupDialog group={renameTarget} onClose={() => setRenameTarget(null)} />

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title="Delete this client?"
        description={
          deleteTarget && (
            <>
              <code className="font-mono">{deleteTarget.matcher}</code> falls back to the default
              group.
            </>
          )
        }
        isPending={deleteClient.isPending}
        onConfirm={() => {
          if (!deleteTarget) return;
          const target = deleteTarget;
          deleteClient.mutate(target.id, {
            onSuccess: () => {
              toast.success("Client deleted");
              setDeleteTarget(null);
            },
            onError: () => toast.error(`Couldn't delete ${target.matcher}`),
          });
        }}
      />
    </div>
  );
}
