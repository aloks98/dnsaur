import { useEffect, type ReactNode } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import type { z } from "zod";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  Button,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
} from "@e412/rnui-react";

/**
 * The two dialogs every list-shaped screen needs, in one place.
 *
 * They live under pages/ rather than components/ on purpose: both are
 * compositions of rnui primitives with this app's copy and mutation
 * conventions baked in, not new primitives.
 */

interface RenameValues {
  name: string;
}

/**
 * Ask for one name and do something with it — rename a group, clone a zone.
 *
 * It is named for its first caller rather than generalised into a
 * `TextPromptDialog`: every use so far asks for a name, validates it with the
 * caller's own schema, and posts it, and a second component for the same
 * three things would be the drift this one exists to prevent. What varies is
 * the copy, which is all props.
 *
 * `targetId` is the reset key, and the reason this exists. Both screens used
 * to mount their rename dialog unconditionally and let `useForm` capture
 * `defaultValues` once — at mount, when there was no target — so the field
 * opened empty on the first rename and, from then on, held whatever had last
 * been typed into it. Renaming "Kids" to "Children" and then opening
 * "Office" showed "Children". Re-seeding whenever the target changes is what
 * makes the field describe the thing being renamed.
 */
export function RenameDialog({
  targetId,
  title,
  description,
  label = "Name",
  initialName,
  placeholder,
  hint,
  schema,
  submitLabel = "Save",
  pendingLabel = "Saving…",
  isPending,
  onSubmit,
  onClose,
}: {
  /** The thing being renamed; `null` closes the dialog. */
  targetId: number | null;
  title: string;
  description?: ReactNode;
  label?: string;
  /** What the field opens on — a group's current name, or blank where the
   * server reads blank as "go back to the default". */
  initialName: string;
  placeholder?: string;
  hint?: ReactNode;
  /** The caller's own validation, since "blank" means different things:
   * a group needs a name, a list reads blank as "back to the URL default". */
  schema: z.ZodType<RenameValues, RenameValues>;
  /** The confirm button's words, and the words it wears while the request is
   * in flight. "Save" is right for a rename and wrong for anything that
   * creates something. */
  submitLabel?: string;
  pendingLabel?: string;
  isPending: boolean;
  onSubmit: (name: string) => void;
  onClose: () => void;
}) {
  const form = useForm<RenameValues>({
    resolver: zodResolver(schema),
    defaultValues: { name: initialName },
  });
  const { reset } = form;

  useEffect(() => {
    reset({ name: initialName });
  }, [targetId, initialName, reset]);

  if (targetId === null) return null;

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description !== undefined && <DialogDescription>{description}</DialogDescription>}
        </DialogHeader>
        <Form {...form}>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit((values) => onSubmit(values.name.trim()))(e)}
            noValidate
          >
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{label}</FormLabel>
                  <FormControl>
                    <Input {...field} placeholder={placeholder} autoComplete="off" />
                  </FormControl>
                  {hint !== undefined && <FormDescription>{hint}</FormDescription>}
                  <FormMessage />
                </FormItem>
              )}
            />
            <DialogFooter>
              <DialogClose render={<Button type="button" variant="outline" />}>Cancel</DialogClose>
              <Button type="submit" disabled={isPending}>
                {isPending ? pendingLabel : submitLabel}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Confirm a destructive action.
 *
 * The solid destructive class list rather than rnui's `destructive` Button
 * variant: that one is tinted, which would make deleting read softer than
 * the confirms it sits beside.
 */
export function ConfirmDeleteDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel = "Delete",
  pendingLabel = "Deleting…",
  isPending,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: ReactNode;
  confirmLabel?: string;
  pendingLabel?: string;
  isPending: boolean;
  onConfirm: () => void;
}) {
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            className="bg-destructive text-destructive-solid-foreground hover:bg-destructive/90"
            disabled={isPending}
            onClick={onConfirm}
          >
            {isPending ? pendingLabel : confirmLabel}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
