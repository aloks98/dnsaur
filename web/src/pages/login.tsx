import {
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  Input,
  Label,
} from "@e412/rnui-react";

// Placeholder — Task 6 replaces this with the real login flow wired to
// useLogin(), including validation, TOTP, and error handling.
export function Login() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <h1 className="text-2xl font-heading font-semibold text-foreground">Log in to dnsaur</h1>
          <CardDescription>Enter your credentials to continue.</CardDescription>
        </CardHeader>
        <CardContent>
          <form className="flex flex-col gap-4">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="login-username">Username</Label>
              <Input id="login-username" name="username" type="text" autoComplete="username" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="login-password">Password</Label>
              <Input
                id="login-password"
                name="password"
                type="password"
                autoComplete="current-password"
              />
            </div>
            <Button type="submit">Log in</Button>
          </form>
        </CardContent>
      </Card>
    </main>
  );
}
