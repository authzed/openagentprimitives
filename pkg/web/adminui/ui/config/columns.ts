// Per-resource column specs for the generic ResourceTable. The Name (+namespace
// sub), Status pill, and Manage cells are ALWAYS rendered by the table itself;
// a ColumnSpec configures only the middle columns — which badge values or count
// rollups a given config kind surfaces. Derived from the prototype's per-table
// columns (renderConfigTools/renderSkills/renderSources/… in the design HTML)
// and the projector badge/count keys in pkg/web/admind/config/projectors.

import type { ReactNode } from "react";
import type { ResourceRow } from "../lib/api";

export interface ColumnSpec {
  // Column header text.
  header: string;
  // Where the cell value comes from: a row badge, a row count, or the scope.
  source: "badge" | "count" | "scope";
  // For badge columns, the badge key to surface (every matching badge renders
  // as a chip — e.g. multiple "cred" badges). Omitted → all badges.
  // For count columns, the count label to surface. Omitted → the first count.
  key?: string;
  // Numeric/right-aligned columns (counts) set this.
  align?: "left" | "right";
  // colorize (badge columns only) renders each value as a KindBadge — a stable
  // per-value color dot + label (via lib/KindBadge → lib/ordinalColor) so the
  // same kind reads the same color everywhere in the console. Used on the
  // genuine "kind" columns (tools / channels / skills / sources / providers Kind,
  // identities credential kind).
  colorize?: boolean;
  // Optional custom cell renderer. When set it fully replaces the default
  // badge/count/scope projection for this column — used where a cell needs a
  // link or conditional coloring the generic projection can't express (the
  // Channels role chip / agentClass link, the Users decoded-subject cell). The
  // render function is supplied by the .tsx view that owns the columns.
  render?: (row: ResourceRow) => ReactNode;
}

// COLUMNS maps a config slug to its middle-column layout. agents renders as a
// card grid by default (AgentsView), but its table-view toggle reuses the
// generic table with this column set.
export const COLUMNS: Record<string, ColumnSpec[]> = {
  agents: [
    { header: "Identity", source: "badge", key: "identityMode" },
    { header: "Tools", source: "count", key: "tools", align: "right" },
    { header: "Skills", source: "count", key: "skills", align: "right" },
  ],
  tools: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    // "Capabilities" stays neutral across kinds: observed tools for MCP/sidecar
    // rows, subcommands for toolkit/toolspec rows. Value is still counts[0].
    { header: "Capabilities", source: "count", align: "right" },
  ],
  skills: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    { header: "Delivery", source: "badge", key: "delivery" },
    { header: "Pin", source: "badge", key: "pin" },
  ],
  sources: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    { header: "Repo", source: "badge", key: "repo" },
    { header: "Ref", source: "badge", key: "ref" },
    { header: "Skills", source: "count", key: "discoveredSkills", align: "right" },
  ],
  directory: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    { header: "Credential", source: "badge", key: "credential" },
    { header: "Scopes", source: "count", key: "scopes", align: "right" },
    { header: "Written", source: "count", key: "written", align: "right" },
    { header: "Join misses", source: "count", key: "joinMisses", align: "right" },
    // Scopes processed can read 156 while every one of them failed; nothing
    // else on this row tells those two apart.
    { header: "Errors", source: "count", key: "errors", align: "right" },
  ],
  channels: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    { header: "Role", source: "badge", key: "role" },
    { header: "Agent class", source: "badge", key: "agentClass" },
  ],
  users: [
    { header: "Subject", source: "badge", key: "subject" },
    { header: "Credentials", source: "count", key: "availableCredentials", align: "right" },
    { header: "Resolved", source: "count", key: "resolved", align: "right" },
  ],
  identities: [
    { header: "Credential kinds", source: "badge", key: "cred", colorize: true },
    { header: "Credentials", source: "count", key: "credentials", align: "right" },
    { header: "Resolved", source: "count", key: "resolved", align: "right" },
  ],
  providers: [
    { header: "Kind", source: "badge", key: "kind", colorize: true },
    { header: "Issuer", source: "badge", key: "issuer" },
  ],
};
