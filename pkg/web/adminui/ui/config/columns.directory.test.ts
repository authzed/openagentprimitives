import { describe, expect, it } from "vitest";
import { COLUMNS } from "./columns";
import fixture from "../../../admind/config/projectors/testdata/directory_column_keys.json";

// columns.ts references the directory projector's badge keys and count
// labels by string, and nothing enforces the match on its own — a renamed
// key silently blanks a column. Three earlier rounds had a Go test
// regex-parse THIS file's TypeScript to check the match, and each round's
// pattern broke on a different reformatting shape (adjacency, an
// intervening property, a nested brace): regex-parsing one language from the
// other was the wrong tool, not any one pattern.
//
// This test and its Go counterpart (TestDirectoryProjectorKeysMatchFixture in
// pkg/web/admind/config/projectors/columns_contract_test.go) instead both
// read the same committed fixture
// (pkg/web/admind/config/projectors/testdata/directory_column_keys.json) —
// the projector's emitted badge keys and count labels — so neither side
// parses the other's language. A key renamed on the Go side without
// updating the fixture fails the Go test; a key columns.ts asks for that
// the fixture doesn't list fails here.
describe("COLUMNS.directory", () => {
  it("only asks for badge/count keys the directory projector actually emits", () => {
    const emitted: Record<"badge" | "count", Set<string>> = {
      badge: new Set(fixture.badges),
      count: new Set(fixture.counts),
    };

    const keyed = COLUMNS.directory.filter((col) => col.key);
    // Keeps this test honest: if the directory entry were ever emptied out or
    // every column made keyless, the loop below would check nothing and pass
    // vacuously.
    expect(keyed.length).toBeGreaterThan(0);

    for (const col of keyed) {
      if (col.source !== "badge" && col.source !== "count") continue; // scope columns carry no key to check
      expect(emitted[col.source].has(col.key as string)).toBe(true);
    }
  });
});
