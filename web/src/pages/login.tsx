import { useEffect, useState } from "react";
import { CircleAlert, KeyRound, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { useForm } from "react-hook-form";
import { useQueryClient } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  cn,
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
  Input,
  InputOTP,
  InputOTPGroup,
  InputOTPSeparator,
  InputOTPSlot,
} from "@e412/rnui-react";
import { ApiError } from "../api/client";
import { AuthLayout } from "../components/auth-layout";
import { authKeys, useLogin } from "../hooks/use-auth";

type Step = "credentials" | "totp";

interface LoginFormValues {
  username: string;
  password: string;
  totpCode: string;
}

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
  const [step, setStep] = useState<Step>("credentials");
  const [formError, setFormError] = useState<FormError | null>(null);
  const settled = useStepTransition(step);

  const form = useForm<LoginFormValues>({
    defaultValues: { username: "", password: "", totpCode: "" },
  });

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
            const message = "Invalid verification code — try again.";
            setFormError({ setupRequired: false, message });
            form.resetField("totpCode");
            toast.error(message);
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
              ? "Invalid username or password."
              : err instanceof ApiError
                ? err.message
                : "Couldn't log in — try again.";
          setFormError({ setupRequired: false, message });
          form.resetField("password");
          setStep("credentials");
          toast.error(message);
          form.setFocus("password");
        },
      },
    );
  }

  return (
    <AuthLayout eyebrow="Welcome back" title="Log in to dnsaur">
      <Card className="gap-6 py-6">
        <CardHeader className="px-6">
          <CardDescription className="flex items-center gap-1.5">
            {step === "credentials" ? (
              <KeyRound className="size-3.5 shrink-0" aria-hidden />
            ) : (
              <ShieldCheck className="size-3.5 shrink-0" aria-hidden />
            )}
            {step === "credentials"
              ? "Enter your credentials to continue."
              : "Two-factor authentication is enabled for this account."}
          </CardDescription>
        </CardHeader>
        <CardContent className="px-6">
          {formError && (
            <Alert variant={formError.setupRequired ? "info" : "destructive"} className="mb-4">
              <CircleAlert />
              <AlertTitle>
                {formError.setupRequired ? "No admin account yet" : "Couldn't log in"}
              </AlertTitle>
              <AlertDescription>
                <p>{formError.message}</p>
                {formError.setupRequired && (
                  <Button type="button" variant="outline" size="sm" onClick={goToSetup}>
                    Go to setup
                  </Button>
                )}
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
              {step === "credentials" ? (
                <>
                  <FormField
                    control={form.control}
                    name="username"
                    rules={{ required: "Username is required" }}
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
                    rules={{ required: "Password is required" }}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Password</FormLabel>
                        <FormControl>
                          <Input {...field} type="password" autoComplete="current-password" />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <Button type="submit" disabled={login.isPending}>
                    {login.isPending ? "Logging in…" : "Log in"}
                  </Button>
                </>
              ) : (
                <>
                  <p className="text-sm text-muted-foreground">
                    Signing in as{" "}
                    <span className="font-medium text-foreground">
                      {form.getValues("username")}
                    </span>
                    .
                  </p>
                  <FormField
                    control={form.control}
                    name="totpCode"
                    rules={{
                      validate: (value) => value.length === 6 || "Enter the 6-digit code",
                    }}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Verification code</FormLabel>
                        <FormControl>
                          <InputOTP maxLength={6} {...field}>
                            <InputOTPGroup>
                              <InputOTPSlot index={0} />
                              <InputOTPSlot index={1} />
                              <InputOTPSlot index={2} />
                            </InputOTPGroup>
                            <InputOTPSeparator />
                            <InputOTPGroup>
                              <InputOTPSlot index={3} />
                              <InputOTPSlot index={4} />
                              <InputOTPSlot index={5} />
                            </InputOTPGroup>
                          </InputOTP>
                        </FormControl>
                        <FormDescription>
                          Enter the 6-digit code from your authenticator app.
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <div className="flex items-center justify-between gap-3">
                    <Button type="button" variant="ghost" onClick={backToCredentials}>
                      Back
                    </Button>
                    <Button type="submit" disabled={login.isPending}>
                      {login.isPending ? "Verifying…" : "Verify"}
                    </Button>
                  </div>
                </>
              )}
            </form>
          </Form>
        </CardContent>
      </Card>
    </AuthLayout>
  );
}
