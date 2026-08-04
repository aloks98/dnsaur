import { Card, CardContent, CardDescription, CardHeader } from "@e412/rnui-react";

// Placeholder — Task 5 replaces this with the real first-run setup flow.
export function Setup() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <h1 className="text-2xl font-heading font-semibold text-foreground">Set up dnsaur</h1>
          <CardDescription>Create the first admin account to get started.</CardDescription>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">
            The setup flow lands in a later task. This page confirms routing works.
          </p>
        </CardContent>
      </Card>
    </main>
  );
}
