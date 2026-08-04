/**
 * A minimal EventSource test double.
 *
 * jsdom does not implement EventSource at all, and there's no realistic way
 * to drive a genuine server-sent-events stream through MSW either — so
 * sse.ts's own tests, and any page test exercising the live query tail,
 * drive this directly instead of a real network stream.
 *
 * Behaves like the real thing in the one way that matters for correctness:
 * once closed, it stops delivering anything, mirroring a real EventSource
 * whose readyState has moved to CLOSED.
 */
export class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly url: string;
  closed = false;
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  emitOpen(): void {
    if (this.closed) return;
    this.onopen?.();
  }

  emit(data: unknown): void {
    if (this.closed) return;
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent<string>);
  }

  emitRaw(data: string): void {
    if (this.closed) return;
    this.onmessage?.({ data } as MessageEvent<string>);
  }

  emitError(): void {
    if (this.closed) return;
    this.onerror?.();
  }

  close(): void {
    this.closed = true;
  }
}
