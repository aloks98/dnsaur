import { useEffect, useMemo, useState, type ReactNode } from "react";
import { CircleHelp, Plus, TriangleAlert, X } from "lucide-react";
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
import type { Rule } from "../../api/types";
import { useAddRule, useDeleteRule, useRules } from "../../hooks/use-filters";
import { useGroups } from "../../hooks/use-groups";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { DEFAULT_GROUP_ID } from "../../lib/query-rows";
import { ConfirmDeleteDialog } from "../dialogs";

// The server compiles a regex rule's pattern with Go's regexp package (see
// internal/api/filters_handlers.go's handleRuleCreate); `new RegExp` isn't a
// perfect stand-in for RE2 syntax, but it catches the vast majority of
// typos — unbalanced groups, bad escapes — before a round trip. The 512
// cap mirrors that same handler's own length check exactly.
const MAX_REGEX_PATTERN_LENGTH = 512;

/**
 * The pattern rewritten into V8's spelling of the constructs Go's regexp has
 * and V8 does not — used only to let `new RegExp` judge the *syntax*. What
 * gets sent is always the pattern as typed.
 *
 * Two of them, both verified against both engines: inline flag settings
 * (`(?i)ads`, `(?is)^ads.*`), which V8 has no equivalent for at all, and
 * `(?P<name>…)`, which V8 spells `(?<name>…)`. Without this the check
 * rejected patterns the resolver compiles happily — the exact failure
 * lib/schemas.ts warns against, since a false reject silently blocks a
 * legitimate value while a false accept costs one 400 toast.
 *
 * `(?i:…)` needs nothing: V8 has modifier groups.
 */
function inV8Spelling(pattern: string): string {
  return pattern
    .replaceAll(/\(\?[imsU-]+\)/g, "")
    .replaceAll(/\(\?[imsU-]+:/g, "(?:")
    .replaceAll("(?P<", "(?<");
}

/** Same red/green thread as the Lists tab's block/allow badges — an action
 * reads the same way wherever it appears, even though this is a distinct
 * vocabulary (rule action, not list kind or query decision). */
const ACTION_VARIANT: Record<Rule["action"], NonNullable<BadgeProps["variant"]>> = {
  block: "destructive-light",
  allow: "primary-light",
};

/**
 * Evaluation order, which is the thing people get wrong about this screen.
 *
 * Allow beats block, literal beats regex, and every rule beats every list.
 * The four rule stages are emphasised and the two list stages are not,
 * because the point being made is where *these* rules sit relative to the
 * lists on the neighbouring tab — not that six stages exist.
 */
const EVAL_ORDER = [
  { label: "literal allow", rule: true },
  { label: "regex allow", rule: true },
  { label: "literal block", rule: true },
  { label: "regex block", rule: true },
  { label: "allowlists", rule: false },
  { label: "blocklists", rule: false },
] as const;

/**
 * The order, on demand.
 *
 * It was a permanent band across the page, which spent a full row of
 * vertical space on something you need once and then know. Behind a
 * control it can also afford to be numbered and to explain itself, which
 * the inline strip could not without becoming a paragraph.
 */
function MatchOrderPopover() {
  return (
    <Popover>
      <PopoverTrigger render={<Button type="button" size="sm" variant="outline" />}>
        <CircleHelp />
        Match order
      </PopoverTrigger>
      <PopoverContent align="end" className="w-88 p-0">
        <p className="border-b border-border px-3 py-2.5 font-mono text-xs font-semibold tracking-widest text-muted-foreground uppercase">
          First match wins
        </p>
        <ol className="flex flex-col">
          {EVAL_ORDER.map((stage, i) => (
            <li
              key={stage.label}
              className={cn(
                "grid grid-cols-[22px_1fr] items-center gap-2.5 border-b border-border-muted px-3 py-1.5",
                stage.rule && "bg-accent",
              )}
            >
              <span className="font-mono text-xs text-muted-foreground">{i + 1}.</span>
              <span
                className={cn(
                  "font-mono text-sm",
                  stage.rule ? "font-semibold text-foreground" : "text-muted-foreground",
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

/** The pattern's rules depend on the regex switch beside it, so the check
 * lives at the object level (with an explicit `path`) where the refinement
 * can see both fields at once.
 *
 * Neither the length cap nor the compile check runs against a trimmed
 * pattern, deliberately: leading/trailing whitespace is significant inside
 * a regex, and the server measures and compiles the pattern it was sent. */
const addRuleSchema = z
  .object({
    action: z.enum(["block", "allow"]),
    pattern: z.string(),
    is_regex: z.boolean(),
  })
  .superRefine(({ pattern, is_regex }, ctx) => {
    const fail = (message: string) => ctx.addIssue({ code: "custom", message, path: ["pattern"] });
    if (!pattern.trim()) return fail("Pattern is required");
    if (!is_regex) return;
    if (pattern.length > MAX_REGEX_PATTERN_LENGTH) {
      return fail(`Regex pattern must be ${MAX_REGEX_PATTERN_LENGTH} characters or fewer`);
    }
    try {
      // eslint-disable-next-line no-new -- constructed only to validate syntax
      new RegExp(inV8Spelling(pattern));
    } catch (err) {
      fail(`Invalid regular expression: ${err instanceof Error ? err.message : "syntax error"}`);
    }
  });

type AddRuleValues = z.infer<typeof addRuleSchema>;
const ADD_RULE_DEFAULTS: AddRuleValues = { action: "block", pattern: "", is_regex: false };

/** Header and data rows. The form row below needs a wider third column for
 * its switch and label, so it declares its own — but the other three tracks
 * match, which is what keeps the form visually aligned with the table. */
const GRID = "grid grid-cols-[104px_1fr_116px_96px] items-center gap-4 px-4";
const FORM_GRID = "grid grid-cols-[104px_1fr_220px_96px] items-center gap-4 px-4";

/**
 * The add row: one band under the header rather than a dialog, so a rule is
 * written on the same line the existing ones are read on.
 */
function AddRuleRow({ groupId, onClose }: { groupId: number; onClose: () => void }) {
  const addRule = useAddRule();
  const form = useForm<AddRuleValues>({
    resolver: zodResolver(addRuleSchema),
    defaultValues: ADD_RULE_DEFAULTS,
  });
  const isRegex = useWatch({ control: form.control, name: "is_regex" });

  useEffect(() => {
    form.setFocus("pattern");
  }, [form]);

  function onSubmit(values: AddRuleValues) {
    addRule.mutate(
      { groupId, action: values.action, pattern: values.pattern.trim(), is_regex: values.is_regex },
      {
        onSuccess: () => {
          toast.success("Rule added");
          // Stays open, cleared: adding one rule is usually adding several,
          // and the action/regex choice is normally the same for each — so
          // only the pattern resets.
          form.reset({ ...form.getValues(), pattern: "" });
          form.setFocus("pattern");
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the rule"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(FORM_GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="action"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <NativeSelect {...field} aria-label="Rule action">
                    <NativeSelectOption value="block">block</NativeSelectOption>
                    <NativeSelectOption value="allow">allow</NativeSelectOption>
                  </NativeSelect>
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="pattern"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Pattern"
                    placeholder={isRegex ? "^ads?[0-9]*\\." : "doubleclick.net"}
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="is_regex"
            render={({ field }) => (
              <FormItem className="flex flex-row items-center gap-2.5">
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={(next) => {
                      field.onChange(next);
                      // The pattern's rules change with this switch, so a
                      // shown error has to be re-judged against the new mode
                      // rather than waiting for the next submit.
                      if (form.getFieldState("pattern").invalid) void form.trigger("pattern");
                    }}
                    aria-label="Regular expression"
                  />
                </FormControl>
                <span
                  className={cn(
                    "font-mono text-xs",
                    field.value ? "text-foreground" : "text-muted-foreground",
                  )}
                >
                  regular expression
                </span>
              </FormItem>
            )}
          />
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={addRule.isPending}>
              {addRule.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the add-rule row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>

        {/* Under the pattern field, because it describes what that field
            will accept — and it changes with the switch. */}
        <div className={cn(FORM_GRID, "items-start pb-2.5")}>
          <span />
          <span className="text-xs text-pretty text-muted-foreground">
            <FormField
              control={form.control}
              name="pattern"
              render={() =>
                form.formState.errors.pattern ? (
                  <FormMessage />
                ) : (
                  <FormItem>
                    {isRegex
                      ? "RE2 syntax, unanchored, matched against the lowercased query name. Max 512 bytes."
                      : "Matches this domain and every subdomain, on label boundaries. No length limit."}
                  </FormItem>
                )
              }
            />
          </span>
          <span />
          <span />
        </div>
      </form>
    </Form>
  );
}

/**
 * Filtering › Rules — per-domain and regex allow/block rules, scoped to one
 * group at a time. A rule belongs to exactly one group; there are no global
 * rules, which is why the group selector is the first control on the page
 * rather than a filter among others.
 */
export function RulesTab() {
  const groups = useGroups();
  const [selectedId, setSelectedId] = useState<number>(DEFAULT_GROUP_ID);

  // Fall back to whatever group actually exists once groups load, in the
  // unlikely case the seeded default (id 1) isn't among them. Derived here
  // rather than corrected by an effect: the effect let one render go out
  // naming a group that isn't there — and useRules fetch its rules — before
  // putting it right.
  const known = groups.data;
  const groupId =
    known && known.length > 0 && !known.some((g) => g.id === selectedId) ? known[0].id : selectedId;

  const rules = useRules(groupId);
  const deleteRule = useDeleteRule();
  const [deleteTarget, setDeleteTarget] = useState<Rule | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [actionFilter, setActionFilter] = useState<"" | Rule["action"]>("");

  const groupName = groups.data?.find((g) => g.id === groupId)?.name ?? "this group";
  const all = useMemo(() => rules.data ?? [], [rules.data]);

  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return all.filter(
      (r) =>
        (actionFilter === "" || r.action === actionFilter) &&
        (q === "" || r.pattern.toLowerCase().includes(q)),
    );
  }, [all, search, actionFilter]);

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

  let body: ReactNode;
  if (rules.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (rules.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load rules</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (all.length === 0) {
    body = (
      <div className="flex flex-col items-center gap-3 p-10 text-center">
        <p className="font-heading text-base font-semibold">No rules in {groupName}</p>
        {/* The one thing empty does not mean here: that the group filters
            nothing. */}
        <p className="text-sm text-muted-foreground">Lists still apply.</p>
        {!addOpen && (
          <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
            <Plus />
            Add rule
          </Button>
        )}
      </div>
    );
  } else if (shown.length === 0) {
    body = (
      <p className="p-10 text-center text-sm text-muted-foreground">
        No rules match this filter.{" "}
        <Button
          type="button"
          variant="link"
          size="sm"
          onClick={() => {
            setSearch("");
            setActionFilter("");
          }}
        >
          Clear it
        </Button>
      </p>
    );
  } else {
    body = shown.map((rule) => (
      <div
        key={rule.id}
        data-slot="rule-row"
        className={cn(GRID, "border-b border-border-muted py-2")}
      >
        <span>
          <Badge variant={ACTION_VARIANT[rule.action]}>{rule.action}</Badge>
        </span>
        <span className="truncate font-mono text-sm" title={rule.pattern}>
          {rule.pattern}
        </span>
        <span>
          {/* Warning-toned for regex, not because it is worse, but because
              it is the one that can match far more than it looks like it
              will. */}
          <Badge variant={rule.is_regex ? "warning-outline" : "outline"}>
            {rule.is_regex ? "regex" : "literal"}
          </Badge>
        </span>
        <span className="flex items-center justify-end">
          <Button
            type="button"
            size="sm"
            variant="ghost"
            aria-label={`Delete rule ${rule.pattern}`}
            onClick={() => setDeleteTarget(rule)}
          >
            Delete
          </Button>
        </span>
      </div>
    ));
  }

  return (
    // GRID and FORM_GRID are fixed-pixel column templates, and the shell
    // around this is h-screen/overflow-hidden (components/app-shell.tsx) —
    // so a narrow viewport clipped the right-hand columns with nothing to
    // scroll. On the column, so the header and the rows move together.
    <div data-slot="h-scroll" className="flex h-full min-h-0 flex-col overflow-x-auto">
      <div className="flex shrink-0 flex-wrap items-center gap-3 border-b border-border px-4 py-2.5">
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            Group
          </span>
          <NativeSelect
            value={String(groupId)}
            onChange={(e) => setSelectedId(Number(e.target.value))}
            disabled={groups.isPending || !groups.data || groups.data.length === 0}
            aria-label="Group"
          >
            {groups.data?.map((g) => (
              <NativeSelectOption key={g.id} value={String(g.id)}>
                {g.name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="search patterns…"
          aria-label="Search patterns"
          className="w-60"
        />
        <div className="flex items-center gap-1.5">
          <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
            Action
          </span>
          <NativeSelect
            value={actionFilter}
            onChange={(e) => setActionFilter(e.target.value as "" | Rule["action"])}
            aria-label="Filter by action"
          >
            <NativeSelectOption value="">All actions</NativeSelectOption>
            <NativeSelectOption value="allow">allow</NativeSelectOption>
            <NativeSelectOption value="block">block</NativeSelectOption>
          </NativeSelect>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <MatchOrderPopover />
          {!addOpen && (
            <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
              <Plus />
              Add rule
            </Button>
          )}
        </div>
      </div>

      {rules.isError && rules.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="rules"
            onRetry={() => void rules.refetch()}
            isRetrying={rules.isFetching}
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
        <span>Action</span>
        <span>Pattern</span>
        <span>Type</span>
        <span className="text-right">Actions</span>
      </div>

      {/* Keyed on the group so switching groups gives a fresh form rather
          than carrying a half-typed pattern across a scope change. */}
      {addOpen && <AddRuleRow key={groupId} groupId={groupId} onClose={() => setAddOpen(false)} />}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title="Delete this rule?"
        description={
          deleteTarget && (
            <>
              The {deleteTarget.action} rule for{" "}
              <code className="font-mono break-all text-foreground">{deleteTarget.pattern}</code>{" "}
              stops applying to {groupName}.
            </>
          )
        }
        isPending={deleteRule.isPending}
        onConfirm={onConfirmDelete}
      />
    </div>
  );
}
