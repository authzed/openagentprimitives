import { ResourceSection } from "./ResourceSection";
import { COLUMNS } from "./columns";

export function ToolsView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="tools"
      entity="tool"
      columns={COLUMNS.tools}
      note="MCPServer · SidecarToolbox · SpiceboxToolkit · SpiceboxToolspec — managed via oap tools / kubectl"
      emptyText="No tools configured yet."
    />
  );
}
