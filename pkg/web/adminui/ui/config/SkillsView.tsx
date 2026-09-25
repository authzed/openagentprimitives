import { ResourceSection } from "./ResourceSection";
import { COLUMNS } from "./columns";

export function SkillsView({ apiBase }: { apiBase: string }) {
  return (
    <ResourceSection
      apiBase={apiBase}
      resource="skills"
      entity="skill"
      columns={COLUMNS.skills}
      note="Skill · ClusterSkill — instruction & executable skills · managed via oap skill / kubectl get skills"
      emptyText="No skills configured yet."
    />
  );
}
