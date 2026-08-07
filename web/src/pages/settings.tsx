import { useRef } from "react";
import {
  Database,
  ListChecks,
  ScrollText,
  Server,
  ShieldBan,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
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
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  Skeleton,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import type { Settings } from "../api/types";
import { useSettings, useUpdateSetting } from "../hooks/use-settings";
import { StaleDataAlert } from "../components/stale-data-alert";
import { requiredText } from "../lib/schemas";

// --- field model -------------------------------------------------------
// One row per key in internal/api/settings_handlers.go's editableSettings
// map — 11 keys, no more, no less (see the exhaustiveness note by
// SETTING_GROUPS below). Each field's `schema` mirrors that map's check
// function exactly, so a value accepted here is one PUT /settings will
// also accept, and nothing rejected here would have been rejected there
// either — see the schemas just below for the key-by-key mapping.

interface SelectOption {
  value: string;
  /** The stored value, shown verbatim — these are the strings the API,
   * the config file and the docs all use. */
  label: string;
  /** What the choice produces, when that is short enough to show: the
   * answer a blocking mode returns, the client IP a privacy level records. */
  sample?: string;
  /** One line on what the option does. */
  hint?: string;
}

interface BaseField {
  key: string;
  label: string;
  description: string;
  /** cache.* and lists.refresh_hours are read once at startup — see
   * docs/configuration.md's "restart required" note — everything else
   * hot-reloads live. */
  restartRequired?: boolean;
  /** Every field's value is a string on the wire and in the form, so each
   * schema is string-in/string-out — it only ever trims and judges. */
  schema: z.ZodType<string, string>;
}

interface TextField extends BaseField {
  kind: "text";
  placeholder?: string;
}

interface IntField extends BaseField {
  kind: "int";
}

interface SelectField extends BaseField {
  kind: "select";
  options: SelectOption[];
}

type SettingField = TextField | IntField | SelectField;

interface SettingGroup {
  title: string;
  description: string;
  icon: LucideIcon;
  fields: SettingField[];
}

/** Mirrors editableSettings["upstreams"]: `strings.TrimSpace(v) != ""` —
 * nothing more. The server does no host:port format checking, so this
 * must not invent stricter format rules and false-reject a legitimate
 * entry (the lesson from Tasks 9-11's own over-strict validators). */
const nonEmptySchema = requiredText("Required.");

/** Mirrors editableSettings' oneOf(...) checks (upstream.strategy,
 * blocking.mode, qlog.privacy): membership in a fixed, exact-match set.
 * Not z.enum(): the message has to name the accepted values in the
 * server's own order, and the form's value type stays a plain string. */
function oneOfSchema(values: readonly string[]) {
  return z
    .string()
    .trim()
    .refine((value) => values.includes(value), "Pick one of the options.");
}

/** Mirrors editableSettings' nonNegInt(...): Go's
 * strconv.ParseInt(v, 10, 64) succeeding with a non-negative result — an
 * optional leading sign then digits only, no surrounding whitespace
 * tolerated (the value is trimmed first, so this only rejects internal
 * whitespace/non-digits, matching ParseInt exactly on the trimmed form).
 *
 * Checked in order so each rejection says the actual reason. One regex
 * covering all of them answered an emptied field with "Enter a whole
 * number", which describes the format rather than the problem. */
const nonNegIntSchema = z
  .string()
  .trim()
  .superRefine((value, ctx) => {
    const fail = (message: string) => ctx.addIssue({ code: "custom", message });
    if (!value) return fail("Required.");
    if (!/^[+-]?\d+$/.test(value)) return fail("Numbers only — no units or separators.");
    const n = Number(value);
    if (n < 0) return fail("Can't be negative.");
    if (!Number.isSafeInteger(n)) return fail("Too large.");
  });

// Fallback display defaults, matching docs/configuration.md's table —
// used only if the server's GET /settings response is unexpectedly
// missing a key (in practice every key is seeded on first run).
const SETTING_DEFAULTS: Record<string, string> = {
  upstreams: "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
  "upstream.strategy": "race",
  "blocking.mode": "null-ip",
  "blocking.ttl": "30",
  "cache.min_ttl": "0",
  "cache.max_ttl": "86400",
  "cache.max_entries": "10000",
  "cache.serve_stale_for": "86400",
  "lists.refresh_hours": "24",
  "qlog.retention_days": "90",
  "qlog.privacy": "full",
};

// Exhaustiveness: this must list exactly the 11 keys in
// internal/api/settings_handlers.go's editableSettings — no fewer (an
// editable setting the admin can't reach) and no more (a PUT the server
// would 400 with "setting not editable").
const SETTING_GROUPS: SettingGroup[] = [
  {
    title: "Upstreams",
    description: "Where dnsaur forwards queries it doesn't answer locally or from cache.",
    icon: Server,
    fields: [
      {
        key: "upstreams",
        kind: "text",
        label: "Upstream resolvers",
        description: "Comma-separated host:port pairs, tried in order.",
        placeholder: "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
        schema: nonEmptySchema,
      },
      {
        key: "upstream.strategy",
        kind: "select",
        label: "Upstream strategy",
        // Kept short on purpose — this sits in a narrow grid column, and a
        // long run-on wraps into a ragged stack there. The per-strategy
        // detail lives in the option labels below, so this only has to say
        // what the setting decides and that changing it takes effect
        // immediately (applySettings rebuilds the forwarder on every
        // settings write — see internal/app/app.go). All three strategies
        // are implemented in internal/upstream/forwarder.go; an earlier
        // version of this line wrongly claimed only Race was.
        description: "Which upstream answers a query. Takes effect immediately.",
        options: [
          { value: "race", label: "race", hint: "All at once, first good answer." },
          { value: "fastest", label: "fastest", hint: "By measured latency." },
          { value: "failover", label: "failover", hint: "In the order listed." },
        ],
        schema: oneOfSchema(["failover", "fastest", "race"]),
      },
    ],
  },
  {
    title: "Blocking",
    description: "How dnsaur responds when a query matches a block list or rule.",
    icon: ShieldBan,
    fields: [
      {
        key: "blocking.mode",
        kind: "select",
        label: "Blocking mode",
        description: "How dnsaur answers a blocked query.",
        options: [
          {
            value: "null-ip",
            label: "null-ip",
            sample: "0.0.0.0",
            hint: "Unroutable address.",
          },
          {
            value: "nxdomain",
            label: "nxdomain",
            sample: "NXDOMAIN",
            hint: "Name doesn't exist.",
          },
        ],
        schema: oneOfSchema(["null-ip", "nxdomain"]),
      },
      {
        key: "blocking.ttl",
        kind: "int",
        label: "Blocked response TTL (seconds)",
        description: "How long resolvers may cache a blocked answer.",
        schema: nonNegIntSchema,
      },
    ],
  },
  {
    title: "Cache",
    description: "In-memory response cache sizing. Read once at startup.",
    icon: Database,
    fields: [
      {
        key: "cache.min_ttl",
        kind: "int",
        label: "Minimum cache TTL (seconds)",
        description: "Floor enforced on cached response TTLs.",
        restartRequired: true,
        schema: nonNegIntSchema,
      },
      {
        key: "cache.max_ttl",
        kind: "int",
        label: "Maximum cache TTL (seconds)",
        description: "Ceiling clamp on cached response TTLs.",
        restartRequired: true,
        schema: nonNegIntSchema,
      },
      {
        key: "cache.max_entries",
        kind: "int",
        label: "Maximum cache entries",
        description: "Upper bound on how many entries the in-memory cache holds.",
        restartRequired: true,
        schema: nonNegIntSchema,
      },
      {
        key: "cache.serve_stale_for",
        kind: "int",
        label: "Serve stale for (seconds)",
        description: "How long a stale entry may still be served if upstream is unreachable.",
        restartRequired: true,
        schema: nonNegIntSchema,
      },
    ],
  },
  {
    title: "Query log",
    description: "What gets recorded per query, and for how long.",
    icon: ScrollText,
    fields: [
      {
        key: "qlog.privacy",
        kind: "select",
        label: "Query log privacy",
        description: "How much per-query detail is recorded.",
        options: [
          {
            value: "full",
            label: "full",
            sample: "192.168.11.104",
            hint: "Client IP as seen.",
          },
          { value: "anon", label: "anon", sample: "192.168.11.0", hint: "Last octet masked." },
          { value: "none", label: "none", sample: "—", hint: "Nothing logged. History stays." },
        ],
        schema: oneOfSchema(["full", "anon", "none"]),
      },
      {
        key: "qlog.retention_days",
        kind: "int",
        label: "Retention (days)",
        description: "How long query log rows are kept before the pruner deletes them.",
        schema: nonNegIntSchema,
      },
    ],
  },
  {
    title: "Lists",
    description: "How often subscribed block/allow lists are re-fetched.",
    icon: ListChecks,
    fields: [
      {
        key: "lists.refresh_hours",
        kind: "int",
        label: "Refresh interval (hours)",
        description: "How often blocklists and allowlists are re-downloaded and recompiled.",
        restartRequired: true,
        schema: nonNegIntSchema,
      },
    ],
  },
];

// No Storage/info section: there's no read-only endpoint (DB size, cache
// occupancy, etc.) to source it from honestly — see hooks/use-stats.ts and
// hooks/use-filters.ts for the full set of reads available today, and
// docs/api.md for the full route list. Adding one here would mean either
// fabricating numbers or wiring a fake "0 B" placeholder, both worse than
// omitting it until a real endpoint exists.

const ALL_FIELDS: SettingField[] = SETTING_GROUPS.flatMap((group) => group.fields);

type SettingsFormValues = Record<string, string>;

// react-hook-form's field `name` is a dot-path into a nested object (its
// internal get/set treat "." as a path separator, the same way lodash's
// _.get/_.set do) — but every settings key here (e.g. "blocking.ttl",
// "cache.min_ttl") IS the literal, flat key the API expects, not a nested
// path. Using field.key directly as the RHF name would silently produce
// both a stray top-level "blocking.ttl" AND a nested `{ blocking: { ttl }
// }`, corrupting handleSubmit's values. Every RHF-facing name goes through
// this sanitizer instead; field.key remains the one true API key,
// recovered from the field definition (not the form values) wherever a
// PUT payload is built — see onSubmit.
function rhfName(key: string): string {
  return key.replaceAll(".", "__");
}

function buildDefaults(settings: Settings): SettingsFormValues {
  const values: SettingsFormValues = {};
  for (const field of ALL_FIELDS) {
    values[rhfName(field.key)] = settings[field.key] ?? SETTING_DEFAULTS[field.key] ?? "";
  }
  return values;
}

// One schema for the whole form, keyed the same way the form is (sanitized
// names, not API keys) so the resolver's issue paths land on the right
// fields. Assembled from the field definitions rather than written out
// again, which keeps the "exactly editableSettings' 11 keys" guarantee
// above the single thing to maintain.
const SETTINGS_SCHEMA = z.object(
  Object.fromEntries(ALL_FIELDS.map((field) => [rhfName(field.key), field.schema])),
);

/**
 * Whether a whole section waits for a restart.
 *
 * Read off the fields rather than declared on the group: `restartRequired`
 * is a property of the setting, and a group whose fields disagreed would be
 * a design problem to notice rather than one to paper over with a group
 * flag. Today every group is all-or-nothing.
 */
function groupNeedsRestart(group: SettingGroup): boolean {
  return group.fields.length > 0 && group.fields.every((f) => f.restartRequired);
}

/**
 * A one-of choice as a list of rows, not a select.
 *
 * These three settings each change what dnsaur *does* with a query, and
 * every option needs a line of explanation — which a select can only show
 * one at a time, after opening it. Laid out, the choice is comparable
 * without interacting with it.
 *
 * Native radios inside a fieldset: correct semantics, arrow-key navigation
 * and grouping come free, and the visible square is a sibling driven by
 * `peer-checked` rather than a div pretending to be an input.
 */
function SettingRadioList({
  field,
  value,
  onChange,
}: {
  field: SettingField & { kind: "select" };
  value: string;
  onChange: (next: string) => void;
}) {
  // aria-label rather than an sr-only <legend>: the visible FormLabel above
  // already carries this text, and a legend would put a second copy of it in
  // both the DOM and the accessibility tree.
  return (
    <fieldset aria-label={field.label} className="flex flex-col border border-border">
      {field.options.map((option) => (
        <label
          key={option.value}
          className="grid cursor-pointer grid-cols-[14px_1fr] items-start gap-2.5 border-b border-border-muted p-2.5 last:border-b-0 has-[:checked]:bg-accent"
        >
          <input
            type="radio"
            name={rhfName(field.key)}
            value={option.value}
            checked={value === option.value}
            onChange={() => onChange(option.value)}
            className="peer sr-only"
          />
          <span
            aria-hidden
            className="mt-0.5 size-3 border border-border peer-checked:border-primary peer-checked:bg-primary peer-checked:shadow-[inset_0_0_0_2px_var(--background)] peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-ring"
          />
          <span className="flex min-w-0 flex-col gap-0.5">
            <span className="flex items-baseline gap-2">
              <span className="font-mono text-xs peer-checked:font-semibold">{option.label}</span>
              {option.sample && (
                <span className="font-mono text-xs text-muted-foreground">{option.sample}</span>
              )}
            </span>
            {option.hint && (
              <span className="text-xs text-pretty text-muted-foreground">{option.hint}</span>
            )}
          </span>
        </label>
      ))}
    </fieldset>
  );
}

/**
 * One setting: what it is called, what it is set to, and — when it has been
 * touched — what it used to be.
 *
 * The key is printed beside the label because it is what the API, the
 * config file and the docs all call this thing, and an admin reading any of
 * those needs to be able to find it here.
 */
function SettingRow({
  field,
  control,
}: {
  field: SettingField;
  control: Control<SettingsFormValues>;
}) {
  const name = rhfName(field.key);

  return (
    <FormField
      control={control}
      name={name}
      render={({ field: rhfField, fieldState }) => {
        return (
          <FormItem className="min-w-0">
            <div className="flex items-baseline gap-2">
              <FormLabel>{field.label}</FormLabel>
              <span className="font-mono text-xs text-muted-foreground">{field.key}</span>
            </div>

            {field.kind === "select" ? (
              <SettingRadioList field={field} value={rhfField.value} onChange={rhfField.onChange} />
            ) : (
              <FormControl>
                <Input
                  {...rhfField}
                  inputMode={field.kind === "int" ? "numeric" : undefined}
                  placeholder={field.kind === "text" ? field.placeholder : undefined}
                  autoComplete="off"
                  className={cn(
                    field.kind === "text" && "font-mono",
                    field.kind === "int" && "w-32",
                  )}
                />
              </FormControl>
            )}

            {/* The description stays put — it says what the setting does,
                which is worth knowing whether or not the field has been
                touched. Only an error displaces it. */}
            {fieldState.error ? (
              <FormMessage />
            ) : (
              <FormDescription>{field.description}</FormDescription>
            )}
          </FormItem>
        );
      }}
    />
  );
}

// --- skeleton --------------------------------------------------------------

function SettingsSkeleton() {
  return (
    <div className="flex flex-col" aria-hidden="true">
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} className="grid grid-cols-[288px_1fr] border-b border-border">
          <div className="flex flex-col gap-2 border-r border-border p-5">
            <Skeleton className="h-5 w-32" />
            <Skeleton className="h-4 w-28" />
          </div>
          <div className="flex flex-col gap-3 p-5">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-2/3" />
          </div>
        </div>
      ))}
    </div>
  );
}

// --- save bar --------------------------------------------------------------

/**
 * What is unsaved, and when it will take effect.
 *
 * The second half is the point. Half these settings apply the moment they
 * are written and half wait for a restart, so "3 unsaved changes" alone
 * leaves an admin unsure whether saving is the end of the job.
 */
function SaveBar({
  hotCount,
  restartCount,
  isSubmitting,
  onDiscard,
}: {
  hotCount: number;
  restartCount: number;
  isSubmitting: boolean;
  onDiscard: () => void;
}) {
  const total = hotCount + restartCount;
  const dirty = total > 0;

  // Says only which of the two it is. The sections are already badged, so
  // the mixed case does not need to re-explain the split.
  let detail = "";
  if (restartCount > 0 && hotCount > 0) {
    detail = "Some need a restart.";
  } else if (restartCount > 0) {
    detail = "Needs a restart.";
  } else if (hotCount > 0) {
    detail = "Applies on save.";
  }

  return (
    <div
      className={cn(
        "flex shrink-0 items-center gap-3.5 border-b border-border px-5 py-2.5",
        dirty && "bg-warning/8 shadow-[inset_3px_0_0_var(--warning)]",
      )}
    >
      <output className="flex min-w-0 items-baseline gap-2.5">
        <span
          className={cn(
            "font-mono text-xs font-semibold tracking-widest whitespace-nowrap uppercase",
            dirty ? "text-warning-foreground" : "text-muted-foreground",
          )}
        >
          {dirty ? `${total} unsaved ${total === 1 ? "change" : "changes"}` : "All changes saved"}
        </span>
        <span className="text-sm text-muted-foreground">{detail}</span>
      </output>
      <div className="ml-auto flex items-center gap-2">
        <Button type="button" size="sm" variant="ghost" disabled={!dirty} onClick={onDiscard}>
          Discard
        </Button>
        <Button type="submit" size="sm" disabled={!dirty || isSubmitting}>
          {isSubmitting ? "Saving…" : "Save changes"}
        </Button>
      </div>
    </div>
  );
}

// --- form ------------------------------------------------------------------

function SettingsForm({ settings }: { settings: Settings }) {
  const updateSetting = useUpdateSetting();
  const defaultsRef = useRef(buildDefaults(settings));
  const form = useForm<SettingsFormValues>({
    resolver: zodResolver(SETTINGS_SCHEMA),
    defaultValues: defaultsRef.current,
  });

  // Watched rather than read from formState.dirtyFields: RHF marks a field
  // dirty against its *default*, which the save below deliberately moves
  // per-field on partial success. Comparing to the same baseline the submit
  // diffs against keeps the bar, the per-field dots and the payload
  // agreeing with each other.
  const values = form.watch();
  const baseline = defaultsRef.current;
  const changedFields = ALL_FIELDS.filter(
    (f) => (values[rhfName(f.key)] ?? "") !== (baseline[rhfName(f.key)] ?? ""),
  );
  const restartCount = changedFields.filter((f) => f.restartRequired).length;
  const hotCount = changedFields.length - restartCount;

  async function onSubmit(submitted: SettingsFormValues) {
    const base = defaultsRef.current;
    // Diff against ALL_FIELDS (the authoritative key list), not
    // Object.entries(values) — RHF's `name` is a dot-path, so `values`
    // also holds react-hook-form's own bookkeeping shape for any
    // dotted-path collisions; walking the known fields instead sidesteps
    // that entirely and recovers each field's real (dotted) API key
    // straight from its definition rather than from the sanitized RHF
    // name.
    const changed = ALL_FIELDS.map((field) => {
      const name = rhfName(field.key);
      const value = (submitted[name] ?? "").trim();
      return { apiKey: field.key, name, value };
    }).filter(({ name, value }) => value !== (base[name] ?? ""));
    if (changed.length === 0) return;

    const settled = await Promise.allSettled(
      changed.map(({ apiKey, value }) => updateSetting.mutateAsync({ key: apiKey, value })),
    );

    const nextBaseline = { ...base };
    const failedKeys: string[] = [];
    settled.forEach((result, i) => {
      const { apiKey, name, value } = changed[i];
      if (result.status === "fulfilled") {
        nextBaseline[name] = value;
      } else {
        failedKeys.push(apiKey);
      }
    });
    defaultsRef.current = nextBaseline;
    // keepValues: true — only the baseline (dirty comparison) moves;
    // fields that failed to save keep the admin's attempted value in
    // place, still marked dirty, ready to retry.
    form.reset(nextBaseline, { keepValues: true });

    const savedCount = changed.length - failedKeys.length;
    if (failedKeys.length === 0) {
      toast.success(`${savedCount} setting${savedCount === 1 ? "" : "s"} updated`);
    } else if (savedCount > 0) {
      toast.error(`Saved ${savedCount}, but couldn't save ${failedKeys.join(", ")} — try again`);
    } else {
      const firstFailure = settled.find((r): r is PromiseRejectedResult => r.status === "rejected");
      const reason = firstFailure?.reason;
      toast.error(
        reason instanceof ApiError ? reason.message : "Couldn't save settings — try again",
      );
    }
  }

  return (
    <Form {...form}>
      <form
        className="flex h-full min-h-0 flex-col"
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
      >
        <SaveBar
          hotCount={hotCount}
          restartCount={restartCount}
          isSubmitting={form.formState.isSubmitting}
          onDiscard={() => form.reset(defaultsRef.current)}
        />

        <div className="min-h-0 flex-1 overflow-y-auto">
          {SETTING_GROUPS.map((group) => {
            const restart = groupNeedsRestart(group);
            return (
              <section
                key={group.title}
                className={cn(
                  "grid grid-cols-[288px_1fr] border-b border-border",
                  restart && "bg-warning/4",
                )}
              >
                <div
                  className={cn(
                    "flex flex-col items-start gap-2 border-r border-border p-5",
                    restart && "shadow-[inset_3px_0_0_var(--warning)]",
                  )}
                >
                  <h2 className="font-heading text-base font-semibold">{group.title}</h2>
                  {/* The distinction this page exists to make legible: a
                      value that takes effect on save, versus one that sits
                      in the database until the process restarts. */}
                  <Badge variant={restart ? "warning-light" : "success-light"}>
                    {restart ? "Needs a restart" : "Applies instantly"}
                  </Badge>
                  <p className="text-xs text-pretty text-muted-foreground">{group.description}</p>
                </div>
                <div
                  className={cn(
                    "grid items-start gap-5 p-5",
                    group.fields.length >= 4
                      ? "sm:grid-cols-4"
                      : group.fields.length > 1
                        ? "sm:grid-cols-2"
                        : "max-w-sm",
                  )}
                >
                  {group.fields.map((field) => (
                    <SettingRow key={field.key} field={field} control={form.control} />
                  ))}
                </div>
              </section>
            );
          })}
        </div>
      </form>
    </Form>
  );
}

// --- page ------------------------------------------------------------------

/**
 * Settings — the 11 keys in internal/api/settings_handlers.go's
 * editableSettings, saved together rather than one at a time.
 */
export function SettingsPage() {
  const settings = useSettings();

  if (settings.isPending) return <SettingsSkeleton />;

  if (settings.data === undefined) {
    return (
      <div className="p-5">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load settings</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {settings.isError && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="settings"
            onRetry={() => void settings.refetch()}
            isRetrying={settings.isFetching}
          />
        </div>
      )}
      <div className="min-h-0 flex-1">
        <SettingsForm settings={settings.data} />
      </div>
    </div>
  );
}
