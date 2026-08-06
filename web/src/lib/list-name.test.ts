import { expect, test } from "vitest";
import { deriveListName } from "./list-name";

/**
 * The same table of URLs as Go's TestDeriveListName
 * (internal/store/listname_test.go). This file exists to catch the one real
 * hazard of mirroring the rule in two languages: the Add-list dialog
 * promising one default while the server stores another.
 */
test("derives the same names the server does", () => {
  const cases: [string, string][] = [
    [
      "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/pro.txt",
      "hagezi wildcard/pro.txt",
    ],
    ["https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts", "StevenBlack hosts"],
    [
      "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/hosts/pro.txt",
      "hagezi hosts/pro.txt",
    ],
    [
      "https://github.com/hagezi/dns-blocklists/blob/main/wildcard/pro.txt",
      "hagezi wildcard/pro.txt",
    ],
    ["https://example.com/hosts", "example.com hosts"],
    ["https://www.example.com/lists/ads.txt", "example.com ads.txt"],
    ["https://blocklist.example.org/", "blocklist.example.org"],
    ["https://blocklist.example.org", "blocklist.example.org"],
    // Too short for the owner/repo/ref shape: falls through, never throws.
    ["https://raw.githubusercontent.com/hagezi", "raw.githubusercontent.com hagezi"],
    [
      "https://raw.githubusercontent.com/hagezi/dns-blocklists/main",
      "raw.githubusercontent.com main",
    ],
    ["", "unnamed list"],
    ["::not a url::", "::not a url::"],
  ];
  // Compared as one array rather than in a loop, so a failure's diff names
  // the URL that drifted instead of just reporting two bare strings.
  expect(cases.map(([url]) => [url, deriveListName(url)])).toEqual(cases);
});

test("a derived name stays short enough for a table cell", () => {
  const got = deriveListName(`https://example.com/${"verylongsegment".repeat(20)}`);
  expect([...got].length).toBeLessThanOrEqual(60);
  expect(got.endsWith("…")).toBe(true);
});
