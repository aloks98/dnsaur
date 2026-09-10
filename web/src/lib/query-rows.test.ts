import { describe, expect, test } from "vitest";
import type { QueryEntry } from "../api/types";
import { decisionTone, durationLabel, namesByKey, rowKey } from "./query-rows";

function entry(over: Partial<QueryEntry> = {}): QueryEntry {
  return {
    id: 0,
    at: 1785946876523,
    instance_id: "i",
    client_ip: "192.168.1.10",
    client_id: 0,
    q_name: "example.com",
    q_type: "A",
    decision: "forwarded",
    rule_id: 0,
    list_id: 0,
    upstream: "1.1.1.1:53",
    r_code: "NOERROR",
    duration_ms: 0,
    matched: "",
    ...over,
  };
}

describe("rowKey", () => {
  // Every live row carries id 0 — the entry is published to the SSE hub
  // before the batched insert assigns a primary key. Keying on it collapses
  // the whole tail onto one React identity.
  test("gives distinct keys to distinct live rows that all have id 0", () => {
    const a = entry(),
      b = entry(),
      c = entry();
    const keys = new Set([rowKey(a), rowKey(b), rowKey(c)]);
    expect(keys.size).toBe(3);
  });

  test("is stable for the same entry object across renders", () => {
    const e = entry();
    expect(rowKey(e)).toBe(rowKey(e));
  });

  test("uses the real id once the row has one", () => {
    expect(rowKey(entry({ id: 42 }))).toBe("q42");
  });
});

describe("durationLabel", () => {
  // duration_ms is truncated whole milliseconds, so a 180µs cache hit logs
  // as 0. Printing "0" claims an instantaneous resolve.
  test("renders sub-millisecond as <1, not 0", () => {
    expect(durationLabel(0)).toBe("<1");
  });

  test("renders real measurements verbatim", () => {
    expect(durationLabel(24)).toBe("24");
  });
});

describe("namesByKey", () => {
  // A client's name is not validated and may be empty (ui-contract §3.4).
  // An empty string in the map is a hit, so every `?? UNKNOWN` fallback at
  // the call site stops firing and the cell renders blank.
  test("leaves a client with an empty name out of the map", () => {
    const map = namesByKey(
      [
        { id: 1, name: "Laptop" },
        { id: 2, name: "" },
      ],
      (c) => c.id,
    );
    expect(map.get(1)).toBe("Laptop");
    expect(map.has(2)).toBe(false);
  });

  test("takes no clients at all", () => {
    expect(namesByKey(undefined, (c: { id: number; name: string }) => c.id).size).toBe(0);
  });
});

describe("decisionTone", () => {
  // The vocabulary is the resolver's (internal/dnssrv/pipeline.go), and
  // there is no `allowed` in it — a decision this map has never heard of
  // must still render, in the neutral tone.
  test("tones every decision the resolver writes, and falls back for the rest", () => {
    expect(decisionTone("blocked")).toBe("text-destructive");
    expect(decisionTone("error")).toBe("text-destructive");
    expect(decisionTone("stale")).toBe("text-warn");
    expect(decisionTone("cached")).toBe("text-muted-foreground");
    expect(decisionTone("forwarded")).toBe("text-foreground");
    expect(decisionTone("authoritative")).toBe("text-primary");
    expect(decisionTone("teleported")).toBe("text-muted-foreground");
  });
});
