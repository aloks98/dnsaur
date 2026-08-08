import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Lock, Pencil, Plus, Trash2, TriangleAlert, X } from "lucide-react";
import { toast } from "sonner";
import { useForm, type Control } from "react-hook-form";
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
  Form,
  FormControl,
  FormField,
  FormItem,
  FormMessage,
  Input,
  NativeSelect,
  NativeSelectOption,
  Skeleton,
  type BadgeProps,
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { Zone } from "../../api/types";
import { useCreateZone, useDeleteZone, useZoneRecords, useZones } from "../../hooks/use-zones";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { relativeTime } from "../../lib/format";

// A category tag, not a verdict. Exact mapping from the artboard —
// deliberately not "each type gets a fresh colour": stub shares its badge
// with the neutral `secondary` variant rather than getting one of its own.
const TYPE_VARIANT: Record<Zone["type"], NonNullable<BadgeProps["variant"]>> = {
  primary: "primary-light",
  secondary: "info-light",
  stub: "secondary",
  forwarder: "warning-light",
  internal: "secondary",
};

/**
 * The types the create row's select actually offers. `secondary` is not
 * merely unimplemented — investigation triggered by this page's own
 * `primaries` finding (see PrimariesField below) turned up that
 * answer.go's forwarder/stub fall-through list is missing `secondary`
 * entirely, so a secondary zone with no transfer mechanism behind it (no
 * transfers exist before #72) gets served like a primary: authoritative
 * NXDOMAIN for every name but its apex NS, for a domain the user actually
 * owns. The API now 400s anything but `primary` ("only primary zones are
 * supported", commit 6cdbf85) — this list is that restriction, not an
 * arbitrary one, and it is meant to widen back to the four below the
 * moment #72 (zone transfers, Milestone D) ships. A single-option select
 * is honest about what Milestone A can actually do; four options where
 * three 400 is not.
 */
const CREATABLE_TYPES = ["primary"] as const satisfies readonly Zone["type"][];

/** Mirrors the server's own check (internal/api/zones_handlers.go's
 * normalizeZoneName) closely enough that a name accepted here round-trips —
 * lowercasing and the trailing-dot strip happen server-side regardless, so
 * this only has to catch what would otherwise be a wasted request: blank,
 * whitespace/path characters, or an empty label (e.g. "e412..in"). */
const zoneNameSchema = z
  .string()
  .trim()
  .refine((value) => {
    const name = value.replace(/\.$/, "");
    if (name === "" || /[ \t\r\n/\\]/.test(name)) return false;
    return !name.split(".").some((label) => label === "");
  }, "Enter a domain name, e.g. example.com");

const addZoneSchema = z.object({
  name: zoneNameSchema,
  type: z.enum(CREATABLE_TYPES),
  // Free text, unvalidated — see the comment beside the primaries field for
  // why it is collected but not sent anywhere yet.
  primaries: z.string(),
});
// Exported so list.test.tsx can build a standalone `useForm<AddZoneValues>`
// harness for PrimariesField/CreateRowHint below — see their own comments
// for why that's currently the only way to exercise the type !== "primary"
// branch at all.
export type AddZoneValues = z.infer<typeof addZoneSchema>;
const ADD_ZONE_DEFAULTS: AddZoneValues = { name: "", type: "primary", primaries: "" };

/** One declaration of the column geometry, shared by the header, the create
 * row and every zone row — matching the artboard's grid exactly. */
const GRID = "grid grid-cols-[1fr_108px_168px_116px_84px_116px_92px] items-center gap-3.5 px-4";

/** "17 days" — the expired-secondary warning's exact wording. lib/format.ts's
 * relativeTime abbreviates ("17d ago"); this line spells the unit out, so it
 * gets its own tiny formatter rather than a flag on the shared one. Only
 * ever fed a real refreshed_at (secondary zones only), so it doesn't need
 * relativeTime's "never"/zero-sentinel handling. */
function daysAgo(epochMs: number): string {
  const days = Math.max(0, Math.floor((Date.now() - epochMs) / 86_400_000));
  return `${days} ${days === 1 ? "day" : "days"}`;
}

/** A secondary past its SOA expiry is a stronger claim than "disabled": the
 * zone may still be `enabled`, but nothing behind it can answer for it
 * anymore. Only secondary zones carry `expires_at` at all (primary/stub/
 * forwarder/internal leave it 0 — see api/types.ts's Zone doc). */
function isExpiredSecondary(zone: Zone): boolean {
  return zone.type === "secondary" && zone.expires_at !== 0 && zone.expires_at < Date.now();
}

function zoneStatus(zone: Zone, expired: boolean): { dot: string; text: string; label: string } {
  if (expired) {
    return {
      dot: "bg-destructive",
      text: "font-semibold text-destructive-foreground",
      label: "Not answering",
    };
  }
  if (zone.enabled) {
    return { dot: "bg-success", text: "", label: "Enabled" };
  }
  return { dot: "bg-muted-foreground", text: "text-muted-foreground", label: "Disabled" };
}

/** The Records column: a per-zone request rather than a field on Zone
 * itself (the API has no record-count field — see api/types.ts). Same
 * shape as filtering/groups-clients.tsx's GroupListsCell, which reads a
 * per-group query the same way for the same reason: the list endpoint
 * doesn't carry it, and the count is still worth showing without turning
 * this page into a records browser. Grouped with `.toLocaleString()`,
 * unlike Serial beside it — the artboard draws that distinction
 * deliberately (a serial is an opaque counter, not a quantity). */
function RecordsCell({ zoneId }: { zoneId: number }) {
  const records = useZoneRecords(zoneId);
  return (
    <span className="text-right font-mono text-sm">
      {records.data ? records.data.length.toLocaleString() : "—"}
    </span>
  );
}

/**
 * The create row's STATUS-column cell: a primaries input for secondary/
 * stub/forwarder zones, an empty cell for primary. Milestone A's create/
 * patch endpoints (zoneCreate/zonePatch in zones_handlers.go) have no
 * `primaries` field at all yet, so nothing typed here is sent — see
 * AddZoneRow's onSubmit.
 *
 * Split out into its own component, taking `type` as a plain prop rather
 * than reading `form.watch("type")` itself, so this branch stays directly
 * testable. The type select above currently offers only "primary" (see
 * CREATABLE_TYPES) — nothing a real user can click ever drives `type` here
 * away from "primary", so in the running app this always renders the empty
 * cell. list.test.tsx renders this component on its own, with `type` set
 * directly, which is currently the *only* way `type !== "primary"` is
 * exercised anywhere. #72 (zone transfers, Milestone D) widens the select
 * back out and makes this reachable for real.
 */
export function PrimariesField({
  type,
  control,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
}) {
  if (type !== "secondary" && type !== "stub" && type !== "forwarder") return <span />;
  return (
    <FormField
      control={control}
      name="primaries"
      render={({ field }) => (
        <FormItem>
          <FormControl>
            <Input
              {...field}
              aria-label="Primary servers"
              placeholder="192.168.150.1:53"
              autoComplete="off"
              className="font-mono"
            />
          </FormControl>
        </FormItem>
      )}
    />
  );
}

/**
 * The create row's hint line: column 1 normally ("SOA defaults are filled
 * in."), column 3 once PrimariesField above is showing ("Primary servers,
 * comma separated."), matching the artboard exactly rather than pinning
 * both to column 1. A name validation error takes over column 1 when there
 * is one.
 *
 * Same reachability note as PrimariesField: `type` is a prop rather than
 * read from the form, so list.test.tsx can drive the non-primary branch
 * directly even though the select above cannot produce one today.
 */
export function CreateRowHint({
  type,
  control,
  nameError,
}: {
  type: Zone["type"];
  control: Control<AddZoneValues>;
  nameError: string | undefined;
}) {
  const showPrimaries = type === "secondary" || type === "stub" || type === "forwarder";
  return (
    <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
      <span>
        {nameError ? (
          <FormField control={control} name="name" render={() => <FormMessage />} />
        ) : !showPrimaries ? (
          <span className="text-muted-foreground">SOA defaults are filled in.</span>
        ) : null}
      </span>
      <span />
      <span>
        {showPrimaries && (
          <span className="text-muted-foreground">Primary servers, comma separated.</span>
        )}
      </span>
      <span />
      <span />
      <span />
      <span />
    </div>
  );
}

/**
 * The create row: one persistent band, not a dialog — matching the create
 * affordance already used for groups and clients (see
 * filtering/groups-clients.tsx's AddGroupRow). Closes on success rather
 * than resetting and staying open the way the client row does — adding a
 * zone is a one-off act, not "add four in a row" the way DNS records are.
 */
function AddZoneRow({ onClose }: { onClose: () => void }) {
  const createZone = useCreateZone();
  const form = useForm<AddZoneValues>({
    resolver: zodResolver(addZoneSchema),
    defaultValues: ADD_ZONE_DEFAULTS,
  });

  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  const type = form.watch("type");
  const nameError = form.formState.errors.name?.message;

  function onSubmit(values: AddZoneValues) {
    createZone.mutate(
      {
        name: values.name.trim(),
        // Omitted at the default: handleZoneCreate already treats a
        // missing type as "primary" (zones_handlers.go), so there is no
        // need to say it explicitly in the common case.
        ...(values.type === "primary" ? {} : { type: values.type }),
        // primaries is deliberately NOT sent — see PrimariesField's own
        // comment above for why.
      },
      {
        onSuccess: () => {
          toast.success("Zone added");
          onClose();
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the zone"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="add-zone-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GRID, "py-2.5")}>
          <FormField
            control={form.control}
            name="name"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Zone name"
                    placeholder="home.lan"
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="type"
            render={({ field }) => (
              <FormItem>
                <FormControl>
                  {/* Milestone A only: `primary` is the sole option — see
                      CREATABLE_TYPES for why (a real bug this page's
                      `primaries` finding surfaced, not just "unbuilt").
                      Widens back to primary/secondary/stub/forwarder the
                      moment #72 (zone transfers, Milestone D) ships; the
                      control stays here now rather than disappearing and
                      reappearing later. */}
                  {CREATABLE_TYPES.length === 1 ? (
                    // A select with one option is a control that cannot be
                    // used — it invites a click and then does nothing. While
                    // `primary` is the only creatable type this cell just
                    // states it, matching the row's other server-decided
                    // cells. The select comes back, populated, with #72.
                    <span className="font-mono text-sm text-muted-foreground">
                      {CREATABLE_TYPES[0]}
                      <input type="hidden" {...field} />
                    </span>
                  ) : (
                    <NativeSelect {...field} aria-label="Zone type">
                      {CREATABLE_TYPES.map((t) => (
                        <NativeSelectOption key={t} value={t}>
                          {t}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  )}
                </FormControl>
              </FormItem>
            )}
          />
          <PrimariesField type={type} control={form.control} />
          <span className="text-right font-mono text-sm text-muted-foreground">—</span>
          <span className="text-right font-mono text-sm text-muted-foreground">—</span>
          <span className="text-right font-mono text-xs text-muted-foreground">—</span>
          <div className="flex items-center justify-end gap-1">
            <Button type="submit" size="sm" disabled={createZone.isPending}>
              {createZone.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the add-zone row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>

        <CreateRowHint type={type} control={form.control} nameError={nameError} />
      </form>
    </Form>
  );
}

/**
 * One zone. `internal` zones are read-only — no delete, no edit — because
 * they are the RFC 6303 zones that stop junk queries (localhost, the
 * reverse-mapping arpa zones, …) reaching the root servers, seeded in a
 * later milestone rather than managed by hand. The Actions cell says so
 * outright (`BUILT-IN`) rather than merely omitting buttons, so an admin
 * scanning the column never wonders whether the buttons failed to render.
 */
function ZoneRow({ zone, onDelete }: { zone: Zone; onDelete: () => void }) {
  const isInternal = zone.type === "internal";
  const expired = isExpiredSecondary(zone);
  const status = zoneStatus(zone, expired);

  return (
    <div
      data-testid="zone-row"
      data-slot="zone-row"
      className={cn(
        "border-b border-border-muted",
        isInternal && "bg-muted/30 text-muted-foreground",
        // The expired warning below gets its own left-edge mark and tint —
        // the same "this needs a human" treatment idle filter lists get in
        // filtering/lists.tsx, on the row that contains the reason.
        expired && "bg-destructive/5 shadow-[inset_3px_0_0_var(--destructive)]",
      )}
    >
      <div className={cn(GRID, "py-2.5")}>
        {isInternal ? (
          <span className="flex min-w-0 items-center gap-1.5 truncate text-sm" title={zone.name}>
            <Lock aria-hidden="true" className="size-3.5 shrink-0" />
            <span className="truncate">{zone.name}</span>
          </span>
        ) : (
          // Zone detail (Task 12) owns SOA fields and records — see
          // pages/zones/detail.tsx.
          <Link
            to={`/zones/${zone.id}`}
            className="truncate text-sm font-medium text-primary underline decoration-2 underline-offset-[3px]"
            title={zone.name}
          >
            {zone.name}
          </Link>
        )}
        <span>
          <Badge variant={TYPE_VARIANT[zone.type]}>{zone.type}</Badge>
        </span>
        <span className="flex items-center gap-2">
          <span aria-hidden className={cn("size-1.5 shrink-0", status.dot)} />
          <span className={cn("text-sm", status.text)}>{status.label}</span>
        </span>
        {/* No thousands grouping — a serial is an opaque counter, not a
            quantity, and the artboard draws that line deliberately. */}
        <span className="text-right font-mono text-sm">{zone.soa_serial}</span>
        <RecordsCell zoneId={zone.id} />
        <span className="text-right font-mono text-xs">{relativeTime(zone.modified_at)}</span>
        <span className="flex items-center justify-end">
          {isInternal ? (
            <span className="font-mono text-xs tracking-widest text-muted-foreground uppercase">
              Built-in
            </span>
          ) : (
            <div className="flex items-center gap-1">
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label={`Edit ${zone.name}`}
                render={<Link to={`/zones/${zone.id}`} />}
              >
                <Pencil />
              </Button>
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label={`Delete ${zone.name}`}
                onClick={onDelete}
              >
                <Trash2 />
              </Button>
            </div>
          )}
        </span>
      </div>

      {expired && (
        <div className="flex items-center gap-3 px-4 pb-2.5">
          <span className="shrink-0 font-mono text-[9.5px] font-semibold tracking-[0.14em] text-destructive-foreground uppercase">
            Expired
          </span>
          <span className="min-w-0 flex-1 text-pretty text-xs text-muted-foreground">
            Transfer from {zone.primaries || "unknown"} last succeeded {daysAgo(zone.refreshed_at)}{" "}
            ago, past the SOA expiry.
          </span>
          {/* No API for zone transfers in this milestone (they arrive in
              Milestone D) — rendered to match the design, deliberately not
              wired to anything. */}
          <Button type="button" size="sm" variant="outline" className="shrink-0">
            Retry transfer
          </Button>
        </div>
      )}
    </div>
  );
}

/**
 * Zones — the authoritative DNS zones this instance serves, replacing the
 * old flat Local DNS override table (see api/types.ts's Zone doc). A name
 * inside an enabled zone is answered or refused, never forwarded.
 *
 * GET/POST/DELETE /zones via use-zones.ts's hooks (no inline edit here —
 * enabling/disabling and SOA changes live on the zone detail page, Task 12).
 */
export function ZonesList() {
  const zones = useZones();
  const deleteZone = useDeleteZone();

  const [addOpen, setAddOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Zone | null>(null);

  const all = useMemo(() => zones.data ?? [], [zones.data]);

  function onConfirmDelete() {
    if (!deleteTarget) return;
    const target = deleteTarget;
    deleteZone.mutate(target.id, {
      onSuccess: () => {
        toast.success("Zone deleted");
        setDeleteTarget(null);
      },
      onError: () => toast.error(`Couldn't delete ${target.name}`),
    });
  }

  let body: ReactNode;
  if (zones.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (zones.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load zones</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (all.length === 0) {
    body = (
      <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
        <p className="font-heading text-sm font-semibold">No zones yet</p>
        <p className="text-sm text-muted-foreground">Everything is forwarded upstream.</p>
        {!addOpen && (
          <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
            <Plus />
            New zone
          </Button>
        )}
      </div>
    );
  } else {
    body = all.map((zone) => (
      <ZoneRow key={zone.id} zone={zone} onDelete={() => setDeleteTarget(zone)} />
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border bg-card px-4 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {all.length} {all.length === 1 ? "zone" : "zones"}
        </span>
        {!addOpen && (
          <Button type="button" size="sm" className="ml-auto" onClick={() => setAddOpen(true)}>
            <Plus />
            New zone
          </Button>
        )}
      </div>

      {zones.isError && zones.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="zones"
            onRetry={() => void zones.refetch()}
            isRetrying={zones.isFetching}
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
        <span>Name</span>
        <span>Type</span>
        <span>Status</span>
        <span className="text-right">Serial</span>
        <span className="text-right">Records</span>
        <span className="text-right">Modified</span>
        <span className="text-right">Actions</span>
      </div>

      {addOpen && <AddZoneRow onClose={() => setAddOpen(false)} />}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this zone?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget && (
                <>
                  <span className="font-medium text-foreground">{deleteTarget.name}</span> and its
                  records will stop being served immediately.
                </>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
              onClick={onConfirmDelete}
              disabled={deleteZone.isPending}
            >
              {deleteZone.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
