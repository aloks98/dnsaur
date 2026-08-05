import { useEffect, useState, type ReactNode } from "react";
import { Plus, Regex, ShieldBan, ShieldCheck, Trash2, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { useForm, useWatch } from "react-hook-form";
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
import type { Rule } from "../../api/types";
import { useAddRule, useDeleteRule, useRules } from "../../hooks/use-filters";
import { useGroups } from "../../hooks/use-groups";

// The server compiles a regex rule's pattern with Go's regexp package (see
// internal/api/filters_handlers.go's handleRuleCreate); `new RegExp` isn't a
// perfect stand-in for RE2 syntax, but it catches the vast majority of
// typos — unbalanced groups, bad escapes — before a round trip. The 512
// cap mirrors that same handler's own length check exactly.
const MAX_REGEX_PATTERN_LENGTH = 512;

// The default group (id 1, seeded and undeletable — see use-groups.ts) is a
// safe first selection: it always exists, so the Select never opens on an
// empty choice while groups are still loading.
const DEFAULT_GROUP_ID = 1;

// Same red/green thread as the Lists tab's block/allow badges (see
// lists.tsx's KIND_META) — a rule's action reads the same way wherever it
// shows up, even though this is a distinct vocabulary (rule action, not
// list kind or query decision).
const ACTION_META: Record<
  Rule["action"],
  { label: string; variant: NonNullable<BadgeProps["variant"]>; icon: typeof ShieldBan }
> = {
  block: { label: "Block", variant: "destructive-light", icon: ShieldBan },
  allow: { label: "Allow", variant: "success-light", icon: ShieldCheck },
};

// `items` maps each value to its display label — without it, SelectValue
// renders the raw stored value ("block") instead of the option's label
// (Task 12's finding; see settings.tsx/account.tsx for the same fix).
const RULE_ACTION_ITEMS: Record<Rule["action"], string> = {
  block: ACTION_META.block.label,
  allow: ACTION_META.allow.label,
};

function validatePattern(value: string, isRegex: boolean): string | true {
  if (!value.trim()) return "Pattern is required";
  if (!isRegex) return true;
  if (value.length > MAX_REGEX_PATTERN_LENGTH) {
    return `Regex pattern must be ${MAX_REGEX_PATTERN_LENGTH} characters or fewer`;
  }
  try {
    // eslint-disable-next-line no-new -- constructed only to validate syntax
    new RegExp(value);
  } catch (err) {
    return `Invalid regular expression: ${err instanceof Error ? err.message : "syntax error"}`;
  }
  return true;
}

interface AddRuleValues {
  action: Rule["action"];
  pattern: string;
  is_regex: boolean;
}

const ADD_RULE_DEFAULTS: AddRuleValues = { action: "block", pattern: "", is_regex: false };

function AddRuleDialog({ groupId, groupName }: { groupId: number; groupName: string }) {
  const [open, setOpen] = useState(false);
  const addRule = useAddRule();
  const form = useForm<AddRuleValues>({ defaultValues: ADD_RULE_DEFAULTS });
  const isRegex = useWatch({ control: form.control, name: "is_regex" });

  function onOpenChange(next: boolean) {
    setOpen(next);
    if (!next) form.reset(ADD_RULE_DEFAULTS);
  }

  function onSubmit(values: AddRuleValues) {
    addRule.mutate(
      { groupId, action: values.action, pattern: values.pattern.trim(), is_regex: values.is_regex },
      {
        onSuccess: () => {
          toast.success("Rule added");
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the rule"),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger render={<Button type="button" size="sm" />}>
        <Plus />
        Add rule
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add a rule</DialogTitle>
          <DialogDescription>
            Scoped to {groupName} — matched against every query name.
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
              name="action"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Action</FormLabel>
                  <Select
                    items={RULE_ACTION_ITEMS}
                    value={field.value}
                    onValueChange={field.onChange}
                  >
                    <SelectTrigger aria-label="Action">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="block">Block</SelectItem>
                      <SelectItem value="allow">Allow</SelectItem>
                    </SelectContent>
                  </Select>
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="pattern"
              rules={{
                validate: (value: string, formValues) =>
                  validatePattern(value, formValues.is_regex),
              }}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Pattern</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      placeholder={isRegex ? "^ads\\..*\\.example\\.com$" : "ads.example.com"}
                      autoComplete="off"
                      className="font-mono"
                    />
                  </FormControl>
                  <FormDescription>
                    {isRegex
                      ? "Compiled as a regular expression and matched against the query name."
                      : "Matched exactly against the query name."}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name="is_regex"
              render={({ field }) => (
                <FormItem className="flex flex-row items-center justify-between gap-4 rounded-md border border-border px-3 py-2.5">
                  <div>
                    <FormLabel>Regular expression</FormLabel>
                    <FormDescription>
                      Off matches the pattern exactly, no wildcards.
                    </FormDescription>
                  </div>
                  <FormControl>
                    {/* No explicit aria-label needed — FormField wires this
                        control's aria-labelledby to the FormLabel below,
                        and that takes precedence over aria-label anyway. */}
                    <Switch checked={field.value} onCheckedChange={field.onChange} />
                  </FormControl>
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={addRule.isPending}>
                {addRule.isPending ? "Adding…" : "Add rule"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

function RulesTable({
  rules,
  onDeleteRequest,
}: {
  rules: Rule[];
  onDeleteRequest: (rule: Rule) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Action</TableHead>
          <TableHead>Pattern</TableHead>
          <TableHead>Match</TableHead>
          <TableHead className="text-right">
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rules.map((rule) => {
          const meta = ACTION_META[rule.action];
          const Icon = meta.icon;
          return (
            <TableRow key={rule.id}>
              <TableCell>
                <Badge variant={meta.variant}>
                  <Icon />
                  {meta.label}
                </Badge>
              </TableCell>
              <TableCell className="max-w-96 truncate font-mono text-xs" title={rule.pattern}>
                {rule.pattern}
              </TableCell>
              <TableCell>
                {rule.is_regex ? (
                  <Badge variant="outline">
                    <Regex />
                    Regex
                  </Badge>
                ) : (
                  <Badge variant="secondary">Exact</Badge>
                )}
              </TableCell>
              <TableCell className="text-right">
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Delete rule ${rule.pattern}`}
                  onClick={() => onDeleteRequest(rule)}
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
 * A *background* refetch failed while data from an earlier successful fetch
 * is still in hand. Adding or deleting a rule invalidates this group's rules
 * query, which refetches immediately; query-core flips `status` to "error"
 * if that refetch fails, even though `data` is intact — so gating the
 * destructive "couldn't load" Alert on `isError` alone would swap a
 * populated, still correct table for an error card right after a successful
 * add or delete (refetchOnReconnect, on by default, is a second trigger).
 * The destructive Alert is reserved for `isError && data === undefined` —
 * genuinely nothing to show — and this quiet banner covers the rest.
 */
function StaleDataAlert({ onRetry, isRetrying }: { onRetry: () => void; isRetrying: boolean }) {
  return (
    <Alert variant="warning">
      <TriangleAlert />
      <AlertTitle>Couldn&apos;t refresh rules</AlertTitle>
      <AlertDescription>
        <p>Showing what last loaded successfully.</p>
        <Button type="button" variant="outline" size="sm" onClick={onRetry} disabled={isRetrying}>
          {isRetrying ? "Retrying…" : "Try again"}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

/**
 * Filtering › Rules — per-domain and regex allow/block rules, scoped to one
 * group at a time via the Select at the top. Consumes use-filters.ts's
 * useRules/useAddRule/useDeleteRule (Task 9) rather than redefining them;
 * this tab is what those hooks' doc comments already point to.
 */
export function RulesTab() {
  const groups = useGroups();
  const [groupId, setGroupId] = useState<number>(DEFAULT_GROUP_ID);

  // Fall back to whatever group actually exists once groups load, in the
  // unlikely case the seeded default (id 1) isn't among them.
  useEffect(() => {
    if (groups.data && groups.data.length > 0 && !groups.data.some((g) => g.id === groupId)) {
      setGroupId(groups.data[0].id);
    }
  }, [groups.data, groupId]);

  const rules = useRules(groupId);
  const deleteRule = useDeleteRule();
  const [deleteTarget, setDeleteTarget] = useState<Rule | null>(null);

  const currentGroup = groups.data?.find((g) => g.id === groupId);
  const groupName = currentGroup?.name ?? "this group";

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteRule.mutate(
      { id: target.id, groupId },
      {
        onSuccess: () => {
          toast.success("Rule deleted");
          setDeleteTarget(null);
        },
        onError: () => toast.error(`Couldn't delete the rule for ${target.pattern}`),
      },
    );
  }

  const isEmpty = rules.data?.length === 0;

  let body: ReactNode;
  if (rules.isPending) {
    body = (
      <div className="flex flex-col gap-2" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
        ))}
      </div>
    );
  } else if (rules.data === undefined) {
    body = (
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Couldn&apos;t load rules</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  } else if (isEmpty) {
    body = (
      <EmptyState
        icon={<Regex />}
        title="No rules for this group yet"
        description={`Add a block or allow rule scoped to ${groupName}.`}
        action={<AddRuleDialog groupId={groupId} groupName={groupName} />}
      />
    );
  } else {
    body = <RulesTable rules={rules.data} onDeleteRequest={setDeleteTarget} />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="flex flex-col gap-1.5">
          <span className="text-sm font-medium text-foreground" id="rules-group-label">
            Group
          </span>
          <Select
            // `items` maps each value to its display label — without it,
            // SelectValue renders the raw group id ("1") instead of the
            // group's name (Task 12's finding; see settings.tsx/account.tsx
            // for the same fix).
            items={Object.fromEntries((groups.data ?? []).map((g) => [String(g.id), g.name]))}
            value={String(groupId)}
            onValueChange={(v) => setGroupId(Number(v))}
            disabled={groups.isPending || !groups.data || groups.data.length === 0}
          >
            <SelectTrigger aria-labelledby="rules-group-label" className="w-56">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {groups.data?.map((g) => (
                <SelectItem key={g.id} value={String(g.id)}>
                  {g.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {!isEmpty && <AddRuleDialog groupId={groupId} groupName={groupName} />}
      </div>

      {rules.isError && rules.data !== undefined && (
        <StaleDataAlert onRetry={() => void rules.refetch()} isRetrying={rules.isFetching} />
      )}

      {body}

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this rule?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  The {ACTION_META[deleteTarget.action].label.toLowerCase()} rule for{" "}
                  <code className="font-mono break-all text-foreground">
                    {deleteTarget.pattern}
                  </code>{" "}
                  will stop applying to {groupName}.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteRule.isPending}
            >
              {deleteRule.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
