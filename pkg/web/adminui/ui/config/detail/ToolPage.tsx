import { ConfigDetail } from "./ConfigDetail";

// ToolPage is the detail for MCPServer / SidecarToolbox / SpiceboxToolkit /
// SpiceboxToolspec. Tabs = Overview + the backend sections (Connection/Command,
// Tools/Subcommands, Policy, Observed tools, and Health when degraded — the
// degraded box surfaces the FULL statusReason, e.g. an unreachable MCP endpoint).
export function ToolPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="tools"
      entity="tool"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "tools" }}
    />
  );
}
