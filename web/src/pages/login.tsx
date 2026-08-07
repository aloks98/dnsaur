import { useEffect, useState, type ReactNode } from "react";
import { CircleAlert, UserRound } from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { useQueryClient } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Card,
  cn,
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import { AuthLayout } from "../components/auth-layout";
import { authKeys, useLogin } from "../hooks/use-auth";
import { useHealth } from "../hooks/use-stats";
import { totpCodeSchema } from "../lib/schemas";

type Step = "credentials" | "totp";

/**
 * One schema per step, rather than one for the form.
 *
 * The verification code field only exists — and only has to hold six
 * digits — once the server has asked for it; on the credentials step it's
 * an unmounted, empty string that must not block the first submit. A
 * resolver validates the whole schema at once (unlike per-field `rules`,
 * which only ran for mounted fields), so the step picks the schema and
 * `useForm` is handed the matching resolver on every render.
 *
 * Neither username nor password is trimmed: whitespace can be part of a
 * password, and this only has to reject the empty string exactly as the
 * `required` rule it replaces did.
 */
const credentialsSchema = z.object({
  username: z.string().min(1, "Username is required"),
  password: z.string().min(1, "Password is required"),
  totpCode: z.string(),
});

const totpChallengeSchema = credentialsSchema.extend({ totpCode: totpCodeSchema });

type LoginFormValues = z.infer<typeof credentialsSchema>;

interface FormError {
  /** True when the error should offer a way to jump to first-run setup. */
  setupRequired: boolean;
  message: string;
}

/**
 * Gives step content a quiet, mechanical settle-in (fade + slight rise)
 * whenever `step` changes, instead of the credentials/TOTP swap just
 * popping. Deliberately entrance-only (no exit animation) to keep this
 * proportional to a small auth screen. Respects reduced-motion by only
 * ever applying the transition itself under `motion-safe`, so with motion
 * reduced the content still appears — just without the animated approach.
 */
function useStepTransition(step: Step) {
  const [settled, setSettled] = useState(false);

  useEffect(() => {
    setSettled(false);
    const raf = requestAnimationFrame(() => setSettled(true));
    return () => cancelAnimationFrame(raf);
  }, [step]);

  return settled;
}

/**
 * A rejected submission, said next to the field that was rejected rather
 * than in a banner above the form.
 *
 * These are not validation messages — the form is well-formed, the server
 * just said no — so they don't go through FormMessage, which is driven by
 * the resolver and would be cleared by the next keystroke.
 */
function SubmitError({ children }: { children: ReactNode }) {
  return (
    <p className="flex items-start gap-2 text-sm text-destructive-foreground">
      <CircleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
      <span className="text-pretty">{children}</span>
    </p>
  );
}

/**
 * Login screen with an optional TOTP second step.
 *
 * POST /auth/login can reply three different ways: 200 (session cookie set —
 * useLogin invalidates `me` and the app's auth gate swaps to the
 * authenticated routes on its own, no navigation needed here), 428 (the
 * account has TOTP enabled and *no* code was supplied), or 401 (anything
 * else the server rejects: wrong username, wrong password — and wrong TOTP
 * code, since internal/auth/service.go maps a supplied-but-invalid code to
 * ErrBadCredentials, not ErrTOTPRequired). A 428 is not a "failure" — it's an
 * expected step — so it silently reveals the verification-code step. A 401
 * *while on that step* is a rejected code: it keeps the user there with the
 * password intact, rather than bouncing them back to retype everything.
 *
 * Username and password stay registered in the form (react-hook-form keeps
 * unmounted field values by default) across the step change, since the
 * second request must resend them alongside totp_code.
 */
export function Login() {
  const qc = useQueryClient();
  const login = useLogin();
  const health = useHealth();
  const [step, setStep] = useState<Step>("credentials");
  const [formError, setFormError] = useState<FormError | null>(null);
  const settled = useStepTransition(step);

  const form = useForm<LoginFormValues>({
    resolver: zodResolver(step === "totp" ? totpChallengeSchema : credentialsSchema),
    defaultValues: { username: "", password: "", totpCode: "" },
  });

  // Focus the code the moment the step reveals it. An `autoFocus` attribute
  // would do the same thing but fires on mount regardless of how the field
  // got there, which is the usability problem the a11y rule is about; this
  // only moves focus as the direct result of the user submitting. An effect
  // rather than a call beside setStep(): the field does not exist until the
  // render that state change causes.
  useEffect(() => {
    if (step === "totp") form.setFocus("totpCode");
  }, [step, form]);

  function goToSetup() {
    void qc.invalidateQueries({ queryKey: authKeys.setup });
  }

  function backToCredentials() {
    setFormError(null);
    form.resetField("totpCode");
    setStep("credentials");
    form.setFocus("username");
  }

  function onSubmit(values: LoginFormValues) {
    setFormError(null);
    login.mutate(
      {
        username: values.username,
        password: values.password,
        ...(step === "totp" ? { totp_code: values.totpCode } : {}),
      },
      {
        onSuccess: () => toast.success("Logged in"),
        onError: (err) => {
          if (err instanceof ApiError && err.status === 428) {
            form.resetField("totpCode");
            setStep("totp");
            return;
          }
          // A *wrong* code is 401, not 428: internal/auth/service.go only
          // returns ErrTOTPRequired when no code was supplied at all, so a
          // supplied-but-invalid one comes back as plain bad credentials.
          // Stay on the code step (the password is still valid and must not
          // be thrown away) and say what actually went wrong.
          if (err instanceof ApiError && err.status === 401 && step === "totp") {
            const message =
              "That code didn't match. Codes rotate every 30 seconds — wait for the next one and try again. Your password is still accepted.";
            setFormError({ setupRequired: false, message });
            form.resetField("totpCode");
            toast.error("Invalid verification code — try again.");
            return;
          }
          if (err instanceof ApiError && err.status === 409) {
            const message = "This dnsaur instance hasn't been set up yet.";
            setFormError({ setupRequired: true, message });
            toast.error(message);
            return;
          }
          const message =
            err instanceof ApiError && err.status === 401
              ? // Named as the pair it is. The server checks both together
                // and will not say which half failed, so a message blaming
                // "username or password" invites re-typing the wrong one.
                "That username and password don't match. Both are checked together, so either one could be wrong."
              : err instanceof ApiError
                ? err.message
                : "Couldn't log in — try again.";
          setFormError({ setupRequired: false, message });
          form.resetField("password");
          setStep("credentials");
          toast.error("Invalid username or password.");
          form.setFocus("password");
        },
      },
    );
  }

  const onCredentials = step === "credentials";
  // A rejected submission, as opposed to the 409 that gets its own banner.
  const rejected = formError !== null && !formError.setupRequired;

  return (
    <AuthLayout
      eyebrow={onCredentials ? "Welcome back" : "Step 2 of 2"}
      title={onCredentials ? "Log in to dnsaur" : "Enter your code"}
      footer={
        onCredentials ? (
          // The build is worth stating on a self-hosted box: this is the one
          // screen an operator reaches before any chrome exists, and "which
          // version am I actually running?" is the first question when
          // something looks wrong. GET /health is public, so it answers even
          // signed out. The `v` is added here rather than baked into the
          // reported string, and stripped first so a tag that already has one
          // doesn't render as "vv".
          <>{health.data ? `dnsaur v${health.data.version.replace(/^v/, "")}` : "dnsaur"}</>
        ) : (
          <>Lost your authenticator? You&apos;ll need shell access to reset it.</>
        )
      }
    >
      <Card className="gap-4 p-5">
        {/* Nothing on the code step, deliberately. The artboard says "Your
            password was accepted" there, which states out loud that the
            password was right — useful to the account's owner and equally
            useful to someone who has only the password. The field's own hint
            below already says where the code comes from, so the sentence was
            not carrying anything else.
            This narrows the leak rather than closing it: the server answers
            428 only for credentials it accepted, so *arriving* at this step
            is itself the signal. Removing the wording is the part the UI can
            control. */}
        {onCredentials && (
          <p className="text-sm text-pretty text-muted-foreground">
            Enter your credentials to continue.
          </p>
        )}

        {/* Only the "there is no admin account" case keeps a banner: it is
            the one failure that is not about what was typed, and the only
            one with somewhere else to send you. Rejected credentials speak
            next to the field instead. */}
        {formError?.setupRequired && (
          <Alert variant="info">
            <CircleAlert />
            <AlertTitle>No admin account yet</AlertTitle>
            <AlertDescription>
              <p>{formError.message}</p>
              <Button type="button" variant="outline" size="sm" onClick={goToSetup}>
                Go to setup
              </Button>
            </AlertDescription>
          </Alert>
        )}

        <Form {...form}>
          <form
            className={cn(
              "flex flex-col gap-4 motion-safe:transition-all motion-safe:duration-200 motion-safe:ease-out",
              settled ? "translate-y-0 opacity-100" : "translate-y-1 opacity-0",
            )}
            onSubmit={(e) => void form.handleSubmit(onSubmit)(e)}
            noValidate
          >
            {onCredentials ? (
              <>
                <FormField
                  control={form.control}
                  name="username"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Username</FormLabel>
                      <FormControl>
                        <Input {...field} autoComplete="username" />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name="password"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>Password</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          type="password"
                          autoComplete="current-password"
                          aria-invalid={rejected || undefined}
                        />
                      </FormControl>
                      <FormMessage />
                      {rejected && <SubmitError>{formError.message}</SubmitError>}
                    </FormItem>
                  )}
                />
                <Button type="submit" disabled={login.isPending}>
                  {login.isPending ? "Logging in…" : "Log in"}
                </Button>
              </>
            ) : (
              <>
                {/* Who the code is for. On the second step the username is
                    off screen, and a code typed against the wrong account
                    fails with no hint as to why. */}
                <div className="flex items-center gap-2.5 border border-border bg-background p-2.5">
                  <UserRound className="size-4 shrink-0 text-muted-foreground" aria-hidden />
                  <span className="min-w-0 flex-1 truncate font-mono text-sm">
                    {form.getValues("username")}
                  </span>
                </div>

                <FormField
                  control={form.control}
                  name="totpCode"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>6-digit code</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          // Not type="number": a leading zero is significant
                          // and spinners are nonsense here. inputMode gets
                          // the numeric keypad on a phone without either.
                          inputMode="numeric"
                          autoComplete="one-time-code"
                          maxLength={6}
                          placeholder="000000"
                          aria-invalid={rejected || undefined}
                          className="h-11 text-center font-mono text-2xl tracking-widest"
                        />
                      </FormControl>
                      <FormMessage />
                      {rejected ? (
                        <SubmitError>{formError.message}</SubmitError>
                      ) : (
                        <p className="text-sm text-muted-foreground">
                          From your authenticator app. Rotates every 30 seconds.
                        </p>
                      )}
                    </FormItem>
                  )}
                />

                <div className="flex flex-col gap-2">
                  <Button type="submit" disabled={login.isPending}>
                    {login.isPending ? "Verifying…" : "Verify and sign in"}
                  </Button>
                  <Button type="button" variant="ghost" onClick={backToCredentials}>
                    Back to password
                  </Button>
                </div>
              </>
            )}
          </form>
        </Form>
      </Card>
    </AuthLayout>
  );
}
