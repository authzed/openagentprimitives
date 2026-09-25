import type { ArtifactRow } from "../lib/api";
import { ordinalColor } from "../lib/ordinalColor";

// ArtifactTracks renders a git-commit-graph "track" visualization for the
// Artifacts table. Every revision of one logical artifact (grouped by
// namespace + artifactId) is a track with a stable COLOR; tracks are packed
// into LANES (column positions) with git-graph compaction, so non-overlapping
// tracks share a lane rather than each burning a fresh column. For each table
// row we draw a filled dot in its track's lane and a continuous vertical line
// spanning that track's revisions — pass-through halves for tracks whose span
// brackets a row — so the graph reads as a set of connected vertical branch
// lines, one dot per revision.

// LANE_W is the pixel width of one lane; LANE_MAX caps how many lanes we draw
// so the column stays compact (extra tracks collapse onto the last lane). DOT_R
// is the dot radius. These keep the column to "a handful of lanes".
export const LANE_W = 14;
export const LANE_MAX = 8;
const DOT_R = 4;

// trackKey groups revisions of one logical artifact: namespace + artifactId.
// Unlabeled renders fall back to their own name (already done backend-side, but
// we defend against a blank artifactId so each render is still its own track).
export function trackKey(a: ArtifactRow): string {
  return `${a.namespace}/${a.artifactId || a.name}`;
}

// artifactUID is a row's unique render identity (namespace/name) — every
// ArtifactRender has a distinct name, so this is a stable, collision-free key.
export function artifactUID(a: ArtifactRow): string {
  return `${a.namespace}/${a.name}`;
}

// latestTrackUIDs returns the set of render UIDs that are the LATEST revision of
// their track. Rows are grouped by trackKey (namespace + artifactId); within a
// track the render with the newest `created` timestamp is the latest. Ties
// break on UID for determinism. Single-revision tracks are trivially latest.
export function latestTrackUIDs(rows: ArtifactRow[]): Set<string> {
  const newest = new Map<string, ArtifactRow>();
  for (const a of rows) {
    const k = trackKey(a);
    const cur = newest.get(k);
    if (cur === undefined) {
      newest.set(k, a);
      continue;
    }
    const at = new Date(a.created).getTime();
    const ct = new Date(cur.created).getTime();
    if (at > ct || (at === ct && artifactUID(a) < artifactUID(cur))) newest.set(k, a);
  }
  return new Set([...newest.values()].map(artifactUID));
}

// LaneSegment is what one lane draws at one row: a line up (top edge → center),
// a line down (center → bottom edge), and/or a filled dot at the center. The
// line halves connect to the adjacent rows' halves, so a track reads as one
// continuous vertical branch line through its lane.
export interface LaneSegment {
  lane: number;
  color: string;
  up: boolean;
  down: boolean;
  dot: boolean;
}

// RowTrack is the per-row render model: the row's own track lane/color, whether
// it is the latest revision of its track, and every lane with anything to draw
// at this row (its own dot plus any pass-through lines).
export interface RowTrack {
  uid: string;
  lane: number;
  color: string;
  isLatest: boolean;
  segments: LaneSegment[];
}

export interface TrackModel {
  laneCount: number; // capped number of lanes actually drawn
  width: number; // pixel width of the tracks column
  rows: RowTrack[]; // one per input row, same order
}

// buildTrackModel computes the git-graph model for an ordered (newest-first) row
// list. Each logical artifact is a TRACK spanning [first, last] row indices.
//
// Lane assignment uses git-graph lane COMPACTION with reuse: walking tracks in
// first-appearance order, each track claims the LOWEST lane whose previous
// occupant has already ENDED (its last row is above this track's first row), so
// two non-overlapping tracks SHARE a lane instead of each burning a fresh one.
// Lanes are clamped to LANE_MAX-1 so the column stays compact (extra concurrent
// tracks collapse onto the last lane).
//
// COLOR is keyed on the TRACK (not the lane), so a reused lane paints each of
// its tracks in that track's own color. For every row a lane emits up/down line
// halves whenever the track occupying it there brackets the row — a CONTINUOUS
// vertical line across the whole span — plus a dot on the row's own track lane.
export function buildTrackModel(rows: ArtifactRow[]): TrackModel {
  // Per-track span (first/last row) and stable color, in first-appearance order
  // (the newest-first list means a track's topmost row is its first appearance).
  const first = new Map<string, number>();
  const last = new Map<string, number>();
  const color = new Map<string, string>();
  const trackOfRow: string[] = [];
  rows.forEach((a, i) => {
    const k = trackKey(a);
    trackOfRow.push(k);
    if (!first.has(k)) {
      first.set(k, i);
      color.set(k, ordinalColor(k) ?? "#888");
    }
    last.set(k, i);
  });

  // laneEnd[trueLane] = last row currently occupying that lane. A track reuses
  // the lowest lane whose occupant ended before the track starts; else a new
  // lane. laneOfTrack records the CLAMPED (rendered) lane.
  const orderedTracks = [...first.keys()];
  const laneEnd: number[] = [];
  const laneOfTrack = new Map<string, number>();
  for (const k of orderedTracks) {
    const f = first.get(k)!;
    let lane = laneEnd.findIndex((end) => end < f);
    if (lane === -1) {
      lane = laneEnd.length;
      laneEnd.push(last.get(k)!);
    } else {
      laneEnd[lane] = last.get(k)!;
    }
    laneOfTrack.set(k, Math.min(lane, LANE_MAX - 1));
  }

  const laneCount = Math.min(laneEnd.length, LANE_MAX);

  // Which track occupies each (rendered lane, row)? Fill each track's rendered
  // lane across its whole [first, last] span. Tracks that share a lane are
  // non-overlapping by construction, so the only collision is on the clamped
  // last lane — resolved first-writer-wins so its color stays stable.
  const at: (string | undefined)[][] = Array.from({ length: laneCount }, () =>
    new Array<string | undefined>(rows.length).fill(undefined),
  );
  for (const k of orderedTracks) {
    const lane = laneOfTrack.get(k)!;
    for (let i = first.get(k)!; i <= last.get(k)!; i++) {
      if (at[lane][i] === undefined) at[lane][i] = k;
    }
  }

  const latest = latestTrackUIDs(rows);

  const out: RowTrack[] = rows.map((a, i) => {
    const ownKey = trackOfRow[i];
    const ownLane = laneOfTrack.get(ownKey)!;
    const segments: LaneSegment[] = [];
    for (let lane = 0; lane < laneCount; lane++) {
      const k = at[lane][i];
      const dot = lane === ownLane;
      if (k === undefined && !dot) continue;
      // The line halves connect to the adjacent rows of the track occupying this
      // lane here; present whenever that track extends above/below this row.
      const up = k !== undefined && i > first.get(k)!;
      const down = k !== undefined && i < last.get(k)!;
      segments.push({ lane, color: dot ? color.get(ownKey)! : color.get(k!)!, up, down, dot });
    }
    return {
      uid: artifactUID(a),
      lane: ownLane,
      color: color.get(ownKey)!,
      isLatest: latest.has(artifactUID(a)),
      segments,
    };
  });

  return { laneCount, width: Math.max(1, laneCount) * LANE_W, rows: out };
}

// laneX is the horizontal center of a lane in pixels.
function laneX(lane: number): number {
  return lane * LANE_W + LANE_W / 2;
}

// TrackCell renders ONE row of the git-graph inside a table cell. The vertical
// lines live in an SVG stretched to fill the cell height (preserveAspectRatio
// "none"), so each row's line halves meet the next row's exactly — no gaps
// between revisions. The dots are overlaid as absolutely-positioned round spans
// (kept circular, unaffected by the SVG's vertical stretch) at the lane center.
export function TrackCell({ row, width }: { row: RowTrack; width: number }) {
  return (
    <div className="relative h-full min-h-[1.75rem]" style={{ width }}>
      <svg
        className="absolute inset-0 h-full w-full"
        viewBox={`0 0 ${width} 10`}
        preserveAspectRatio="none"
        aria-hidden="true"
      >
        {row.segments.map((s) => {
          const x = laneX(s.lane);
          return (
            <g key={s.lane}>
              {s.up && <line x1={x} y1={0} x2={x} y2={5} stroke={s.color} strokeWidth={1.5} strokeOpacity={0.7} />}
              {s.down && <line x1={x} y1={5} x2={x} y2={10} stroke={s.color} strokeWidth={1.5} strokeOpacity={0.7} />}
            </g>
          );
        })}
      </svg>
      <div className="absolute inset-0 flex items-center">
        {row.segments
          .filter((s) => s.dot)
          .map((s) => (
            <span
              key={s.lane}
              className="absolute rounded-full border"
              style={{
                left: laneX(s.lane) - DOT_R,
                width: DOT_R * 2,
                height: DOT_R * 2,
                backgroundColor: s.color,
                borderColor: "hsl(var(--card))",
                // The latest revision reads slightly bolder than older ones.
                opacity: row.isLatest ? 1 : 0.55,
              }}
              data-lane={s.lane}
              data-testid="track-dot"
            />
          ))}
      </div>
    </div>
  );
}

// ArtifactTracks renders a standalone vertical stack of the git-graph (used in
// isolation / smoke tests). In the Artifacts table, callers instead render one
// TrackCell per table row via buildTrackModel so the graph aligns with rows.
export function ArtifactTracks({ rows }: { rows: ArtifactRow[] }) {
  const model = buildTrackModel(rows);
  return (
    <div role="img" aria-label="artifact tracks">
      {model.rows.map((r) => (
        <div key={r.uid} className="h-7">
          <TrackCell row={r} width={model.width} />
        </div>
      ))}
    </div>
  );
}
