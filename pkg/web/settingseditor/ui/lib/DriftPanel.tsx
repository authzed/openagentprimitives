import { Alert, AlertDescription, Button } from "@ap/design";

export interface DriftPanelProps {
  drift: string[];
  onTakeOwnership: () => void;
  busy?: boolean;
  // afterTakeOwnership marks that `drift` is what came back from a
  // TakeOwnership re-PUT (a whole-spec REPLACE), not the ordinary SSA apply.
  // Drift surviving THAT write can't be fixed by retrying the same button —
  // a defaulting webhook re-adding the fields is the likely explanation — so
  // the panel swaps the retry button for an informational warning instead of
  // offering a button that would just repeat the same write.
  afterTakeOwnership?: boolean;
}

// DriftPanel renders the path-level diff a PUT /cluster/settings response
// carries after the server reads the object back and diffs it against the
// intended spec: SSA cannot remove a field a DIFFERENT field manager owns,
// so an ordinary save can leave the cluster differing from the saved spec at
// specific paths. Shared between ClusterTab and AdvancedTab — both PUT the
// same clusterSettingsUpdateResponse shape, just with a spec vs. a yaml body.
export function DriftPanel({ drift, onTakeOwnership, busy, afterTakeOwnership }: DriftPanelProps) {
  if (afterTakeOwnership) {
    return (
      <Alert>
        <AlertDescription className="flex flex-col gap-2">
          <p>
            Drift remains even after taking full ownership at these paths — a defaulting webhook may be re-adding
            them:
          </p>
          <ul className="list-inside list-disc text-xs">
            {drift.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <Alert>
      <AlertDescription className="flex flex-col gap-2">
        <p>
          The saved spec differs from the cluster&apos;s at these paths — a different field manager may own them
          (SSA cannot remove a field it doesn&apos;t own by omission):
        </p>
        <ul className="list-inside list-disc text-xs">
          {drift.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
        <div>
          <Button size="sm" onClick={onTakeOwnership} disabled={busy}>
            Take full ownership and re-apply
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
