import { useState, type ReactNode } from "react";
import { useSearchParams } from "react-router";
import { Plus, TriangleAlert, X } from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertTitle,
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
} from "@e412/rnui-react";
import { ApiError } from "../../api/client";
import type { DHCPReservation, DHCPScope } from "../../api/types";
import {
  useCreateReservation,
  useDeleteReservation,
  useReservations,
  useScopes,
  useUpdateReservation,
} from "../../hooks/use-dhcp";
import { useManagedBy } from "../../hooks/use-sync";
import { ManagedNotice } from "../../components/managed-notice";
import { StaleDataAlert } from "../../components/stale-data-alert";
import { ConfirmDeleteDialog } from "../dialogs";
import { canonicalMAC, macSchema } from "../../lib/mac";
import { requiredText } from "../../lib/schemas";

/** The board's six columns, head, rows and the inline form alike. */
const GRID = "grid grid-cols-[130px_180px_190px_1fr_1fr_104px] gap-3.5 px-4";

/** The query the Scopes page's reservations column sets. */
const SCOPE_PARAM = "scope";

/** An empty comment. The app's own "nothing here" glyph. */
const NO_COMMENT = "—";

const reservationSchema = z.object({
  scope_id: z.string(),
  ip: requiredText("Address is required"),
  mac: macSchema,
  hostname: z.string(),
  comment: z.string(),
});
type ReservationValues = z.infer<typeof reservationSchema>;

/**
 * Create and edit as one highlighted row in the table's own grid, as the
 * board draws it — not a dialog. A reservation is five short values and the
 * row it will become is on screen behind it; a modal would hide the list
 * that answers "is this address already taken?".
 *
 * `target` null is the create case. The scope select is offered only then:
 * `scope_id` is fixed once created (the server refuses a PATCH that moves
 * one, since an address validated against one subnet must not be carried
 * into another), so on an edit the scope is text.
 */
function ReservationForm({
  target,
  scopes,
  defaultScopeId,
  onClose,
}: {
  target: DHCPReservation | null;
  scopes: DHCPScope[];
  defaultScopeId: number;
  onClose: () => void;
}) {
  const create = useCreateReservation();
  const update = useUpdateReservation();
  const form = useForm<ReservationValues>({
    resolver: zodResolver(reservationSchema),
    defaultValues: {
      scope_id: String(target?.scope_id ?? defaultScopeId),
      ip: target?.ip ?? "",
      mac: target?.mac ?? "",
      hostname: target?.hostname ?? "",
      comment: target?.comment ?? "",
    },
  });
  const isPending = create.isPending || update.isPending;

  function onSubmit(values: ReservationValues) {
    const onError = (err: unknown) =>
      toast.error(err instanceof ApiError ? err.message : "Couldn't save the reservation");
    const body = {
      // Canonical lowercase colon form, which is how the server stores it:
      // sending "AA-BB-CC-DD-EE-FF" would be accepted and come back spelled
      // differently, so the row would appear to change under the operator.
      mac: canonicalMAC(values.mac),
      ip: values.ip.trim(),
      hostname: values.hostname.trim(),
      comment: values.comment.trim(),
    };
    if (target) {
      update.mutate({ id: target.id, ...body }, { onSuccess: onClose, onError });
      return;
    }
    create.mutate({ scope_id: Number(values.scope_id), ...body }, { onSuccess: onClose, onError });
  }

  const scopeName = scopes.find((s) => s.id === target?.scope_id)?.name ?? "";

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="reservation-form"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GRID, "items-end py-2.5")}>
          {target ? (
            <span className="self-center truncate text-[12.5px]">{scopeName}</span>
          ) : (
            <FormField
              control={form.control}
              name="scope_id"
              render={({ field }) => (
                <FormItem className="min-w-0">
                  <FormControl>
                    <NativeSelect {...field} aria-label="Scope">
                      {scopes.map((s) => (
                        <NativeSelectOption key={s.id} value={String(s.id)}>
                          {s.name}
                        </NativeSelectOption>
                      ))}
                    </NativeSelect>
                  </FormControl>
                </FormItem>
              )}
            />
          )}
          <FormField
            control={form.control}
            name="ip"
            render={({ field }) => (
              <FormItem className="min-w-0">
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Address"
                    placeholder="192.168.150.50"
                    autoComplete="off"
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="mac"
            render={({ field, fieldState }) => (
              <FormItem className="min-w-0">
                <FormControl>
                  <Input
                    {...field}
                    aria-label="MAC"
                    placeholder="aa:bb:cc:dd:ee:ff"
                    autoComplete="off"
                    aria-invalid={fieldState.error !== undefined}
                    className="font-mono"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="hostname"
            render={({ field }) => (
              <FormItem className="min-w-0">
                <FormControl>
                  <Input
                    {...field}
                    aria-label="Hostname"
                    placeholder="printer"
                    autoComplete="off"
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name="comment"
            render={({ field }) => (
              <FormItem className="min-w-0">
                <FormControl>
                  <Input {...field} aria-label="Comment" autoComplete="off" />
                </FormControl>
              </FormItem>
            )}
          />
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={isPending}>
              {isPending ? "Saving…" : target ? "Save" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the reservation form"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>
        <div className={cn(GRID, "items-start pb-2.5")}>
          <span />
          <span className="text-xs text-pretty text-muted-foreground">
            <FormField control={form.control} name="ip" render={() => <FormMessage />} />
          </span>
          <span className="text-xs text-pretty text-muted-foreground">
            <FormField control={form.control} name="mac" render={() => <FormMessage />} />
          </span>
          <span />
          <span />
          <span />
        </div>
      </form>
    </Form>
  );
}

function ReservationRow({
  reservation,
  scopeName,
  managed,
  onEdit,
  onDelete,
}: {
  reservation: DHCPReservation;
  scopeName: string;
  managed: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  return (
    <div
      data-testid="reservation-row"
      className={cn(GRID, "items-center border-b border-border-muted py-1.5")}
    >
      <span className="truncate text-[12.5px]">{scopeName}</span>
      <span className="truncate font-mono text-[13px]">{reservation.ip}</span>
      <span className="truncate font-mono text-[13px] text-muted-foreground">
        {reservation.mac}
      </span>
      <span className="truncate text-[13px] font-medium">{reservation.hostname}</span>
      <span className="truncate text-[12.5px] text-muted-foreground">
        {reservation.comment || NO_COMMENT}
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
 * DHCP › Reservations. Every fixed address on the box, across every scope.
 *
 * The scope filter is in the URL rather than in local state because the
 * Scopes page links here with it already set — its reservations column is
 * how most visits to this screen start — so the filter has to survive being
 * arrived at, bookmarked and reloaded. `replace` keeps a filter change out
 * of the back stack: it narrows a listing, it does not navigate.
 */
export function DHCPReservations() {
  const reservations = useReservations();
  const scopes = useScopes();
  const managedBy = useManagedBy();
  const managed = managedBy !== "";
  const deleteReservation = useDeleteReservation();
  const [params, setParams] = useSearchParams();

  const [formFor, setFormFor] = useState<DHCPReservation | null | undefined>(undefined);
  const [deleteTarget, setDeleteTarget] = useState<DHCPReservation | null>(null);

  const allScopes = scopes.data ?? [];
  // An unknown or unparseable `?scope=` shows everything rather than an
  // empty table: the parameter narrows a listing, and a stale bookmark
  // should not read as "this box has no reservations".
  const raw = Number(params.get(SCOPE_PARAM));
  const scopeFilter = allScopes.some((s) => s.id === raw) ? raw : 0;
  const rows = (reservations.data ?? []).filter(
    (r) => scopeFilter === 0 || r.scope_id === scopeFilter,
  );
  const names = new Map(allScopes.map((s) => [s.id, s.name]));

  function setScopeFilter(next: string) {
    const params_ = new URLSearchParams(params);
    if (next === "0") params_.delete(SCOPE_PARAM);
    else params_.set(SCOPE_PARAM, next);
    setParams(params_, { replace: true });
  }

  let body: ReactNode;
  if (reservations.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-7 w-full" />
        ))}
      </div>
    );
  } else if (reservations.data === undefined) {
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load reservations</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (rows.length === 0) {
    // Two different emptinesses. "No reservations yet" under a filter that
    // is hiding four of them is a lie, and the way out of it is the filter
    // rather than the form — so the action clears the filter.
    const filtered = scopeFilter !== 0;
    body = (
      <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
        <p className="font-heading text-sm font-semibold">
          {filtered ? "No reservations in this scope" : "No reservations yet"}
        </p>
        {filtered ? (
          <Button type="button" size="sm" variant="outline" onClick={() => setScopeFilter("0")}>
            All scopes
          </Button>
        ) : (
          <p className="text-sm text-muted-foreground">Every device takes its turn in the pool.</p>
        )}
      </div>
    );
  } else {
    body = rows.map((reservation) => (
      <ReservationRow
        key={reservation.id}
        reservation={reservation}
        scopeName={names.get(reservation.scope_id) ?? ""}
        managed={managed}
        onEdit={() => setFormFor(reservation)}
        onDelete={() => setDeleteTarget(reservation)}
      />
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-x-auto">
      <div className="flex shrink-0 items-center gap-2.5 border-b border-border px-4 py-2.5">
        <span className="font-heading text-[13.5px] leading-none font-semibold tracking-tight">
          Reservations
        </span>
        {/* The chrome cell above keeps the total; this one counts what is on
            screen, and says which of the two it is whenever they differ. */}
        <span className="font-mono text-[10.5px] text-muted-foreground">
          {scopeFilter === 0
            ? `${rows.length} ${rows.length === 1 ? "reservation" : "reservations"}`
            : `${rows.length} in this scope`}
        </span>
        {/* The same control the form uses for the same value, so "which
            scope" looks like one idea on this screen rather than two. */}
        <NativeSelect
          aria-label="Filter by scope"
          value={String(scopeFilter)}
          onChange={(e) => setScopeFilter(e.target.value)}
          className="w-44"
        >
          <NativeSelectOption value="0">All scopes</NativeSelectOption>
          {allScopes.map((s) => (
            <NativeSelectOption key={s.id} value={String(s.id)}>
              {s.name}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        <div className="ml-auto flex items-center gap-3">
          {managed && <ManagedNotice />}
          {formFor === undefined && (
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={managed || allScopes.length === 0}
              onClick={() => setFormFor(null)}
            >
              <Plus />
              New reservation
            </Button>
          )}
        </div>
      </div>

      {reservations.isError && reservations.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="reservations"
            onRetry={() => void reservations.refetch()}
            isRetrying={reservations.isFetching}
          />
        </div>
      )}

      <div
        className={cn(
          GRID,
          "shrink-0 items-center border-b border-border py-2",
          "font-mono text-[9.5px] leading-none font-medium tracking-[0.14em] text-muted-foreground uppercase",
        )}
      >
        <span>Scope</span>
        <span>Address</span>
        <span>MAC</span>
        <span>Hostname</span>
        <span>Comment</span>
        <span className="text-right">Actions</span>
      </div>

      {formFor !== undefined && (
        <ReservationForm
          // Re-seeded whenever the target changes: without this the fields
          // would keep whatever the last edit typed into them, and opening
          // a second row would show the first one's values.
          key={formFor?.id ?? "new"}
          target={formFor}
          scopes={allScopes}
          defaultScopeId={scopeFilter || (allScopes[0]?.id ?? 0)}
          onClose={() => setFormFor(undefined)}
        />
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>

      <ConfirmDeleteDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title="Delete this reservation?"
        description={
          deleteTarget && (
            <>
              <span className="font-medium text-foreground">{deleteTarget.mac}</span> goes back to
              taking its turn in the pool.
            </>
          )
        }
        isPending={deleteReservation.isPending}
        onConfirm={() =>
          deleteTarget &&
          deleteReservation.mutate(deleteTarget.id, {
            onSuccess: () => setDeleteTarget(null),
            onError: (err) =>
              toast.error(
                err instanceof ApiError ? err.message : "Couldn't delete the reservation",
              ),
          })
        }
      />
    </div>
  );
}
