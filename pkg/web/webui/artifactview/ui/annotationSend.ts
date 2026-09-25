import type { Bundle } from "./host/bundle";

export interface InteractRequest {
  kind: "annotation_batch";
  artifactId: string;
  payload: { annotations: unknown[] };
}

// interactRequestFromBundle builds the POST /interact body. artifactId comes from
// the SHELL (which holds it in props and whose value the server re-checks via
// CheckArtifactView) — never from the sandbox bundle. The server mints the Via
// from artifactId and re-checks authorization (see plan D3); the client never
// supplies a via.
export function interactRequestFromBundle(bundle: Bundle, artifactId: string): InteractRequest {
  return {
    kind: "annotation_batch",
    artifactId,
    payload: { annotations: bundle.annotations },
  };
}
