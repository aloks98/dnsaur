import { Fragment, useId, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Plus, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import {
  useFieldArray,
  useForm,
  useFormContext,
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
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  Switch,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { DHCPClass, DHCPScope, DHCPStatus } from "../../api/types";
import {
  useApplyDHCP,
  useClasses,
  useCreateScope,
  useDeleteScope,
  useDHCPStatus,
  useReservations,
  useScopes,
  useUpdateScope,
  type ScopeInput,
} from "../../hooks/use-dhcp";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { ConfirmDeleteDialog } from "../dialogs";
import { requiredText } from "../../lib/schemas";
import { DHCP_RESERVATIONS_PATH } from "../../lib/nav";
import { parseIPv4 } from "../../lib/acl";

/** The board's nine columns, shared by the head and every row so the two
 * cannot drift. */
const GRID =
  "grid grid-cols-[1fr_150px_230px_90px_128px_130px_130px_66px_104px] items-center gap-3.5 px-4";

// --- engine status line ----------------------------------------------------

/** The dot's colour and the line's, per state. `transparent` is not an
 * option here: the muted square is what keeps the line's text from shifting
 * left when the state changes. */
type EngineTone = "quiet" | "bad";

interface EngineLine {
  text: string;
  tone: EngineTone;
  /** The rejected state, and the only one with an action. */
  rejected: boolean;
}

/**
 * The status line above the table (spec §8.4), as its exact words.
 *
 * Pure, and exported, because it is four states plus a replica case and
 * every one of them is a sentence an operator reads literally — a switch
 * buried in JSX is a switch nobody can test one branch at a time.
 *
 * `ha.peer` is what the partner calls itself, as the engine talking to it
 * reports the name. It is `omitempty`, and an engine whose `status-get` does
 * not carry one leaves the clause out rather than inventing a peer from the
 * config-sync side — a different fact that merely usually refers to the same
 * box.
 */
export function engineLine(status: DHCPStatus | undefined, isReplica: boolean): EngineLine | null {
  if (!status?.enabled) return null;
  if (status.engine === "unreachable") {
    return { text: "Engine unreachable", tone: "bad", rejected: false };
  }
  if (status.engine === "config rejected") {
    return { text: `Config rejected: ${status.message ?? ""}`, tone: "bad", rejected: true };
  }
  const version = status.engine_version ?? "";
  if (status.ha) {
    const peer = status.ha.peer ?? "";
    return {
      text: `Engine ${version} · ${status.ha.mode}${peer === "" ? "" : ` with ${peer}`} · ${status.ha.local_state}`,
      tone: "quiet",
      rejected: false,
    };
  }
  // No HA block and this box follows a main: the main did not choose it as
  // the standby, so it is running plain Kea beside a pair it is not in.
  // "single" would be true of the engine and wrong about the network.
  if (isReplica) return { text: "DHCP: not in the HA pair", tone: "quiet", rejected: false };
  return { text: `Engine ${version} · single`, tone: "quiet", rejected: false };
}

function EngineStatusLine({ line, canApply }: { line: EngineLine; canApply: boolean }) {
  const apply = useApplyDHCP();

  return (
    <div className="flex min-h-9 shrink-0 items-center gap-2.5 border-b border-border bg-card px-4 py-2">
      <span className="font-mono text-[9.5px] font-semibold tracking-[0.14em] whitespace-nowrap text-muted-foreground uppercase">
        Engine
      </span>
      <span
        aria-hidden="true"
        className={cn("size-[7px] shrink-0", line.tone === "bad" ? "bg-destructive" : "bg-primary")}
      />
      <output
        className={cn(
          "font-mono text-[11.5px] leading-snug",
          line.tone === "bad" ? "text-destructive" : "text-muted-foreground",
        )}
      >
        {line.text}
      </output>
      {/* Nothing retries a refused configuration on a timer, so this is the
          only way back — and it is offered exactly where the refusal is
          quoted. Still refused on a replica: the render is a config write. */}
      {line.rejected && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={!canApply || apply.isPending}
          onClick={() =>
            apply.mutate(undefined, {
              onError: (err) =>
                toast.error(err instanceof ApiError ? err.message : "Couldn't apply"),
            })
          }
        >
          {apply.isPending ? "Applying…" : "Apply again"}
        </Button>
      )}
    </div>
  );
}

// --- the scope form --------------------------------------------------------

/** Every field is a string: these are `<input>` values, and a number field
 * that is briefly empty while being retyped has to survive the round trip
 * through the form state. Conversion happens once, in `toInput` below. */
interface ScopeFormValues extends ClientOptionFields {
  name: string;
  cidr: string;
  pools: PoolRow[];
  gateway: string;
  domain: string;
  lease_seconds: string;
  dns_servers: string;
  enabled: boolean;
  /** The switch is "Ignore client identifier", which is this field's
   * opposite — see the control's comment. */
  match_client_id: boolean;
  reservations_only: boolean;
}

/** The Client options fields a scope and a class share, as form strings.
 * Both dialogs render them with ClientOptionSections and convert them with
 * the two functions below it. */
export interface ClientOptionFields {
  domain_search: string;
  ntp_servers: string;
  static_routes: { destination: string; router: string }[];
  options: { code: string; hex: string }[];
  next_server: string;
  server_hostname: string;
  boot_file: string;
}

/** One row of the Pools table. `class_id` is the select's value: "0" is
 * any client. */
interface PoolRow {
  start: string;
  end: string;
  class_id: string;
}

function v4(s: string): number | null {
  const b = parseIPv4(s.trim());
  return b && ((b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3]) >>> 0;
}

/** `a.b.c.d/n` as its first and last address, or null. */
function v4Range(cidr: string): [number, number] | null {
  const [addr, len, extra] = cidr.trim().split("/");
  const base = v4(addr);
  if (base === null || extra !== undefined || !/^\d{1,2}$/.test(len ?? "") || Number(len) > 32) {
    return null;
  }
  const size = 2 ** (32 - Number(len));
  const first = base - (base % size);
  return [first, first + size - 1];
}

/**
 * What is wrong with each pool row, in the board's words, or undefined.
 *
 * Only the four states the board names, and only once a row's addresses
 * parse: a half-typed address is not yet wrong. Everything else — the
 * network and broadcast addresses, an address that never parses — is the
 * server's to say, and its 422 lands on the row it names.
 *
 * `classes` is the current list (undefined while loading, when no class is
 * called unknown); `names` also remembers classes this dialog has seen and
 * the list has since lost.
 */
export function poolRowErrors(
  rows: readonly PoolRow[],
  cidr: string,
  classes: ReadonlySet<number> | undefined,
  names: ReadonlyMap<number, string>,
): (string | undefined)[] {
  const subnet = v4Range(cidr);
  const ranges: ([number, number] | null)[] = [];
  return rows.map((row, i) => {
    if (isBlankRow(row.start, row.end)) return undefined;
    const start = v4(row.start);
    const end = v4(row.end);
    const both = start !== null && end !== null;
    ranges[i] = both && start <= end ? [start, end] : null;
    if (subnet) {
      const outside = (a: number | null) => a !== null && (a < subnet[0] || a > subnet[1]);
      if (outside(start) || outside(end)) return `Not in subnet ${cidr.trim()}`;
    }
    if (both && start > end) return "Start is after end";
    const mine = ranges[i];
    if (mine) {
      for (let j = 0; j < i; j++) {
        const other = ranges[j];
        if (other && mine[0] <= other[1] && other[0] <= mine[1]) {
          return `Overlaps ${rows[j].start.trim()} – ${rows[j].end.trim()}`;
        }
      }
    }
    const id = Number(row.class_id);
    if (id !== 0 && classes !== undefined && !classes.has(id)) {
      return `Unknown class ${names.get(id) ?? id}`;
    }
    return undefined;
  });
}

/**
 * A mini-table row nobody has filled in at all. The `+` button appends an
 * empty one, so a form saved without ever typing into it is not an error —
 * it is a row the operator declined to use, and it is dropped on the way out
 * (see toInput). Half of one is a different thing and is refused, loudly:
 * an option with a code and no bytes is a configuration the engine will take
 * and nobody meant.
 */
function isBlankRow(a: string, b: string): boolean {
  return a.trim() === "" && b.trim() === "";
}

function halfFilled(ctx: z.RefinementCtx, index: number, field: string, what: string): void {
  ctx.addIssue({ code: "custom", path: [index, field], message: `Enter ${what}` });
}

/** The shared Client options fields' schema, spread into both forms'. */
export const clientOptionsShape = {
  domain_search: z.string(),
  ntp_servers: z.string(),
  static_routes: z
    .array(z.object({ destination: z.string(), router: z.string() }))
    .superRefine((rows, ctx) => {
      rows.forEach((row, index) => {
        if (isBlankRow(row.destination, row.router)) return;
        if (row.destination.trim() === "") halfFilled(ctx, index, "destination", "the destination");
        if (row.router.trim() === "") halfFilled(ctx, index, "router", "the router");
      });
    }),
  options: z.array(z.object({ code: z.string(), hex: z.string() })).superRefine((rows, ctx) => {
    rows.forEach((row, index) => {
      const code = row.code.trim();
      if (isBlankRow(code, row.hex)) return;
      if (code === "") halfFilled(ctx, index, "code", "the option code");
      else if (!/^\d+$/.test(code)) {
        ctx.addIssue({
          code: "custom",
          path: [index, "code"],
          message: "Option code must be a number",
        });
      }
      if (row.hex.trim() === "") halfFilled(ctx, index, "hex", "the value's bytes");
    });
  }),
  next_server: z.string(),
  server_hostname: z.string(),
  boot_file: z.string(),
};

/**
 * Client-side validation: a name, a subnet, and the pool rows — the board
 * draws their four states live, so they are checked here too (see
 * poolRowErrors). Everything else — the gateway inside the cidr, the option
 * codes the renderer already emits by name, the overlap with another enabled
 * scope — is a claim about other rows or about netip arithmetic, and the
 * server is the one that can make it. Its message lands as a toast rather
 * than being approximated here (see lib/schemas.ts's framing).
 */
function scopeFormSchema(
  classes: ReadonlySet<number> | undefined,
  names: ReadonlyMap<number, string>,
) {
  return z
    .object({
      name: requiredText("Name is required"),
      cidr: requiredText("Subnet is required"),
      pools: z
        .array(z.object({ start: z.string(), end: z.string(), class_id: z.string() }))
        .superRefine((rows, ctx) => {
          rows.forEach((row, index) => {
            if (isBlankRow(row.start, row.end)) return;
            if (row.start.trim() === "") halfFilled(ctx, index, "start", "the start");
            if (row.end.trim() === "") halfFilled(ctx, index, "end", "the end");
          });
        }),
      gateway: z.string(),
      domain: z.string(),
      lease_seconds: z
        .string()
        .refine(
          (v) => v.trim() === "" || /^\d+$/.test(v.trim()),
          "Lease time must be a whole number",
        ),
      dns_servers: z.string(),
      enabled: z.boolean(),
      ...clientOptionsShape,
      match_client_id: z.boolean(),
      reservations_only: z.boolean(),
    })
    .superRefine((v, ctx) => {
      if (!v.reservations_only && v.pools.every((p) => isBlankRow(p.start, p.end))) {
        ctx.addIssue({ code: "custom", path: ["pools"], message: "Add a pool" });
      }
      poolRowErrors(v.pools, v.cidr, classes, names).forEach((message, index) => {
        if (message !== undefined) {
          ctx.addIssue({ code: "custom", path: ["pools", index, "start"], message });
        }
      });
    });
}

function toFormValues(scope: DHCPScope | null): ScopeFormValues {
  return {
    name: scope?.name ?? "",
    cidr: scope?.cidr ?? "",
    // A new scope starts with one empty row, so the placeholders show
    // where the range goes.
    pools: scope
      ? scope.pools.map((p) => ({ start: p.start, end: p.end, class_id: String(p.class_id) }))
      : [{ start: "", end: "", class_id: "0" }],
    gateway: scope?.gateway ?? "",
    domain: scope?.domain ?? "",
    // 0 is "use the dhcp.lease_seconds setting", which is what an empty
    // box means — printing the sentinel would make it look chosen.
    lease_seconds: scope?.lease_seconds ? String(scope.lease_seconds) : "",
    dns_servers: scope?.dns_servers ?? "",
    enabled: scope?.enabled ?? true,
    ...clientOptionsToForm(scope),
    match_client_id: scope?.match_client_id ?? true,
    reservations_only: scope?.reservations_only ?? false,
  };
}

type ClientOptionsWire = Pick<
  DHCPScope,
  | "domain_search"
  | "ntp_servers"
  | "static_routes"
  | "options"
  | "next_server"
  | "server_hostname"
  | "boot_file"
>;

export function clientOptionsToForm(src: ClientOptionsWire | null): ClientOptionFields {
  return {
    domain_search: src?.domain_search ?? "",
    ntp_servers: src?.ntp_servers ?? "",
    static_routes: src?.static_routes ?? [],
    options: (src?.options ?? []).map((o) => ({ code: String(o.code), hex: o.hex })),
    next_server: src?.next_server ?? "",
    server_hostname: src?.server_hostname ?? "",
    boot_file: src?.boot_file ?? "",
  };
}

export function clientOptionsToInput(values: ClientOptionFields): ClientOptionsWire {
  return {
    domain_search: values.domain_search.trim(),
    ntp_servers: values.ntp_servers.trim(),
    // Blank rows only — the schema has already refused every half-filled
    // one, so nothing that reaches here is being silently discarded.
    static_routes: values.static_routes
      .filter((r) => !isBlankRow(r.destination, r.router))
      .map((r) => ({ destination: r.destination.trim(), router: r.router.trim() })),
    options: values.options
      .filter((o) => !isBlankRow(o.code, o.hex))
      .map((o) => ({ code: Number(o.code.trim()), hex: o.hex.trim() })),
    next_server: values.next_server.trim(),
    server_hostname: values.server_hostname.trim(),
    boot_file: values.boot_file.trim(),
  };
}

/** The request body. Every field is sent, on a create and on a merge alike:
 * this form owns all of them, so an omitted key would mean "keep what you
 * had" about a box the operator has just emptied on purpose. */
function toInput(values: ScopeFormValues): ScopeInput {
  return {
    name: values.name.trim(),
    cidr: values.cidr.trim(),
    // Blank rows are dropped like the other tables'; poolIndexes maps the
    // server's `pools[i]` back to the row it came from.
    pools: values.pools
      .filter((p) => !isBlankRow(p.start, p.end))
      .map((p) => ({ start: p.start.trim(), end: p.end.trim(), class_id: Number(p.class_id) })),
    gateway: values.gateway.trim(),
    domain: values.domain.trim(),
    lease_seconds: Number(values.lease_seconds.trim() || 0),
    enabled: values.enabled,
    dns_servers: values.dns_servers.trim(),
    ...clientOptionsToInput(values),
    match_client_id: values.match_client_id,
    reservations_only: values.reservations_only,
  };
}

/** The form row each sent pool came from: `pools[i]` in a refusal is the
 * i-th non-blank row. */
function poolIndexes(values: ScopeFormValues): number[] {
  return values.pools.flatMap((p, i) => (isBlankRow(p.start, p.end) ? [] : [i]));
}

export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <FormItem className="min-w-0 gap-1.5">
      <span className="flex items-baseline gap-2">
        <FormLabel className="text-[12.5px] leading-none font-medium">{label}</FormLabel>
        {hint !== undefined && (
          <span className="font-mono text-[9.5px] text-muted-foreground">{hint}</span>
        )}
      </span>
      <FormControl>{children}</FormControl>
      <FormMessage />
    </FormItem>
  );
}

/**
 * A labelled switch, in the one shape the form uses for all three.
 *
 * `aria-labelledby` against the visible text rather than a wrapping
 * `<label>`: base-ui's Switch is a span plus a hidden input, and a label
 * wrapping both names two controls with one word.
 */
function SwitchRow({
  label,
  checked,
  onChange,
}: {
  label: string;
  checked: boolean;
  onChange: (next: boolean) => void;
}) {
  const id = useId();
  return (
    <span className="flex items-center gap-2.5">
      <Switch size="sm" checked={checked} onCheckedChange={onChange} aria-labelledby={id} />
      <span id={id} className="text-[12.5px] leading-none font-medium">
        {label}
      </span>
    </span>
  );
}

/** The dialog's two tabs. The board calls them Network and Client options;
 * the values are what the form switches on. */
const TAB_NETWORK = "basics";
const TAB_OPTIONS = "options";

/**
 * Which fields live on the Client options tab, so a refusal can pull its own
 * tab into view. A message on a panel the operator is not looking at is a
 * Save that did nothing and said nothing — which is exactly what this form
 * did before the tabs existed.
 */
const OPTIONS_TAB_FIELDS = new Set<string>([
  "domain_search",
  "ntp_servers",
  "static_routes",
  "options",
  "next_server",
  "server_hostname",
  "boot_file",
  "match_client_id",
  "reservations_only",
]);

/** The tab a refusal is on, or null when everything the form disliked is
 * already in view. */
function tabForErrors(fields: readonly string[]): string | null {
  if (fields.length === 0) return null;
  if (fields.every((name) => OPTIONS_TAB_FIELDS.has(name))) return TAB_OPTIONS;
  return TAB_NETWORK;
}

/** One group on the Client options tab: a mono uppercase head over a rule,
 * and the fields under it. The board's one-line leads under each head are
 * sample text — the field labels are the copy (docs/dashboard.md carries the
 * explanation). */
export function OptionsSection({ head, children }: { head: string; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-2.5">
      <h3 className="border-b border-border-muted pb-1.5 font-mono text-[9.5px] leading-none font-semibold tracking-[0.14em] text-muted-foreground uppercase">
        {head}
      </h3>
      {children}
    </section>
  );
}

/**
 * The four Client options sections a scope and a class share: names and
 * time, routing, PXE boot and generic options. Read from the surrounding
 * <Form>, so either dialog's form can hold them. `inherit` is the class
 * dialog's: a blank field there means "the scope's", and every field says so.
 */
export function ClientOptionSections({ inherit = false }: { inherit?: boolean }) {
  const form = useFormContext<ClientOptionFields>();
  const hint = (example: string) => (inherit ? "inherit" : example);
  const routes = useFieldArray({ control: form.control, name: "static_routes" });
  const options = useFieldArray({ control: form.control, name: "options" });
  const errors = form.formState.errors;

  return (
    <>
      <OptionsSection head="Names &amp; time">
        <div className="grid gap-3.5 gap-x-6 sm:grid-cols-2">
          <FormField
            control={form.control}
            name="domain_search"
            render={({ field }) => (
              <Field label="Domain search list" hint="comma-separated">
                <Input
                  {...field}
                  placeholder={hint("office.e412.in, e412.in")}
                  autoComplete="off"
                  className="font-mono"
                />
              </Field>
            )}
          />
          <FormField
            control={form.control}
            name="ntp_servers"
            render={({ field }) => (
              <Field label="NTP servers" hint="addresses">
                <Input
                  {...field}
                  placeholder={hint("192.168.151.1")}
                  autoComplete="off"
                  className="font-mono"
                />
              </Field>
            )}
          />
        </div>
      </OptionsSection>

      <OptionsSection head="Routing">
        <MiniTable
          columns={["Destination", "Router"]}
          template="grid-cols-[1fr_1fr_64px]"
          noun="route"
          rowError={(index) =>
            errors.static_routes?.[index]?.destination?.message ??
            errors.static_routes?.[index]?.router?.message
          }
          rows={routes.fields.map((row, index) => (
            <Fragment key={row.id}>
              <FormField
                control={form.control}
                name={`static_routes.${index}.destination`}
                render={({ field, fieldState }) => (
                  <Input
                    {...field}
                    placeholder="10.8.0.0/24"
                    aria-label={`Destination ${index + 1}`}
                    aria-invalid={fieldState.error !== undefined}
                    className="font-mono"
                  />
                )}
              />
              <FormField
                control={form.control}
                name={`static_routes.${index}.router`}
                render={({ field, fieldState }) => (
                  <Input
                    {...field}
                    placeholder="192.168.151.254"
                    aria-label={`Router ${index + 1}`}
                    aria-invalid={fieldState.error !== undefined}
                    className="font-mono"
                  />
                )}
              />
            </Fragment>
          ))}
          onRemove={(index) => routes.remove(index)}
          onAdd={() => routes.append({ destination: "", router: "" })}
          addLabel="Add route"
        />
      </OptionsSection>

      <OptionsSection head="PXE boot">
        <div className="grid gap-3.5 sm:grid-cols-3">
          <FormField
            control={form.control}
            name="next_server"
            render={({ field }) => (
              <Field label="Next server">
                <Input
                  {...field}
                  placeholder={hint("192.168.151.5")}
                  autoComplete="off"
                  className="font-mono"
                />
              </Field>
            )}
          />
          <FormField
            control={form.control}
            name="server_hostname"
            render={({ field }) => (
              <Field label="Server hostname">
                <Input
                  {...field}
                  placeholder={hint("pxe.office.e412.in")}
                  autoComplete="off"
                  className="font-mono"
                />
              </Field>
            )}
          />
          <FormField
            control={form.control}
            name="boot_file"
            render={({ field }) => (
              <Field label="Boot file">
                <Input
                  {...field}
                  placeholder={hint("pxelinux.0")}
                  autoComplete="off"
                  className="font-mono"
                />
              </Field>
            )}
          />
        </div>
      </OptionsSection>

      <OptionsSection head="Generic options">
        <MiniTable
          columns={["Code", "Hex value"]}
          template="grid-cols-[72px_1fr_64px]"
          noun="option"
          rowError={(index) =>
            errors.options?.[index]?.code?.message ?? errors.options?.[index]?.hex?.message
          }
          rows={options.fields.map((row, index) => (
            <Fragment key={row.id}>
              <FormField
                control={form.control}
                name={`options.${index}.code`}
                render={({ field, fieldState }) => (
                  <Input
                    {...field}
                    placeholder="252"
                    aria-label={`Code ${index + 1}`}
                    aria-invalid={fieldState.error !== undefined}
                    className="font-mono"
                  />
                )}
              />
              <FormField
                control={form.control}
                name={`options.${index}.hex`}
                render={({ field, fieldState }) => (
                  <Input
                    {...field}
                    placeholder="687474703a2f2f7770"
                    aria-label={`Hex value ${index + 1}`}
                    aria-invalid={fieldState.error !== undefined}
                    className="font-mono"
                  />
                )}
              />
            </Fragment>
          ))}
          onRemove={(index) => options.remove(index)}
          onAdd={() => options.append({ code: "", hex: "" })}
          addLabel="Add option"
        />
      </OptionsSection>
    </>
  );
}

/**
 * The scope dialog's Client options tab: the shared sections, then the two
 * behaviour switches only a scope has.
 */
function ScopeOptions({ form }: { form: UseFormReturn<ScopeFormValues> }) {
  return (
    <div className="flex flex-col gap-5">
      <ClientOptionSections />

      <OptionsSection head="Behaviour">
        <div className="flex flex-wrap items-center gap-6">
          {/* Labelled for what switching it *on* does, which is the opposite
              of the stored column: `match_client_id` false is what cloned
              VMs sharing a client id need, and "Match client identifier ·
              off" is a double negative on screen. */}
          <FormField
            control={form.control}
            name="match_client_id"
            render={({ field }) => (
              <SwitchRow
                label="Ignore client identifier"
                checked={!field.value}
                onChange={(next) => field.onChange(!next)}
              />
            )}
          />
          <FormField
            control={form.control}
            name="reservations_only"
            render={({ field }) => (
              <SwitchRow
                label="Reservations only"
                checked={field.value}
                onChange={field.onChange}
              />
            )}
          />
        </div>
      </OptionsSection>
    </div>
  );
}

/** The repeating lists — Pools on Network, routes and options on Client
 * options — which are the same table with different columns. */
export function MiniTable({
  columns,
  template,
  noun,
  rows,
  rowError,
  errorId,
  onRemove,
  onAdd,
  addLabel,
}: {
  columns: string[];
  template: string;
  /** Names the row in its Remove button, so a panel with four of them does
   * not offer four controls all called "Remove". */
  noun: string;
  rows: ReactNode[];
  /** What the form refused about row `index`, if anything. A half-filled row
   * has to say so where it is, or Save reads as dead. */
  rowError: (index: number) => string | undefined;
  /** The id row `index`'s refusal renders under, for the row's inputs to
   * name in aria-describedby; see describedBy. */
  errorId?: (index: number) => string;
  onRemove: (index: number) => void;
  onAdd: () => void;
  addLabel: string;
}) {
  return (
    <div className="flex min-w-0 flex-col border border-border">
      <div
        className={cn(
          template,
          "grid gap-2.5 border-b border-border bg-muted px-2.5 py-1.5",
          "font-mono text-[9.5px] leading-none font-semibold tracking-[0.12em] text-muted-foreground uppercase",
        )}
      >
        {columns.map((column) => (
          <span key={column}>{column}</span>
        ))}
        <span />
      </div>
      {rows.map((row, index) => {
        const error = rowError(index);
        return (
          <div
            // The caller's rows are keyed Fragments (react-hook-form's own
            // field ids); the index is what addresses the same row in the
            // field array, and reordering is not offered.
            key={index}
            className={cn(
              "border-b border-border-muted",
              error !== undefined && "bg-card shadow-[inset_3px_0_0_var(--destructive)]",
            )}
          >
            <div className={cn(template, "grid items-center gap-2.5 px-2.5 py-1.5")}>
              {row}
              <span className="flex justify-end">
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  aria-label={`Remove ${noun} ${index + 1}`}
                  onClick={() => onRemove(index)}
                >
                  Remove
                </Button>
              </span>
            </div>
            {error !== undefined && (
              <p
                role="alert"
                id={errorId?.(index)}
                className="px-2.5 pb-1.5 font-mono text-[11px] text-destructive-foreground"
              >
                {error}
              </p>
            )}
          </div>
        );
      })}
      <span className="flex p-1">
        <Button type="button" size="sm" variant="ghost" onClick={onAdd}>
          <Plus />
          {addLabel}
        </Button>
      </span>
    </div>
  );
}

/** MiniTable's errorId and the aria-describedby its rows' inputs carry:
 * the row's refusal, when there is one, describes every input in the row. */
export function useRowErrorIds(rowError: (index: number) => string | undefined) {
  const base = useId();
  const errorId = (index: number) => `${base}-row-${index}-error`;
  const describedBy = (index: number) =>
    rowError(index) === undefined ? undefined : errorId(index);
  return { errorId, describedBy };
}

/**
 * The Pools table on the Network tab. `errors` is poolRowErrors' answer for
 * the rows as typed; a refusal the schema or the server pinned on a row
 * shows when there is no live one.
 */
function PoolsTable({
  form,
  errors: live,
  classes,
  names,
}: {
  form: UseFormReturn<ScopeFormValues>;
  errors: (string | undefined)[];
  classes: DHCPClass[] | undefined;
  names: ReadonlyMap<number, string>;
}) {
  const pools = useFieldArray({ control: form.control, name: "pools" });
  const errors = form.formState.errors.pools;
  const tableError = errors?.message ?? errors?.root?.message;
  const rowError = (index: number) =>
    live[index] ?? errors?.[index]?.start?.message ?? errors?.[index]?.end?.message;
  const { errorId, describedBy } = useRowErrorIds(rowError);

  return (
    <div className="flex min-w-0 flex-col gap-1.5 sm:col-span-2">
      <span className="text-[12.5px] leading-none font-medium">Pools</span>
      <MiniTable
        columns={["Start", "End", "Class"]}
        template="grid-cols-[1fr_1fr_150px_64px]"
        noun="pool"
        rowError={rowError}
        errorId={errorId}
        rows={pools.fields.map((row, index) => (
          <Fragment key={row.id}>
            <FormField
              control={form.control}
              name={`pools.${index}.start`}
              render={({ field, fieldState }) => (
                <Input
                  {...field}
                  placeholder="10.0.0.100"
                  autoComplete="off"
                  aria-label={`Pool ${index + 1} start`}
                  aria-invalid={live[index] !== undefined || fieldState.error !== undefined}
                  aria-describedby={describedBy(index)}
                  className="font-mono"
                />
              )}
            />
            <FormField
              control={form.control}
              name={`pools.${index}.end`}
              render={({ field, fieldState }) => (
                <Input
                  {...field}
                  placeholder="10.0.0.199"
                  autoComplete="off"
                  aria-label={`Pool ${index + 1} end`}
                  aria-invalid={live[index] !== undefined || fieldState.error !== undefined}
                  aria-describedby={describedBy(index)}
                  className="font-mono"
                />
              )}
            />
            <FormField
              control={form.control}
              name={`pools.${index}.class_id`}
              render={({ field }) => {
                const id = Number(field.value);
                const listed = id === 0 || (classes ?? []).some((c) => c.id === id);
                return (
                  <NativeSelect
                    {...field}
                    size="sm"
                    aria-label={`Pool ${index + 1} class`}
                    aria-invalid={live[index]?.startsWith("Unknown class") === true}
                    aria-describedby={describedBy(index)}
                  >
                    <NativeSelectOption value="0">any</NativeSelectOption>
                    {(classes ?? []).map((c) => (
                      <NativeSelectOption key={c.id} value={String(c.id)}>
                        {c.name}
                      </NativeSelectOption>
                    ))}
                    {/* A class this row names that the list no longer has
                        (or has not loaded yet) still needs an option, or the
                        select would show "any" for it. */}
                    {!listed && (
                      <NativeSelectOption value={field.value}>
                        {names.get(id) ?? field.value}
                      </NativeSelectOption>
                    )}
                  </NativeSelect>
                );
              }}
            />
          </Fragment>
        ))}
        onRemove={(index) => pools.remove(index)}
        onAdd={() => pools.append({ start: "", end: "", class_id: "0" })}
        addLabel="Add pool"
      />
      {tableError !== undefined && (
        <p role="alert" className="font-mono text-[11px] text-destructive-foreground">
          {tableError}
        </p>
      )}
      <p className="text-[11px] text-muted-foreground">
        Ranges inside the subnet, no overlaps. A pool with a class serves only that class.
      </p>
    </div>
  );
}

/**
 * Every class name this dialog has seen, so a class deleted elsewhere while
 * it is open can still be named in its row's error.
 */
function useClassNames(classes: DHCPClass[] | undefined): ReadonlyMap<number, string> {
  const merge = (prev: ReadonlyMap<number, string>) =>
    new Map([...prev, ...(classes ?? []).map((c) => [c.id, c.name] as const)]);
  const [names, setNames] = useState(() => merge(new Map()));
  const [mergedFor, setMergedFor] = useState(classes);
  // Adjusting state while rendering, React's documented pattern for state
  // derived from a changing prop.
  if (classes !== mergedFor) {
    setMergedFor(classes);
    setNames(merge);
  }
  return names;
}

/**
 * Create and edit in one dialog, as the board draws them: the fields are
 * the same and a second component for the same eighteen boxes would be the
 * drift this avoids. `scope` null is the create case.
 */
function ScopeDialog({
  scope,
  onCreated,
  onClose,
}: {
  scope: DHCPScope | null;
  onCreated: (id: number) => void;
  onClose: () => void;
}) {
  const create = useCreateScope();
  const update = useUpdateScope();
  const classes = useClasses().data;
  const names = useClassNames(classes);
  const classIds = useMemo(() => classes && new Set(classes.map((c) => c.id)), [classes]);
  const schema = useMemo(() => scopeFormSchema(classIds, names), [classIds, names]);
  const form = useForm<ScopeFormValues>({
    resolver: zodResolver(schema),
    defaultValues: toFormValues(scope),
  });
  const isPending = create.isPending || update.isPending;
  const [pools, cidr] = useWatch({ control: form.control, name: ["pools", "cidr"] });
  const poolErrors = poolRowErrors(pools, cidr, classIds, names);
  const poolsInvalid = poolErrors.some((e) => e !== undefined);
  // Controlled, because a refusal has to be able to change it: see
  // tabForErrors. Network is where a new scope starts — it is the half
  // without which there is no scope at all.
  const [tab, setTab] = useState<string>(TAB_NETWORK);

  function onSubmit(values: ScopeFormValues) {
    const body = toInput(values);
    const sent = poolIndexes(values);
    const onError = (err: unknown) => {
      // A refusal naming `pools[i]` belongs on that row, in the server's
      // words — and an overlap on its partner too, since the server names
      // the earlier row and the live check marks the later one. Everything
      // else is a toast.
      const match =
        err instanceof ApiError
          ? /^pools\[(\d+)\](?:.* overlaps pools\[(\d+)\])?/.exec(err.message)
          : null;
      const rows = (match ? [match[1], match[2]] : [])
        .filter((i) => i !== undefined)
        .map((i) => sent[Number(i)])
        .filter((row) => row !== undefined);
      if (err instanceof ApiError && rows.length > 0) {
        for (const row of rows) {
          form.setError(`pools.${row}.start`, { type: "server", message: err.message });
        }
        setTab(TAB_NETWORK);
        return;
      }
      toast.error(err instanceof ApiError ? err.message : "Couldn't save the scope");
    };
    if (scope) {
      update.mutate({ id: scope.id, ...body }, { onSuccess: onClose, onError });
      return;
    }
    create.mutate(body, {
      onSuccess: (created) => {
        onCreated(created.id);
        onClose();
      },
      onError,
    });
  }

  /** A field the form refused is no use behind a tab nobody is looking at. */
  function onInvalid(errors: FieldErrors<ScopeFormValues>) {
    const next = tabForErrors(Object.keys(errors));
    if (next !== null) setTab(next);
  }

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="sm:max-w-[720px]">
        <DialogHeader>
          <DialogTitle className="flex items-baseline gap-2.5">
            {scope ? `Edit scope · ${scope.name}` : "New scope"}
            {scope && <span className="font-mono text-[10px] font-normal">{scope.cidr}</span>}
          </DialogTitle>
          {/* The dialog names itself in its title; this is what a screen
              reader announces underneath it, and the two tabs are the one
              thing about the shape that is not obvious from the title. */}
          <DialogDescription className="sr-only">
            The subnet itself on Network, everything handed to its clients on Client options.
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
                <TabsTrigger value={TAB_NETWORK}>Network</TabsTrigger>
                <TabsTrigger value={TAB_OPTIONS}>Client options</TabsTrigger>
              </TabsList>
              <TabsContent value={TAB_NETWORK} className="flex flex-col gap-3.5 pt-3.5">
                <div className="grid gap-3.5 sm:grid-cols-2">
                  <FormField
                    control={form.control}
                    name="name"
                    render={({ field }) => (
                      <Field label="Name">
                        <Input {...field} placeholder="Office" autoComplete="off" />
                      </Field>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name="cidr"
                    render={({ field }) => (
                      <Field label="Subnet" hint="CIDR">
                        <Input
                          {...field}
                          placeholder="192.168.151.0/24"
                          autoComplete="off"
                          className="font-mono"
                        />
                      </Field>
                    )}
                  />
                  <PoolsTable form={form} errors={poolErrors} classes={classes} names={names} />
                  <FormField
                    control={form.control}
                    name="gateway"
                    render={({ field }) => (
                      <Field label="Gateway">
                        <Input
                          {...field}
                          placeholder="192.168.151.1"
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
                          placeholder="office.e412.in"
                          autoComplete="off"
                          className="font-mono"
                        />
                      </Field>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name="lease_seconds"
                    render={({ field }) => (
                      <Field label="Lease time (seconds)">
                        <Input
                          {...field}
                          placeholder="86400"
                          inputMode="numeric"
                          autoComplete="off"
                          className="font-mono"
                        />
                      </Field>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name="dns_servers"
                    render={({ field }) => (
                      <Field label="DNS servers (override, blank = automatic)">
                        <Input
                          {...field}
                          placeholder="192.168.151.2, 192.168.151.3"
                          autoComplete="off"
                          className="font-mono"
                        />
                      </Field>
                    )}
                  />
                </div>

                <FormField
                  control={form.control}
                  name="enabled"
                  render={({ field }) => (
                    <SwitchRow label="Enabled" checked={field.value} onChange={field.onChange} />
                  )}
                />
              </TabsContent>
              <TabsContent value={TAB_OPTIONS} className="pt-3.5">
                <ScopeOptions form={form} />
              </TabsContent>
            </Tabs>

            <DialogFooter>
              <DialogClose render={<Button type="button" size="sm" variant="ghost" />}>
                Cancel
              </DialogClose>
              <Button type="submit" size="sm" disabled={isPending || poolsInvalid}>
                {isPending ? "Saving…" : "Save"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

// --- the table -------------------------------------------------------------

function ScopeRow({
  scope,
  usage,
  reservations,
  managed,
  isNew,
  onEdit,
  onDelete,
}: {
  scope: DHCPScope;
  usage: { pool_size: number; leased: number } | undefined;
  reservations: number;
  managed: boolean;
  /** Created by this session, and not yet superseded by another create.
   * A scope sorts by id, so a new one lands at the bottom of a long list
   * with nothing saying which of them you just wrote. */
  isNew: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const update = useUpdateScope();
  const toScope = `${DHCP_RESERVATIONS_PATH}?scope=${scope.id}`;
  const first = scope.pools.at(0);

  return (
    <div
      data-testid="scope-row"
      className={cn(
        GRID,
        "border-b border-border-muted py-2",
        isNew && "bg-card shadow-[inset_3px_0_0_var(--primary)]",
      )}
    >
      <span className="flex min-w-0 items-center gap-2">
        <span
          className={cn(
            "truncate text-[13.5px] font-medium",
            scope.enabled ? "text-foreground" : "text-muted-foreground",
          )}
        >
          {scope.name}
        </span>
        {isNew && (
          <span className="shrink-0 border border-border px-1.5 py-0.5 font-mono text-[9px] leading-none tracking-[0.1em] text-muted-foreground uppercase">
            new
          </span>
        )}
      </span>
      <span className="truncate font-mono text-[12.5px]">{scope.cidr}</span>
      {/* The range truncates and `+N more` does not: a full range is
          already about as wide as the column. The title has every range. */}
      <span
        data-testid="scope-pool"
        className="flex min-w-0 font-mono text-[12.5px]"
        title={scope.pools.map((p) => `${p.start} – ${p.end}`).join(", ")}
      >
        {first && (
          <span className="min-w-0 truncate">
            {first.start}
            <span className="text-muted-foreground"> – </span>
            {first.end}
          </span>
        )}
        {scope.pools.length > 1 && (
          <span className="ml-2 shrink-0 text-muted-foreground">
            +{scope.pools.length - 1} more
          </span>
        )}
      </span>
      <span className="truncate text-right font-mono text-[12.5px]">
        {usage?.leased ?? 0}
        <span className="text-muted-foreground"> / {usage?.pool_size ?? 0}</span>
      </span>
      <span className="truncate font-mono text-[12.5px]">{scope.gateway}</span>
      <span className="truncate font-mono text-[12.5px]">{scope.domain}</span>
      <span className="flex min-w-0 items-center">
        {reservations > 0 ? (
          <Link
            to={toScope}
            className="truncate font-mono text-xs underline decoration-border underline-offset-[3px]"
          >
            {reservations} {reservations === 1 ? "reservation" : "reservations"}
          </Link>
        ) : (
          // A link, not a form: the Reservations page owns that form, and
          // this scope's filter is already in the URL when it opens.
          // `nativeButton={false}` is what tells base-ui the rendered element
          // is an anchor, so a replica's disabled state reaches the
          // accessibility tree as `aria-disabled` — an <a> has no `disabled`.
          <Button
            size="sm"
            variant="ghost"
            className="-ml-2"
            nativeButton={false}
            disabled={managed}
            render={<Link to={toScope} />}
          >
            <Plus />
            Add reservation
          </Button>
        )}
      </span>
      <span>
        <Switch
          size="sm"
          aria-label={`${scope.name} enabled`}
          checked={scope.enabled}
          disabled={managed || update.isPending}
          // The one PATCH that is a merge in the strict sense: the body is
          // this key and nothing else, so a scope edited in another tab
          // keeps every field this row is not showing.
          onCheckedChange={(enabled) =>
            update.mutate(
              { id: scope.id, enabled },
              {
                onError: (err) =>
                  toast.error(err instanceof ApiError ? err.message : "Couldn't save the scope"),
              },
            )
          }
        />
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

/**
 * DHCP › Scopes. One row per subnet this box hands out on, the engine's own
 * state above them, and the form that writes both.
 */
export function DHCPScopes() {
  const scopes = useScopes();
  const status = useDHCPStatus();
  const reservations = useReservationCounts();
  const managedBy = useManagedBy();
  const managed = managedBy !== "";
  const deleteScope = useDeleteScope();

  const [dialogFor, setDialogFor] = useState<DHCPScope | null | undefined>(undefined);
  const [deleteTarget, setDeleteTarget] = useState<DHCPScope | null>(null);
  // Marks the row a create just produced, and nothing takes it down but the
  // next create or leaving the screen — a badge that faded on a timer would
  // be gone by the time an operator looked back at the table.
  const [createdId, setCreatedId] = useState<number | null>(null);

  const line = engineLine(status.data, managed);
  const rows = scopes.data ?? [];
  const disabled = rows.filter((s) => !s.enabled).length;
  const usage = new Map(status.data?.scopes.map((s) => [s.id, s]));

  let body: ReactNode;
  if (scopes.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (scopes.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load scopes</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (rows.length === 0) {
    body = (
      <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
        <p className="font-heading text-sm font-semibold">No scopes yet</p>
        <p className="text-sm text-muted-foreground">The engine hands out no addresses.</p>
      </div>
    );
  } else {
    body = rows.map((scope) => (
      <ScopeRow
        key={scope.id}
        scope={scope}
        usage={usage.get(scope.id)}
        reservations={reservations.get(scope.id) ?? 0}
        managed={managed}
        isNew={scope.id === createdId}
        onEdit={() => setDialogFor(scope)}
        onDelete={() => setDeleteTarget(scope)}
      />
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-x-auto">
      {line && <EngineStatusLine line={line} canApply={!managed} />}

      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2.5">
        <span className="font-heading text-[13.5px] leading-none font-semibold tracking-tight">
          Scopes
        </span>
        {disabled > 0 && (
          <span className="font-mono text-[10.5px] text-muted-foreground">{disabled} disabled</span>
        )}
        <div className="ml-auto flex items-center gap-3">
          {managed && <ManagedNotice />}
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={managed}
            onClick={() => setDialogFor(null)}
          >
            <Plus />
            New scope
          </Button>
        </div>
      </div>

      {scopes.isError && scopes.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="scopes"
            onRetry={() => void scopes.refetch()}
            isRetrying={scopes.isFetching}
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
        <span>Subnet</span>
        <span>Pool</span>
        <span className="text-right">Leased</span>
        <span>Gateway</span>
        <span>DNS suffix</span>
        <span>Reservations</span>
        <span>Enabled</span>
        <span className="text-right">Actions</span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      {dialogFor !== undefined && (
        <ScopeDialog
          scope={dialogFor}
          onCreated={setCreatedId}
          onClose={() => setDialogFor(undefined)}
        />
      )}

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title="Delete this scope?"
        description={
          deleteTarget && (
            <>
              <span className="font-medium text-foreground">{deleteTarget.name}</span> and its
              reservations stop being handed out.
            </>
          )
        }
        isPending={deleteScope.isPending}
        onConfirm={() =>
          deleteTarget &&
          deleteScope.mutate(deleteTarget.id, {
            onSuccess: () => setDeleteTarget(null),
            onError: (err) =>
              toast.error(err instanceof ApiError ? err.message : "Couldn't delete the scope"),
          })
        }
      />
    </div>
  );
}

/**
 * How many reservations each scope holds, for the column that links to
 * them. Reads the same query the Reservations page does — react-query
 * dedupes, so this is a subscription rather than a second fetch, and a
 * reservation added there shows up here without a reload.
 */
function useReservationCounts(): Map<number, number> {
  const { data } = useReservations();
  const counts = new Map<number, number>();
  for (const r of data ?? []) counts.set(r.scope_id, (counts.get(r.scope_id) ?? 0) + 1);
  return counts;
}
