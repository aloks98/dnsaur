import { useRef } from "react";
import {
  Database,
  ListChecks,
  Power,
  ScrollText,
  Server,
  ShieldBan,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
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
  Skeleton,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import type { Settings } from "../api/types";
import { useSettings, useUpdateSetting } from "../hooks/use-settings";

// --- field model -------------------------------------------------------
// One row per key in internal/api/settings_handlers.go's editableSettings
// map — 11 keys, no more, no less (see the exhaustiveness note by
// SETTING_GROUPS below). Each field's `validate` mirrors that map's check
// function exactly, so a value accepted here is one PUT /settings will
// also accept, and nothing rejected here would have been rejected there
// either — see the validators just below for the key-by-key mapping.

interface SelectOption {
  value: string;
  label: string;
}

interface BaseField {
  key: string;
  label: string;
  description: string;
  /** cache.* and lists.refresh_hours are read once at startup — see
   * docs/configuration.md's "restart required" note — everything else
   * hot-reloads live. */
  restartRequired?: boolean;
  validate: (value: string) => string | true;
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
function validateNonEmpty(value: string): string | true {
  return value !== "" ? true : "Can't be empty";
}

/** Mirrors editableSettings' oneOf(...) checks (upstream.strategy,
 * blocking.mode, qlog.privacy): membership in a fixed, exact-match set. */
function validateOneOf(values: readonly string[]) {
  return (value: string): string | true =>
    values.includes(value) ? true : `Must be one of: ${values.join(", ")}`;
}

/** Mirrors editableSettings' nonNegInt(...): Go's
 * strconv.ParseInt(v, 10, 64) succeeding with a non-negative result — an
 * optional leading sign then digits only, no surrounding whitespace
 * tolerated (the caller trims first, so this only rejects internal
 * whitespace/non-digits, matching ParseInt exactly on the trimmed form). */
function validateNonNegInt(value: string): string | true {
  if (!/^[+-]?\d+$/.test(value)) return "Enter a whole number";
  const n = Number(value);
  return Number.isSafeInteger(n) && n >= 0 ? true : "Must be zero or greater";
}

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
        validate: validateNonEmpty,
      },
      {
        key: "upstream.strategy",
        kind: "select",
        label: "Upstream strategy",
        // Kept to two short sentences on purpose — this sits in a narrow
        // grid column, and one long run-on wraps into a ragged stack of
        // short lines there. The "not implemented yet" caveat still needs
        // to be said (silently picking a no-op strategy is a real
        // gotcha), just said briefly.
        description: "How dnsaur picks among upstream resolvers. Only Race is implemented today.",
        options: [
          { value: "race", label: "Race — query all, use the fastest reply" },
          { value: "failover", label: "Failover — try in order, fall back on failure" },
          { value: "fastest", label: "Fastest — prefer the historically quickest resolver" },
        ],
        validate: validateOneOf(["failover", "fastest", "race"]),
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
          { value: "null-ip", label: "Null IP (0.0.0.0)" },
          { value: "nxdomain", label: "NXDOMAIN" },
        ],
        validate: validateOneOf(["null-ip", "nxdomain"]),
      },
      {
        key: "blocking.ttl",
        kind: "int",
        label: "Blocked response TTL (seconds)",
        description: "How long resolvers may cache a blocked answer.",
        validate: validateNonNegInt,
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
        validate: validateNonNegInt,
      },
      {
        key: "cache.max_ttl",
        kind: "int",
        label: "Maximum cache TTL (seconds)",
        description: "Ceiling clamp on cached response TTLs.",
        restartRequired: true,
        validate: validateNonNegInt,
      },
      {
        key: "cache.max_entries",
        kind: "int",
        label: "Maximum cache entries",
        description: "Upper bound on how many entries the in-memory cache holds.",
        restartRequired: true,
        validate: validateNonNegInt,
      },
      {
        key: "cache.serve_stale_for",
        kind: "int",
        label: "Serve stale for (seconds)",
        description: "How long a stale entry may still be served if upstream is unreachable.",
        restartRequired: true,
        validate: validateNonNegInt,
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
          { value: "full", label: "Full — record client IPs" },
          { value: "anon", label: "Anonymized — client IPs redacted" },
          { value: "none", label: "None — don't log individual queries" },
        ],
        validate: validateOneOf(["full", "anon", "none"]),
      },
      {
        key: "qlog.retention_days",
        kind: "int",
        label: "Retention (days)",
        description: "How long query log rows are kept before the pruner deletes them.",
        validate: validateNonNegInt,
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
        validate: validateNonNegInt,
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

// --- restart-required badge --------------------------------------------

function RestartBadge() {
  return (
    <Badge variant="warning-light" size="sm">
      <Power />
      Restart required
    </Badge>
  );
}

// --- field control -------------------------------------------------------

function SettingFieldControl({
  field,
  control,
}: {
  field: SettingField;
  control: Control<SettingsFormValues>;
}) {
  return (
    <FormField
      control={control}
      name={rhfName(field.key)}
      rules={{ validate: (value: string) => field.validate(value.trim()) }}
      render={({ field: rhfField }) => (
        <FormItem>
          <div className="flex flex-wrap items-center gap-2">
            <FormLabel>{field.label}</FormLabel>
            {field.restartRequired && <RestartBadge />}
          </div>
          {field.kind === "select" ? (
            // `items` maps each value to its display label — without it,
            // SelectValue renders the raw stored value (e.g. "null-ip")
            // instead of the option's label (base-ui's documented
            // behavior for Select.Value with no `items`/children render
            // prop).
            <Select
              items={Object.fromEntries(field.options.map((o) => [o.value, o.label]))}
              value={rhfField.value}
              onValueChange={rhfField.onChange}
            >
              <SelectTrigger aria-label={field.label}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {field.options.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <FormControl>
              <Input
                {...rhfField}
                type={field.kind === "int" ? "number" : "text"}
                min={field.kind === "int" ? 0 : undefined}
                placeholder={field.kind === "text" ? field.placeholder : undefined}
                autoComplete="off"
                // A uniform width across every field in a section — text,
                // select, and number alike — reads as one matrix of peer,
                // comparable controls; a narrow fixed-width number input
                // floating in a wide grid cell would look unintentional
                // next to the full-width fields beside it.
                className={field.kind === "text" ? "font-mono" : undefined}
              />
            </FormControl>
          )}
          <FormDescription>{field.description}</FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  );
}

// --- skeleton --------------------------------------------------------------

function SettingsSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-hidden="true">
      {SETTING_GROUPS.map((group) => (
        <Card key={group.title}>
          <CardHeader>
            <Skeleton className="h-5 w-40" />
            <Skeleton className="mt-1.5 h-4 w-72" />
          </CardHeader>
          {/* Mirrors SettingsForm's own field-count-based layout exactly
              (grid gaps included) — a skeleton with different proportions
              than the content that replaces it reads as a visible shift
              rather than a seamless load. */}
          {group.fields.length > 1 ? (
            <CardContent className="grid grid-cols-1 gap-x-6 gap-y-5 sm:grid-cols-2">
              {group.fields.map((field) => (
                <div key={field.key} className="flex flex-col gap-2">
                  <Skeleton className="h-4 w-32" />
                  <Skeleton className="h-8 w-full" />
                </div>
              ))}
            </CardContent>
          ) : (
            <CardContent className="max-w-sm">
              <div className="flex flex-col gap-2">
                <Skeleton className="h-4 w-32" />
                <Skeleton className="h-8 w-full" />
              </div>
            </CardContent>
          )}
        </Card>
      ))}
    </div>
  );
}

// --- form --------------------------------------------------------------

/**
 * The full settings form — one react-hook-form instance spanning every
 * Card section, so a single Save validates and diffs every field at once.
 * `defaultsRef` is the save baseline: captured once from the first
 * successful GET (never re-synced from background refetches, so a
 * concurrent settings change elsewhere never silently stomps an admin's
 * in-progress edits here) and advanced only by a field's own successful
 * PUT — see onSubmit.
 */
function SettingsForm({ settings }: { settings: Settings }) {
  const updateSetting = useUpdateSetting();
  const defaultsRef = useRef(buildDefaults(settings));
  const form = useForm<SettingsFormValues>({ defaultValues: defaultsRef.current });

  async function onSubmit(values: SettingsFormValues) {
    const baseline = defaultsRef.current;
    // Diff against ALL_FIELDS (the authoritative key list), not
    // Object.entries(values) — RHF's `name` is a dot-path, so `values`
    // also holds react-hook-form's own bookkeeping shape for any
    // dotted-path collisions; walking the known fields instead sidesteps
    // that entirely and recovers each field's real (dotted) API key
    // straight from its definition rather than from the sanitized RHF
    // name.
    const changed = ALL_FIELDS.map((field) => {
      const name = rhfName(field.key);
      const value = (values[name] ?? "").trim();
      return { apiKey: field.key, name, value };
    }).filter(({ name, value }) => value !== (baseline[name] ?? ""));
    if (changed.length === 0) return;

    const settled = await Promise.allSettled(
      changed.map(({ apiKey, value }) => updateSetting.mutateAsync({ key: apiKey, value })),
    );

    const nextBaseline = { ...baseline };
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

  const isDirty = form.formState.isDirty;
  const isSubmitting = form.formState.isSubmitting;
  const statusText = isDirty ? "You have unsaved changes." : "All changes saved.";

  return (
    <Form {...form}>
      <form
        className="flex flex-col gap-6"
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
      >
        <SaveBar statusText={statusText} isDirty={isDirty} isSubmitting={isSubmitting} announce />

        {SETTING_GROUPS.map((group) => (
          <Card key={group.title}>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <group.icon className="size-4 text-muted-foreground" aria-hidden="true" />
                {group.title}
              </CardTitle>
              <CardDescription>{group.description}</CardDescription>
            </CardHeader>
            {group.fields.length > 1 ? (
              <CardContent className="grid grid-cols-1 gap-x-6 gap-y-5 sm:grid-cols-2">
                {group.fields.map((field) => (
                  <div
                    key={field.key}
                    className={field.kind === "text" ? "sm:col-span-2" : undefined}
                  >
                    <SettingFieldControl field={field} control={form.control} />
                  </div>
                ))}
              </CardContent>
            ) : (
              // A single-field section (Lists) doesn't get the 2-col grid
              // — an only child would occupy one cell and leave the other
              // half of the card visibly empty. A width-capped block reads
              // as one deliberate control instead of a stretched-out form.
              <CardContent className="max-w-sm">
                <SettingFieldControl field={group.fields[0]} control={form.control} />
              </CardContent>
            )}
          </Card>
        ))}

        {/* Mirrors the header's Save affordance — on a page this long, a
            change made in the last card (Lists) shouldn't require
            scrolling back to the top to save it. */}
        <SaveBar statusText={statusText} isDirty={isDirty} isSubmitting={isSubmitting} />
      </form>
    </Form>
  );
}

function SaveBar({
  statusText,
  isDirty,
  isSubmitting,
  announce = false,
}: {
  statusText: string;
  isDirty: boolean;
  isSubmitting: boolean;
  /** Only the top bar announces — with two bars showing the identical
   * status text, a live region on both would read it out twice to a
   * screen reader on every change. */
  announce?: boolean;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <p className="text-sm text-muted-foreground" aria-live={announce ? "polite" : undefined}>
        {statusText}
      </p>
      <Button type="submit" size="sm" disabled={!isDirty || isSubmitting}>
        {isSubmitting ? "Saving…" : "Save changes"}
      </Button>
    </div>
  );
}

// --- page ------------------------------------------------------------------

/**
 * Settings — server, upstream, blocking, cache, query log, and list-refresh
 * configuration. GET/PUT /settings via use-settings.ts's hooks (Task 12).
 * Every field's client-side validation mirrors
 * internal/api/settings_handlers.go's editableSettings map exactly (see the
 * validators above), so nothing accepted here is later 400'd by the
 * server, and nothing the server would accept is wrongly blocked here.
 */
export function SettingsPage() {
  const settings = useSettings();

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-heading font-semibold text-foreground">Settings</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Upstream resolvers, blocking behavior, cache sizing, and query log retention.
        </p>
      </div>

      <Separator />

      {settings.isPending && <SettingsSkeleton />}

      {settings.isError && (
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load settings</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      )}

      {settings.isSuccess && <SettingsForm settings={settings.data} />}
    </div>
  );
}
