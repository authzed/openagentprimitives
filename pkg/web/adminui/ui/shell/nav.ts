import type { LucideIcon } from "lucide-react";
import {
  LayoutDashboard, Activity, Terminal, Shield,
  Bot, Wrench, Sparkles, GitBranch, FolderSync, Hash, KeyRound, Users, Lock, SlidersHorizontal, KeySquare,
  ScrollText, History, File, Database, Network, Wallet, Hammer,
} from "lucide-react";

export type ViewId =
  | "overview"
  | "sessions" | "toolcalls" | "approvals" | "workshops"
  | "agents" | "tools" | "skills" | "sources" | "directory" | "channels" | "identity" | "users" | "access" | "tokens" | "settings"
  | "logs" | "sessionsAudit" | "budget" | "artifacts" | "memory" | "knowledge";

export interface NavItem {
  id: ViewId;
  label: string;
  icon: LucideIcon;
}

export interface NavGroup {
  group: string;
  items: NavItem[];
}

export const NAV: NavGroup[] = [
  { group: "", items: [
    { id: "overview", label: "Overview", icon: LayoutDashboard },
  ]},
  { group: "Live", items: [
    { id: "sessions",  label: "Agent sessions",   icon: Activity  },
    { id: "toolcalls", label: "Live tool calls",  icon: Terminal  },
    { id: "approvals", label: "Approvals",         icon: Shield    },
    { id: "workshops", label: "Workshops",         icon: Hammer    },
  ]},
  { group: "Config", items: [
    { id: "agents",    label: "Agents",    icon: Bot              },
    { id: "tools",     label: "Tools",     icon: Wrench           },
    { id: "skills",    label: "Skills",    icon: Sparkles         },
    { id: "sources",   label: "Sources",   icon: GitBranch        },
    { id: "directory", label: "Directory", icon: FolderSync       },
    { id: "channels",  label: "Channels",  icon: Hash             },
    { id: "identity",  label: "Identity",  icon: KeyRound         },
    { id: "users",     label: "Users",     icon: Users            },
    { id: "access",    label: "Access",    icon: Lock             },
    { id: "tokens",    label: "Tokens",    icon: KeySquare        },
    { id: "settings",  label: "Settings",  icon: SlidersHorizontal},
  ]},
  { group: "Audit", items: [
    { id: "logs",         label: "Logs",      icon: ScrollText },
    { id: "sessionsAudit",label: "Sessions",  icon: History   },
    { id: "budget",       label: "Budget",    icon: Wallet    },
    { id: "artifacts",    label: "Artifacts", icon: File      },
    { id: "memory",       label: "Memory",    icon: Database  },
    { id: "knowledge",    label: "Knowledge", icon: Network   },
  ]},
];

export const VIEW_META: Record<ViewId, { title: string; sub: string }> = {
  overview:     { title: "Overview",          sub: "Platform at a glance — usage, spend, and health" },
  sessions:     { title: "Agent sessions",    sub: "Live agent sessions across the platform" },
  toolcalls:    { title: "Live tool calls",   sub: "Tool invocations as they happen, across every session" },
  approvals:    { title: "Approvals",         sub: "Tool calls & leakage gates awaiting a decision" },
  workshops:    { title: "Workshops",         sub: "Agent-builder sessions' drafted bundles — install, decline, or kill" },
  agents:       { title: "Agents",            sub: "Configured AgentClasses and their readiness · oap class / kubectl agentclass" },
  tools:        { title: "Tools",             sub: "MCP servers, sandboxed CLIs, sidecars, toolkits · oap tools" },
  skills:       { title: "Skills",            sub: "Instruction & executable skills available to agents · oap skill" },
  sources:      { title: "Sources",           sub: "Skill sources synced from git · oap skill source" },
  directory:    { title: "Directory",         sub: "Directory syncs feeding SpiceDB — Slack, GitHub, 1Password · oap directory" },
  channels:     { title: "Channels",          sub: "Where agents are reachable · oap channel / kubectl channel" },
  identity:     { title: "Identity",          sub: "Agent identities, credentials, and login providers · oap identity / oap idp" },
  users:        { title: "Users",             sub: "People linked to the platform · oap user-identity" },
  access:       { title: "Access",            sub: "Platform admins & the SpiceDB authorization schema · oap platform / oap spicedb" },
  tokens:       { title: "Tokens",            sub: "Delegated OAuth access tokens minted via MCP consent — view and revoke" },
  settings:     { title: "Settings",          sub: "Cluster configuration — defaults, ceilings, security · oap settings" },
  logs:         { title: "Logs",              sub: "Cross-session audit — approvals, authz, tool calls, scope changes · oap audit" },
  sessionsAudit:{ title: "Sessions",          sub: "Every agent session, current and historical" },
  budget:       { title: "Budget",            sub: "Estimated spend by model, agent, session, user" },
  artifacts:    { title: "Artifacts",         sub: "Rendered outputs agents produced · oap artifact" },
  memory:       { title: "Memory",            sub: "The searchable, authorized memory store · oap memory" },
  knowledge:    { title: "Knowledge",         sub: "The knowledge graph — entities, facts, communities · oap kg" },
};
