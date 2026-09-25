import type { Story } from "../runtime/story";
import { reviewbotPR } from "./reviewbot-pr";
import { multiplayerApproval } from "./multiplayer-approval";
import { realBlocks } from "./real-blocks";
import { codebotPlangate } from "./codebot-plangate";
import { sharedPermissions } from "./shared-permissions";
import { identityChoice } from "./identity-choice";
import { agentDefinition } from "./agent-definition";
import { safeTools } from "./safe-tools";
import { channelsSlack } from "./channels-slack";
import { memoryArtifact } from "./memory-artifact";
import { infoLeakageDemo } from "./info-leakage";
import { queuedReply } from "./queued-reply";
import { artifactRevise } from "./artifact-revise";
import { threadAdoption } from "./thread-adoption";

// The registry of built-in slacksim stories. The capture engine and the dev
// server resolve a story by name (?scenario=<name>). A story is a scenario plus
// ordered beats that advance it.
export const STORIES: Record<string, () => Story> = {
  "reviewbot-pr": reviewbotPR,
  "multiplayer-approval": multiplayerApproval,
  "real-blocks": realBlocks,
  "codebot-plangate": codebotPlangate,
  "shared-permissions": sharedPermissions,
  "identity-choice": identityChoice,
  "agent-definition": agentDefinition,
  "safe-tools": safeTools,
  "channels-slack": channelsSlack,
  "memory-artifact": memoryArtifact,
  "info-leakage": infoLeakageDemo,
  "queued-reply": queuedReply,
  "artifact-revise": artifactRevise,
  "thread-adoption": threadAdoption,
};

export const DEFAULT_SCENARIO = "reviewbot-pr";

export function loadStory(name: string | null): Story {
  const key = name && name in STORIES ? name : DEFAULT_SCENARIO;
  return STORIES[key]();
}
