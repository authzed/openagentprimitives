import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { ArtifactTracks, buildTrackModel, type LaneSegment } from "./ArtifactTracks";
import { ordinalColor } from "../lib/ordinalColor";
import type { ArtifactRow } from "../lib/api";

afterEach(cleanup);

function row(name: string, artifactId: string, created: string): ArtifactRow {
  return {
    name,
    namespace: "default",
    artifactId,
    session: "default/s1",
    kind: "html",
    phase: "Ready",
    mime: "text/html",
    size: 10,
    revisions: 2,
    created,
  };
}

// Two logical artifacts (art-a, art-b), two revisions each, interleaved
// newest-first: A2, B2, A1, B1. art-a spans rows 0..2, art-b spans rows 1..3,
// so each track "passes through" a row of the other (row1 lane0, row2 lane1).
const A2 = row("ar-a2", "art-a", "2026-06-10T14:00:00Z");
const B2 = row("ar-b2", "art-b", "2026-06-10T13:00:00Z");
const A1 = row("ar-a1", "art-a", "2026-06-10T12:00:00Z");
const B1 = row("ar-b1", "art-b", "2026-06-10T11:00:00Z");
const fixture: ArtifactRow[] = [A2, B2, A1, B1];

function seg(row: { segments: LaneSegment[] }, lane: number): LaneSegment | undefined {
  return row.segments.find((s) => s.lane === lane);
}

describe("buildTrackModel", () => {
  const model = buildTrackModel(fixture);

  it("gives revisions of one artifact the same lane, distinct artifacts distinct lanes", () => {
    // art-a claims lane 0 (first appearance), art-b lane 1.
    expect(model.rows[0].lane).toBe(0); // A2
    expect(model.rows[2].lane).toBe(0); // A1 — same track as A2
    expect(model.rows[1].lane).toBe(1); // B2
    expect(model.rows[3].lane).toBe(1); // B1
    expect(model.laneCount).toBe(2);
  });

  it("colors each track by ordinalColor(namespace/artifactId), distinct per artifact", () => {
    expect(model.rows[0].color).toBe(ordinalColor("default/art-a"));
    expect(model.rows[2].color).toBe(ordinalColor("default/art-a"));
    expect(model.rows[1].color).toBe(ordinalColor("default/art-b"));
    // art-a (teal) and art-b (pink) hash to different palette entries.
    expect(model.rows[0].color).not.toBe(model.rows[1].color);
  });

  it("marks the newest revision of each track as latest", () => {
    expect(model.rows[0].isLatest).toBe(true); // A2 newest of art-a
    expect(model.rows[2].isLatest).toBe(false); // A1 older
    expect(model.rows[1].isLatest).toBe(true); // B2 newest of art-b
    expect(model.rows[3].isLatest).toBe(false); // B1 older
  });

  it("draws a dot on each row's own lane with a connecting line to the next revision", () => {
    // A2 (top of art-a): dot on lane0 + line down to A1.
    const a2l0 = seg(model.rows[0], 0)!;
    expect(a2l0.dot).toBe(true);
    expect(a2l0.down).toBe(true);
    expect(a2l0.up).toBe(false);
    // A1 (bottom of art-a): dot on lane0 + line up from A2.
    const a1l0 = seg(model.rows[2], 0)!;
    expect(a1l0.dot).toBe(true);
    expect(a1l0.up).toBe(true);
    expect(a1l0.down).toBe(false);
  });

  it("draws a pass-through line (up+down, no dot) for a track spanning non-adjacent rows", () => {
    // art-a spans rows 0..2, so at row1 (B2) lane0 is a pass-through line.
    const passA = seg(model.rows[1], 0)!;
    expect(passA.up).toBe(true);
    expect(passA.down).toBe(true);
    expect(passA.dot).toBe(false);
    // art-b spans rows 1..3, so at row2 (A1) lane1 is a pass-through line.
    const passB = seg(model.rows[2], 1)!;
    expect(passB.up).toBe(true);
    expect(passB.down).toBe(true);
    expect(passB.dot).toBe(false);
    // No stray segment where a track has not started / has ended.
    expect(seg(model.rows[0], 1)).toBeUndefined(); // art-b not started at row0
    expect(seg(model.rows[3], 0)).toBeUndefined(); // art-a ended before row3
  });
});

// Two logical artifacts that do NOT overlap: art-a fully above art-b (art-a rows
// 0..1, art-b rows 2..3). With git-graph lane reuse art-b reclaims art-a's freed
// lane 0 instead of burning a second column.
const RA2 = row("ar-a2r", "art-a", "2026-06-10T14:00:00Z");
const RA1 = row("ar-a1r", "art-a", "2026-06-10T13:00:00Z");
const RB2 = row("ar-b2r", "art-b", "2026-06-10T12:00:00Z");
const RB1 = row("ar-b1r", "art-b", "2026-06-10T11:00:00Z");
const reuseFixture: ArtifactRow[] = [RA2, RA1, RB2, RB1];

describe("buildTrackModel lane reuse", () => {
  const model = buildTrackModel(reuseFixture);

  it("packs a non-overlapping second track onto the freed lane 0 (one lane, not two)", () => {
    expect(model.rows[0].lane).toBe(0); // A2
    expect(model.rows[1].lane).toBe(0); // A1
    expect(model.rows[2].lane).toBe(0); // B2 — reuses lane 0 (art-a has ended)
    expect(model.rows[3].lane).toBe(0); // B1
    expect(model.laneCount).toBe(1);
  });

  it("still overlays each track's OWN color on the shared lane (color keyed on track, not lane)", () => {
    expect(model.rows[0].color).toBe(ordinalColor("default/art-a"));
    expect(model.rows[2].color).toBe(ordinalColor("default/art-b"));
    // Distinct artifacts keep distinct colors even sharing a lane.
    expect(model.rows[0].color).not.toBe(model.rows[2].color);
    // The dot segment on the shared lane paints the row's own track color.
    expect(seg(model.rows[2], 0)!.color).toBe(ordinalColor("default/art-b"));
  });

  it("does NOT connect the two tracks across the lane handoff", () => {
    // art-a's last row (A1, row1): dot, no down-half. art-b's first row (B2,
    // row2): dot, no up-half — the shared lane reads as two separate branches.
    const a1 = seg(model.rows[1], 0)!;
    expect(a1.dot).toBe(true);
    expect(a1.down).toBe(false);
    const b2 = seg(model.rows[2], 0)!;
    expect(b2.dot).toBe(true);
    expect(b2.up).toBe(false);
    expect(b2.down).toBe(true);
  });
});

// One artifact with three revisions, contiguous rows 0..2 — the span whose line
// must be CONTINUOUS end to end.
const S3 = row("ar-s3", "art-s", "2026-06-10T14:00:00Z");
const S2 = row("ar-s2", "art-s", "2026-06-10T13:00:00Z");
const S1 = row("ar-s1", "art-s", "2026-06-10T12:00:00Z");

describe("buildTrackModel continuous line", () => {
  const model = buildTrackModel([S3, S2, S1]);

  it("draws a connected line across the whole span: top down, middle up+down, bottom up", () => {
    expect(model.laneCount).toBe(1);
    expect(seg(model.rows[0], 0)).toMatchObject({ dot: true, up: false, down: true });
    expect(seg(model.rows[1], 0)).toMatchObject({ dot: true, up: true, down: true });
    expect(seg(model.rows[2], 0)).toMatchObject({ dot: true, up: true, down: false });
  });
});

describe("ArtifactTracks render", () => {
  it("renders an svg and a dot per row without crashing", () => {
    const { container, getAllByTestId } = render(<ArtifactTracks rows={fixture} />);
    expect(container.querySelectorAll("svg").length).toBe(fixture.length);
    // One dot per row (each row belongs to exactly one track).
    expect(getAllByTestId("track-dot").length).toBe(fixture.length);
  });

  it("handles an empty list", () => {
    const { container } = render(<ArtifactTracks rows={[]} />);
    expect(container.querySelectorAll("svg").length).toBe(0);
  });
});
