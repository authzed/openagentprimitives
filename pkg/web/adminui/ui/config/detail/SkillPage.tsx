import { ConfigDetail } from "./ConfigDetail";

// SkillPage is the Skill / ClusterSkill detail. Tabs = Overview (canonical/
// display/delivery/pin) + Source (repo/ref/resolved SHA, linked to its Source
// CR) + the SKILL.md body (text).
export function SkillPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="skills"
      entity="skill"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "skills" }}
    />
  );
}
