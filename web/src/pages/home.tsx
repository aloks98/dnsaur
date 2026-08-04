import { Card, CardContent, CardDescription, CardHeader } from "@e412/rnui-react";

export function Home() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <h1 className="text-2xl font-heading font-semibold text-foreground">dnsaur</h1>
          <CardDescription>Self-hosted DNS, filtering, and observability.</CardDescription>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">
            The dashboard is warming up. Check back soon.
          </p>
        </CardContent>
      </Card>
    </main>
  );
}
