import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Eye,
  EyeOff,
  KeyRound,
  Pencil,
  Plus,
  RotateCw,
  Trash2,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { useForm, useWatch, type UseFormReturn } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  cn,
  CopyButton,
  EmptyState,
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
import { ApiError } from "../api/client";
import type { TSIGKey } from "../api/types";
import {
  useCreateTSIGKey,
  useDeleteTSIGKey,
  useTSIGKeys,
  useUpdateTSIGKey,
} from "../hooks/use-tsig-keys";
import { useZones } from "../hooks/use-zones";
import { useManagedBy } from "../hooks/use-sync";
import { ManagedNotice } from "../components/managed-notice";
import { StaleDataAlert } from "../components/stale-data-alert";
import { aclKeyNames } from "../lib/acl";
import { dnsNameSchema } from "../lib/dns-name";
import { notifyKeyNames } from "../lib/notify";
import {
  algorithmLabel,
  algorithmOptions,
  canonicalKeyName,
  DEFAULT_TSIG_ALGORITHM,
  generateSecret,
  isBase64,
} from "../lib/tsig";

/**
 * What a secret looks like when it isn't being read.
 *
 * A fixed width rather than one bullet per character: the real secrets are
 * 44 characters for a 32-byte key and 88 for a 64-byte one, and masking to
 * their true length would leak the algorithm's key size and make the column
 * ragged for no gain. Exported so the tests assert on the same constant the
 * page renders.
 */
export const MASK = "•".repeat(24);

/** One declaration of the column geometry, shared by the header, the create
 * row and every key row — the artboard's five, USED BY included. That column
 * was held back in Milestone D1 because nothing could yet reference a key:
 * no handler accepted `zones.tsig_key_id`, so it would have read "—" on every
 * row forever. D2 is what gives it something to count. */
const GRID = "grid grid-cols-[1fr_132px_316px_108px_116px] items-center gap-3.5 px-4";

const keyFormSchema = z.object({
  // The same check the zones list's name field makes, and for the same
  // reason: both are `dns.CanonicalName` on the other side — see
  // lib/dns-name.ts.
  name: dnsNameSchema("Enter a domain name, e.g. xfer.example.com"),
  // Free-form rather than a z.enum over TSIG_ALGORITHMS: the select's
  // options already constrain this to what the server accepts, and an enum
  // here would reject the passthrough option algorithmOptions() adds for a
  // stored value this build doesn't know (see lib/tsig.ts) — turning "edit
  // this key's name" into an un-submittable form.
  algorithm: z.string().min(1),
  secret: z
    .string()
    .trim()
    .min(1, "Enter a secret")
    .refine(isBase64, "Secret must be base64, e.g. the value the peer was given"),
});

type KeyFormValues = z.infer<typeof keyFormSchema>;

/**
 * The three inputs, identical in the create row and the edit row.
 *
 * The algorithm select is the one place the wire↔display mismatch is
 * settled, and it is settled by *not* converting: every option is valued
 * with the API's own string (trailing dot included — miekg's constants) and
 * only labelled without it. A fetched `hmac-sha512.` therefore selects its
 * option verbatim, and a Save that touched nothing else puts back exactly
 * what came out. Labels as values would leave a native select showing its
 * first option for every fetched key, and "edit a key's name" would silently
 * rewrite its algorithm to hmac-sha1.
 */
function KeyFields({
  form,
  algorithms,
  secretPlaceholder,
  nameFixed = false,
  onRegenerate,
}: {
  form: UseFormReturn<KeyFormValues>;
  algorithms: string[];
  secretPlaceholder?: string;
  /** A zone names this key, so its name is not this row's to change — see
   * EditKeyRow. The other two fields stay editable. */
  nameFixed?: boolean;
  onRegenerate: () => void;
}) {
  return (
    <>
      <FormField
        control={form.control}
        name="name"
        render={({ field }) => (
          <FormItem>
            <FormControl>
              <Input
                {...field}
                aria-label="Key name"
                placeholder="xfer.e412.in"
                autoComplete="off"
                disabled={nameFixed}
                className="font-mono"
              />
            </FormControl>
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name="algorithm"
        render={({ field }) => (
          <FormItem>
            <FormControl>
              <NativeSelect {...field} aria-label="Algorithm">
                {algorithms.map((algorithm) => (
                  <NativeSelectOption key={algorithm} value={algorithm}>
                    {algorithmLabel(algorithm)}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            </FormControl>
          </FormItem>
        )}
      />
      <span className="flex min-w-0 items-center gap-1.5">
        <FormField
          control={form.control}
          name="secret"
          render={({ field }) => (
            <FormItem className="min-w-0 flex-1">
              <FormControl>
                <Input
                  {...field}
                  aria-label="Secret"
                  placeholder={secretPlaceholder}
                  autoComplete="off"
                  spellCheck={false}
                  className="font-mono"
                />
              </FormControl>
            </FormItem>
          )}
        />
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label="Generate a new secret"
          onClick={onRegenerate}
        >
          <RotateCw />
        </Button>
      </span>
    </>
  );
}

/**
 * The create row: a band under the header rather than a dialog, matching
 * every other create affordance in the app (zones, clients, tokens).
 *
 * Generating is the default path and the field opens pre-filled, because a
 * hand-typed secret is usually a weak one and the server can only check that
 * a secret is base64, never that it is strong. "Paste an existing secret" is
 * the deliberate departure — used when the peer already holds a key and
 * dnsaur is the side being configured to match.
 */
function NewKeyRow({ onClose }: { onClose: () => void }) {
  const createKey = useCreateTSIGKey();
  // Lazily, once: useForm reads defaultValues on first render only, so
  // calling generateSecret() inline would burn a CSPRNG draw per render and
  // throw all but the first away.
  const [firstSecret] = useState(generateSecret);
  const [pasting, setPasting] = useState(false);
  const form = useForm<KeyFormValues>({
    resolver: zodResolver(keyFormSchema),
    defaultValues: { name: "", algorithm: DEFAULT_TSIG_ALGORITHM, secret: firstSecret },
  });

  useEffect(() => {
    form.setFocus("name");
  }, [form]);

  const typedName = useWatch({ control: form.control, name: "name" });
  const canonical = canonicalKeyName(typedName);
  const nameError = form.formState.errors.name?.message;
  const secretError = form.formState.errors.secret?.message;

  function fillGenerated() {
    setPasting(false);
    form.setValue("secret", generateSecret(), { shouldValidate: false });
    form.clearErrors("secret");
  }

  function switchToPasting() {
    setPasting(true);
    form.setValue("secret", "", { shouldValidate: false });
    form.clearErrors("secret");
    form.setFocus("secret");
  }

  function onSubmit(values: KeyFormValues) {
    createKey.mutate(
      // Sent as typed (trimmed by the schema), not canonicalised here: the
      // server lowercases and fully qualifies every name it is given, and
      // the line under this field already says what that will produce.
      { name: values.name, algorithm: values.algorithm, secret: values.secret },
      {
        onSuccess: () => {
          toast.success("Key added");
          onClose();
        },
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't add the key"),
      },
    );
  }

  return (
    <Form {...form}>
      <form
        onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
        noValidate
        data-slot="new-tsig-key-row"
        className="shrink-0 border-b border-border bg-card shadow-[inset_3px_0_0_var(--primary)]"
      >
        <div className={cn(GRID, "py-2.5")}>
          <KeyFields
            form={form}
            algorithms={algorithmOptions(undefined)}
            secretPlaceholder={pasting ? "base64 secret from the other server" : undefined}
            onRegenerate={fillGenerated}
          />
          {/* USED BY: a key that does not exist yet is used by nothing, and
              the cell holds the column open rather than letting the actions
              slide left. */}
          <span />
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={createKey.isPending}>
              {createKey.isPending ? "Adding…" : "Add"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Close the new-key row"
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>

        <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
          <span>
            {nameError ? (
              <FormField control={form.control} name="name" render={() => <FormMessage />} />
            ) : (
              canonical !== "" && (
                // The server canonicalises every name it stores, and the key
                // has to match the name the peer signs with — so what will
                // actually be saved is said while it is being typed, not
                // discovered in the row afterwards.
                <span className="font-mono text-muted-foreground">Saved as {canonical}</span>
              )
            )}
          </span>
          <span />
          <span className="flex items-center gap-2.5">
            {secretError ? (
              <FormField control={form.control} name="secret" render={() => <FormMessage />} />
            ) : (
              <>
                {/* A real button, not the artboard's `<a href="#">`: it
                    changes what this row does rather than going anywhere. */}
                <button
                  type="button"
                  onClick={pasting ? fillGenerated : switchToPasting}
                  className="rounded-xs text-primary underline-offset-2 outline-none hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                >
                  {pasting ? "Generate one" : "Paste an existing secret"}
                </button>
                {!pasting && <span className="text-muted-foreground">base64, 32 bytes</span>}
              </>
            )}
          </span>
          <span />
          <span />
        </div>
      </form>
    </Form>
  );
}

/**
 * The same row, in place, for an existing key. PUT is a full replace — the
 * server has no partial-patch shape for a TSIG key — so all three fields are
 * sent whether they were touched or not.
 *
 * The **name** is the exception once anything names this key. `allow_transfer`
 * and `notify_to` reference a key by its canonical name rather than its id, so
 * a rename silently detaches it from every zone that named it and the server
 * refuses one outright. Disabled rather than left to fail on click, on the same
 * terms as the delete confirm's own guard — and, like that guard, this is an
 * affordance and not the enforcement: it reads the zones list, so it is blind
 * whenever that list failed to load. The 409 below is what covers that.
 */
function EditKeyRow({
  tsigKey,
  usedBy,
  onClose,
}: {
  tsigKey: TSIGKey;
  usedBy: number;
  onClose: () => void;
}) {
  const updateKey = useUpdateTSIGKey();
  const form = useForm<KeyFormValues>({
    resolver: zodResolver(keyFormSchema),
    defaultValues: {
      name: tsigKey.name,
      algorithm: tsigKey.algorithm,
      secret: tsigKey.secret,
    },
  });
  const nameFixed = usedBy > 0;

  const nameError = form.formState.errors.name?.message;
  const secretError = form.formState.errors.secret?.message;

  function onSubmit(values: KeyFormValues) {
    updateKey.mutate(
      { id: tsigKey.id, name: values.name, algorithm: values.algorithm, secret: values.secret },
      {
        onSuccess: () => {
          toast.success("Key saved");
          onClose();
        },
        onError: (err) => {
          // A 409 is the server refusing this rename because a zone names
          // the key — so it belongs on the name field, in the server's own
          // words, with the attempted value still there to correct. Every
          // other failure is the whole row's and stays a toast.
          if (err instanceof ApiError && err.status === 409) {
            form.setError("name", { type: "server", message: err.message });
            return;
          }
          toast.error(err instanceof ApiError ? err.message : "Couldn't save the key");
        },
      },
    );
  }

  return (
    <Form {...form}>
      <form onSubmit={(e) => void form.handleSubmit(onSubmit)(e)} noValidate>
        <div className={cn(GRID, "py-2")}>
          <KeyFields
            form={form}
            // The stored algorithm is always offered, even if this build
            // doesn't recognise it — see lib/tsig.ts's algorithmOptions.
            algorithms={algorithmOptions(tsigKey.algorithm)}
            nameFixed={nameFixed}
            onRegenerate={() => {
              form.setValue("secret", generateSecret(), { shouldValidate: false });
              form.clearErrors("secret");
            }}
          />
          {/* Kept visible while editing, deliberately: it is the count of
              zones whose next transfer this Save is about to change, which is
              exactly what someone rotating a secret needs in front of them. */}
          <UsedByCell count={usedBy} />
          <div className="flex items-center justify-end gap-1.5">
            <Button type="submit" size="sm" disabled={updateKey.isPending}>
              {updateKey.isPending ? "Saving…" : "Save"}
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label={`Stop editing ${tsigKey.name}`}
              onClick={onClose}
            >
              <X />
            </Button>
          </div>
        </div>

        {(nameError !== undefined || secretError !== undefined || nameFixed) && (
          <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
            <span>
              {nameError ? (
                <FormField control={form.control} name="name" render={() => <FormMessage />} />
              ) : (
                nameFixed && (
                  <span className="text-muted-foreground">
                    In use by {usedBy} {usedBy === 1 ? "zone" : "zones"}. The name can&apos;t
                    change.
                  </span>
                )
              )}
            </span>
            <span />
            <span>
              {secretError && (
                <FormField control={form.control} name="secret" render={() => <FormMessage />} />
              )}
            </span>
            <span />
            <span />
          </div>
        )}
      </form>
    </Form>
  );
}

/**
 * How many zones sign their transfers with this key — the USED BY column.
 *
 * Counted client-side from the zones list rather than served as a field on
 * the key, because it already is one: every zone carries `tsig_key_id`, so
 * the answer is a fold over a list this dashboard fetches anyway, and an
 * endpoint for it would be a second source of the same truth to keep in step.
 *
 * "—" for an unused key rather than "0": the column is answering "what
 * depends on this", and nothing is not a quantity.
 */
function UsedByCell({ count }: { count: number }) {
  return (
    <span
      className={cn(
        "text-right font-mono text-sm",
        count > 0 ? "text-foreground" : "text-muted-foreground",
      )}
    >
      {count === 0 ? "—" : `${count} ${count === 1 ? "zone" : "zones"}`}
    </span>
  );
}

/**
 * One key at rest.
 *
 * The secret is masked, and the two controls beside it are deliberately not
 * the same control: Copy is always available and never reveals, because
 * pasting the secret into the peer's config is the common task and reading
 * it aloud is the exception. The eye is that exception, and only one row can
 * be open at a time (see the page's `revealedId`) — the alternative is four
 * secrets accumulating on screen through a session.
 *
 * None of this is a security boundary. The API returns every secret on every
 * read, deliberately (see api/types.ts's TSIGKey), so masking governs what
 * sits on screen, nothing more.
 */
function KeyRow({
  tsigKey,
  usedBy,
  revealed,
  onReveal,
  onEdit,
  onDelete,
  dimActions,
}: {
  tsigKey: TSIGKey;
  /** Zones whose tsig_key_id names this key — see UsedByCell. */
  usedBy: number;
  revealed: boolean;
  onReveal: () => void;
  onEdit: () => void;
  onDelete: () => void;
  /** Another row (or the create row) is mid-edit — see the page. */
  dimActions: boolean;
}) {
  return (
    <div className={cn(GRID, "py-2")}>
      <span className="min-w-0 truncate font-mono text-sm" title={tsigKey.name}>
        {tsigKey.name}
      </span>
      <span className="font-mono text-sm text-muted-foreground">
        {algorithmLabel(tsigKey.algorithm)}
      </span>
      <span className="flex min-w-0 items-center gap-2">
        <span
          className={cn(
            "min-w-0 flex-1 truncate font-mono text-sm",
            revealed ? "text-foreground" : "tracking-wider text-muted-foreground",
          )}
          title={revealed ? tsigKey.secret : undefined}
        >
          {revealed ? tsigKey.secret : MASK}
        </span>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`${revealed ? "Hide" : "Show"} the secret for ${tsigKey.name}`}
          onClick={onReveal}
        >
          {revealed ? <EyeOff /> : <Eye />}
        </Button>
        {/* Copies without revealing: the value goes to the clipboard, the
            row stays masked. */}
        <CopyButton value={tsigKey.secret} label={`Copy the secret for ${tsigKey.name}`} />
      </span>
      <UsedByCell count={usedBy} />
      {/* The artboard's `actionsOpacity`/`actionsEvents` — while one row is
          being written, the others' actions are visibly not the thing to
          click. Real `disabled` is what delivers that: `pointer-events-none`
          on the wrapper stops the pointer and nothing else, so both buttons
          kept their place in the tab order, still fired on Enter, and were
          announced as available. `disabled:opacity-30` is the artboard's own
          fade in place of rnui's 0.5. */}
      <span className="flex items-center justify-end gap-1">
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Edit ${tsigKey.name}`}
          className="disabled:opacity-30"
          disabled={dimActions}
          onClick={onEdit}
        >
          <Pencil />
        </Button>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Delete ${tsigKey.name}`}
          className="disabled:opacity-30"
          disabled={dimActions}
          onClick={onDelete}
        >
          <Trash2 />
        </Button>
      </span>
    </div>
  );
}

/**
 * The delete confirm: a strip under the row it is about, not a modal.
 *
 * The thing being deleted stays on screen and in place while the question is
 * asked — which is the whole argument for an inline confirm over a dialog
 * that covers the list and names the key in prose.
 *
 * A key some zone signs with cannot be deleted at all: the server answers 409
 * (store.ErrInUse — tsigKeyStore.Delete refuses while any zone's tsig_key_id
 * names it), because removing it would leave that secondary unable to
 * authenticate its transfers with nothing on the zone to say why. So this
 * strip asks a different question in that case — it states the obstacle and
 * what to do about it — and the Delete button is disabled rather than left to
 * produce a 409 on click.
 */
function DeleteConfirm({
  tsigKey,
  usedBy,
  pending,
  onCancel,
  onConfirm,
}: {
  tsigKey: TSIGKey;
  usedBy: number;
  pending: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const inUse = usedBy > 0;
  return (
    <div className="flex items-center gap-3 border-t border-border-muted px-4 pt-2 pb-2.5">
      <TriangleAlert
        aria-hidden="true"
        className={cn("size-4 shrink-0", inUse && "text-destructive-foreground")}
      />
      <span className={cn("text-sm", inUse && "text-destructive-foreground")}>
        {inUse
          ? `In use by ${usedBy} ${usedBy === 1 ? "zone" : "zones"}. Remove it from them first.`
          : `Delete ${tsigKey.name}?`}
      </span>
      <span className="ml-auto flex items-center gap-2">
        <Button type="button" size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          disabled={pending || inUse}
          onClick={onConfirm}
        >
          {pending ? "Deleting…" : "Delete"}
        </Button>
      </span>
    </div>
  );
}

/** "No keys" / "1 key" / "4 keys" — the header's left-hand readout. */
function countLabel(count: number): string {
  if (count === 0) return "No keys";
  return `${count} ${count === 1 ? "key" : "keys"}`;
}

/**
 * TSIG keys — the shared secrets that authenticate zone transfers (RFC
 * 8945). Both ends hold the same named key and sign every transfer message
 * with it; the secret has to be identical on the peer, which is why this
 * screen shows it at all.
 *
 * GET/POST/PUT/DELETE /tsig-keys via use-tsig-keys.ts. Changes take effect
 * on the next signed message — the DNS server reads keys from the store per
 * message, so nothing here needs a restart.
 */
export function TSIGKeys() {
  const keys = useTSIGKeys();
  const zones = useZones();
  const deleteKey = useDeleteTSIGKey();
  // Keys travel in the bundle, secrets included, so on a replica they are
  // the main's to author. Revealing and copying a secret stays live: a
  // replica holds the same key material and an operator may still need to
  // read it out.
  const managedBy = useManagedBy();

  /**
   * How many zones name each key — the USED BY column, and the in-use delete
   * guard that reads the same number.
   *
   * A zone can depend on a key three ways now. `tsig_key_id` is what a
   * *secondary* signs its own pulls with; `allow_transfer` (D3) is who may
   * pull *this* zone — a `key:` entry names a key the same way; and (D4,
   * Task 12) `notify_to` is who this zone signs its own outbound NOTIFYs
   * for, a third `key:`-shaped reference with its own grammar
   * (lib/notify.ts). The store's delete guard refuses any of the three on
   * the same terms (see tsigKeyStore.Delete / internal/store/tsigkeys.go's
   * aclKeyRef and notifyKeyRef). `allow_transfer` and `notify_to` both name
   * keys by their canonical *name*, not id, so `idByName` is what turns
   * `aclKeyNames`'s and `notifyKeyNames`'s output back into the id this map
   * is keyed by — the zones list carries names nowhere else. All three
   * references are folded through one `Set` per zone before being added to
   * the running counts, so a zone that names the same key more than one way
   * still counts once: one 409 is one dependent, not two or three.
   *
   * The guard is an affordance, not the enforcement. The server refuses the
   * delete with 409 whichever way this map came out (tsigKeyStore.Delete does
   * it in one statement, so there is no check-then-act window there), which
   * matters because this map can be wrong in one direction: if /zones failed
   * to load, every key reads "—" and every Delete button is enabled. The
   * onError below is what covers that case, so a key that turns out to be in
   * use says so rather than reporting a generic failure.
   */
  const usageByKeyID = useMemo(() => {
    const idByName = new Map<string, number>();
    for (const k of keys.data ?? []) idByName.set(k.name, k.id);

    const counts = new Map<number, number>();
    for (const zone of zones.data ?? []) {
      const ids = new Set<number>();
      if (zone.tsig_key_id !== 0) ids.add(zone.tsig_key_id);
      for (const name of [...aclKeyNames(zone.allow_transfer), ...notifyKeyNames(zone.notify_to)]) {
        const id = idByName.get(name);
        if (id !== undefined) ids.add(id);
      }
      for (const id of ids) counts.set(id, (counts.get(id) ?? 0) + 1);
    }
    return counts;
  }, [zones.data, keys.data]);
  const usedBy = (id: number) => usageByKeyID.get(id) ?? 0;

  const [addOpen, setAddOpen] = useState(false);
  const [editingId, setEditingId] = useState<number | null>(null);
  const [deletingId, setDeletingId] = useState<number | null>(null);
  // One at a time, by design: revealing a second row closes the first.
  const [revealedId, setRevealedId] = useState<number | null>(null);

  // "Busy" in the artboard's sense: some row is being written. Every other
  // row's actions dim and stop taking clicks while that's true.
  const busy = addOpen || editingId !== null || deletingId !== null;

  function openCreate() {
    setAddOpen(true);
    setEditingId(null);
    setDeletingId(null);
    setRevealedId(null);
  }

  function openEdit(id: number) {
    setEditingId(id);
    setAddOpen(false);
    setDeletingId(null);
    // A row being edited shows its secret in a field; leaving a *different*
    // row revealed behind it just puts a second secret on screen.
    setRevealedId(null);
  }

  function onConfirmDelete(target: TSIGKey) {
    deleteKey.mutate(target.id, {
      onSuccess: () => {
        toast.success("Key deleted");
        setDeletingId(null);
        if (revealedId === target.id) setRevealedId(null);
      },
      onError: (err) => {
        // The guard above should have caught this, and does whenever the
        // zones list is in hand — but a 409 can still arrive: /zones may have
        // failed to load, or a zone may have started using this key since it
        // did. The server's own message for it is "resource in use", which is
        // true and says nothing about what to do, so this one is written
        // here. Anything else keeps the generic failure.
        if (err instanceof ApiError && err.status === 409) {
          toast.error(`${target.name} is in use by a zone. Remove it from them first.`);
          return;
        }
        toast.error(`Couldn't delete ${target.name}`);
      },
    });
  }

  const hasKeys = keys.data !== undefined && keys.data.length > 0;

  let body: ReactNode;
  if (keys.isPending) {
    body = (
      <div className="flex flex-col gap-2 p-4" aria-hidden="true">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (keys.data === undefined) {
    // isError with nothing in hand — the first load itself failed, so there
    // is nothing to fall back to. A background failure with data still good
    // takes the StaleDataAlert path below instead.
    body = (
      <div className="p-4">
        <Alert variant="destructive">
          <TriangleAlert />
          <AlertTitle>Couldn&apos;t load TSIG keys</AlertTitle>
          <AlertDescription>Try refreshing the page.</AlertDescription>
        </Alert>
      </div>
    );
  } else if (!hasKeys) {
    // Nothing to say while the create row is open: it is already the answer
    // to both halves of the empty state.
    body = addOpen ? null : (
      <div className="flex h-full flex-col items-center justify-center">
        <EmptyState
          icon={<KeyRound />}
          title="No TSIG keys yet."
          action={
            <Button type="button" size="sm" disabled={managedBy !== ""} onClick={openCreate}>
              <Plus />
              New key
            </Button>
          }
        />
      </div>
    );
  } else {
    body = keys.data.map((tsigKey) => (
      <div
        key={tsigKey.id}
        data-testid="tsig-key-row"
        data-slot="tsig-key-row"
        className={cn(
          "border-b border-border-muted",
          editingId === tsigKey.id && "bg-card shadow-[inset_3px_0_0_var(--primary)]",
          deletingId === tsigKey.id && "bg-card shadow-[inset_3px_0_0_var(--destructive)]",
        )}
      >
        {editingId === tsigKey.id ? (
          <EditKeyRow
            tsigKey={tsigKey}
            usedBy={usedBy(tsigKey.id)}
            onClose={() => setEditingId(null)}
          />
        ) : (
          <KeyRow
            tsigKey={tsigKey}
            usedBy={usedBy(tsigKey.id)}
            revealed={revealedId === tsigKey.id}
            onReveal={() => setRevealedId(revealedId === tsigKey.id ? null : tsigKey.id)}
            onEdit={() => openEdit(tsigKey.id)}
            onDelete={() => setDeletingId(tsigKey.id)}
            // On a replica for the same reason as while another row is
            // being written: these are not the thing to click.
            dimActions={busy || managedBy !== ""}
          />
        )}
        {deletingId === tsigKey.id && (
          <DeleteConfirm
            tsigKey={tsigKey}
            usedBy={usedBy(tsigKey.id)}
            pending={deleteKey.isPending}
            onCancel={() => setDeletingId(null)}
            onConfirm={() => onConfirmDelete(tsigKey)}
          />
        )}
      </div>
    ));
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {managedBy !== "" && <ManagedNotice peer={managedBy} />}

      <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {keys.data ? countLabel(keys.data.length) : ""}
        </span>
        {!addOpen && (
          <Button
            type="button"
            size="sm"
            className="ml-auto"
            disabled={managedBy !== ""}
            onClick={openCreate}
          >
            <Plus />
            New key
          </Button>
        )}
      </div>

      {keys.isError && keys.data !== undefined && (
        <div className="shrink-0 border-b border-border p-3">
          <StaleDataAlert
            what="TSIG keys"
            onRetry={() => void keys.refetch()}
            isRetrying={keys.isFetching}
          />
        </div>
      )}

      {/* Only over rows. A column header above an empty state labels
          nothing — the artboard gates it on having keys for that reason. */}
      {hasKeys && (
        <div
          className={cn(
            GRID,
            "shrink-0 border-b border-border py-2",
            "font-mono text-xs tracking-widest text-muted-foreground uppercase",
          )}
        >
          <span>Name</span>
          <span>Algorithm</span>
          <span>Secret</span>
          <span className="text-right">Used by</span>
          <span className="text-right">Actions</span>
        </div>
      )}

      {addOpen && <NewKeyRow onClose={() => setAddOpen(false)} />}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>
    </div>
  );
}
