import { Alert, AlertDescription, AlertTitle, Button } from "@e412/rnui-react";
import { WifiOff } from "lucide-react";

interface ApiUnreachableBannerProps {
  onRetry?: () => void;
}

/**
 * Shown when the dnsaur API cannot be reached at all (as opposed to a normal
 * 401 "please log in" response) — e.g. the backend is down or the reverse
 * proxy is misconfigured. Distinct from the auth gate's Login/Setup branches,
 * which assume the API answered.
 */
export function ApiUnreachableBanner({ onRetry }: ApiUnreachableBannerProps) {
  return (
    <Alert variant="destructive">
      <WifiOff />
      <AlertTitle>Can&apos;t reach dnsaur</AlertTitle>
      <AlertDescription>
        <p>The API didn&apos;t respond. Check that the dnsaur server is running.</p>
        {onRetry && (
          <Button type="button" variant="outline" size="sm" onClick={onRetry}>
            Retry
          </Button>
        )}
      </AlertDescription>
    </Alert>
  );
}
