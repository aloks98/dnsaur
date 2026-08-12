import { useEffect, useState, type ReactNode } from "react";
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
import { useForm, type UseFormReturn } from "react-hook-form";
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
import { StaleDataAlert } from "../components/stale-data-alert";
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
 * row and every key row.
 *
 * Four columns, not the artboard's five: USED BY is deliberately absent.
 * `zones.tsig_key_id` exists in the schema but nothing reads or writes it
 * yet — no handler accepts it, nothing references a key, and deleting a
 * referenced key succeeds silently — so the column would read "—" on every
 * row forever. It lands with Milestone D2, together with the in-use delete
 * guard the artboard pairs it with ("In use by 3 zones. Remove it from them
 * first."), which today could never fire. */
const GRID = "grid grid-cols-[1fr_132px_316px_116px] items-center gap-3.5 px-4";

/**
 * Mirrors the server's own check (normalizeTSIGName in
 * internal/api/tsigkeys_handlers.go) closely enough that a name accepted
 * here round-trips: lowercasing and the trailing dot are applied server-side
 * regardless, so this only has to catch what would otherwise be a wasted
 * request — blank, whitespace/path characters, or an empty label (e.g.
 * "e412..in"). Same schema as the zones list's name field, for the same
 * reason: both are `dns.CanonicalName` on the other side.
 */
const keyNameSchema = z
  .string()
  .trim()
  .refine((value) => {
    const name = value.replace(/\.$/, "");
    if (name === "" || /[ \t\r\n/\\]/.test(name)) return false;
    return !name.split(".").some((label) => label === "");
  }, "Enter a domain name, e.g. xfer.example.com");

const keyFormSchema = z.object({
  name: keyNameSchema,
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
  onRegenerate,
}: {
  form: UseFormReturn<KeyFormValues>;
  algorithms: string[];
  secretPlaceholder?: string;
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

  const typedName = form.watch("name");
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
        </div>
      </form>
    </Form>
  );
}

/** The same row, in place, for an existing key. PUT is a full replace — the
 * server has no partial-patch shape for a TSIG key — so all three fields are
 * sent whether they were touched or not. */
function EditKeyRow({ tsigKey, onClose }: { tsigKey: TSIGKey; onClose: () => void }) {
  const updateKey = useUpdateTSIGKey();
  const form = useForm<KeyFormValues>({
    resolver: zodResolver(keyFormSchema),
    defaultValues: {
      name: tsigKey.name,
      algorithm: tsigKey.algorithm,
      secret: tsigKey.secret,
    },
  });

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
        onError: (err) =>
          toast.error(err instanceof ApiError ? err.message : "Couldn't save the key"),
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
            onRegenerate={() => {
              form.setValue("secret", generateSecret(), { shouldValidate: false });
              form.clearErrors("secret");
            }}
          />
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

        {(nameError !== undefined || secretError !== undefined) && (
          <div className={cn(GRID, "items-start pb-2.5 text-xs text-pretty")}>
            <span>
              {nameError && (
                <FormField control={form.control} name="name" render={() => <FormMessage />} />
              )}
            </span>
            <span />
            <span>
              {secretError && (
                <FormField control={form.control} name="secret" render={() => <FormMessage />} />
              )}
            </span>
            <span />
          </div>
        )}
      </form>
    </Form>
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
  revealed,
  onReveal,
  onEdit,
  onDelete,
  dimActions,
}: {
  tsigKey: TSIGKey;
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
      <span
        className={cn(
          "flex items-center justify-end gap-1",
          // Exactly the artboard's `actionsOpacity`/`actionsEvents`: while
          // one row is being written, the others' actions are visibly not
          // the thing to click.
          dimActions && "pointer-events-none opacity-30",
        )}
      >
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Edit ${tsigKey.name}`}
          onClick={onEdit}
        >
          <Pencil />
        </Button>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label={`Delete ${tsigKey.name}`}
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
 */
function DeleteConfirm({
  tsigKey,
  pending,
  onCancel,
  onConfirm,
}: {
  tsigKey: TSIGKey;
  pending: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div className="flex items-center gap-3 border-t border-border-muted px-4 pt-2 pb-2.5">
      <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
      <span className="text-sm">Delete {tsigKey.name}?</span>
      <span className="ml-auto flex items-center gap-2">
        <Button type="button" size="sm" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          disabled={pending}
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
  const deleteKey = useDeleteTSIGKey();

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
      onError: () => toast.error(`Couldn't delete ${target.name}`),
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
            <Button type="button" size="sm" onClick={openCreate}>
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
          <EditKeyRow tsigKey={tsigKey} onClose={() => setEditingId(null)} />
        ) : (
          <KeyRow
            tsigKey={tsigKey}
            revealed={revealedId === tsigKey.id}
            onReveal={() => setRevealedId(revealedId === tsigKey.id ? null : tsigKey.id)}
            onEdit={() => openEdit(tsigKey.id)}
            onDelete={() => setDeletingId(tsigKey.id)}
            dimActions={busy}
          />
        )}
        {deletingId === tsigKey.id && (
          <DeleteConfirm
            tsigKey={tsigKey}
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
      <div className="flex shrink-0 items-center gap-3 border-b border-border px-4 py-2.5">
        <span className="font-mono text-xs text-muted-foreground">
          {keys.data ? countLabel(keys.data.length) : ""}
        </span>
        {!addOpen && (
          <Button type="button" size="sm" className="ml-auto" onClick={openCreate}>
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
          <span className="text-right">Actions</span>
        </div>
      )}

      {addOpen && <NewKeyRow onClose={() => setAddOpen(false)} />}

      <div className="min-h-0 flex-1 overflow-y-auto">{body}</div>
    </div>
  );
}
