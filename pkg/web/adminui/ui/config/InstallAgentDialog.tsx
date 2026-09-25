import * as React from "react";
import {
  Button, Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger,
} from "@ap/design";
import { navigate } from "../lib/router";
import type { OapInstallResult } from "../lib/api";
import { InstallAgentForm } from "./InstallAgentForm";

function InstalledDependencies({ agents }: { agents: OapInstallResult[] }) {
  return (
    <ul className="flex flex-col gap-2 pl-4 text-xs text-muted-foreground">
      {agents.map((agent) => (
        <li key={`${agent.agentPath ?? ""}/${agent.name}`}>
          <p>{agent.agentPath ? `Dependency ${agent.agentPath}` : "Dependency"}</p>
          <p className="font-mono text-foreground">{agent.name}</p>
          {agent.agents && agent.agents.length > 0 && <InstalledDependencies agents={agent.agents} />}
        </li>
      ))}
    </ul>
  );
}

// InstallSuccess is the post-install confirmation: the resolved name, the CR
// kinds actually applied, how many Secrets were created, any pre-existing
// objects this install adopted (seized rather than created — see
// OapInstallResult.adopted's doc comment: adoption is recorded NOWHERE on the
// object itself, so this render is one of the few durable records that it
// happened), and any non-fatal warnings (a Kind allowlist drop, an absent
// shared dependency) that were still worth applying around — kept visible in
// the dialog rather than an auto-dismissing toast, since either one often
// needs a follow-up action.
function InstallSuccess({
  result, namespace, onClose,
}: {
  result: OapInstallResult;
  namespace: string;
  onClose: () => void;
}) {
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-success">
        Installed <span className="font-mono">{result.name}</span> — {result.appliedKinds.join(", ")}
        {result.secretsCreated > 0
          ? `, ${result.secretsCreated} secret${result.secretsCreated === 1 ? "" : "s"} created`
          : ""}
        .
      </p>
      {result.adopted && result.adopted.length > 0 && (
        <div className="flex flex-col gap-1 rounded-md border border-warning/40 bg-warning/5 p-3">
          <p className="text-xs font-medium text-warning">
            This install seized {result.adopted.length} pre-existing object{result.adopted.length === 1 ? "" : "s"}{" "}
            not created by it — their specs were overwritten with this bundle&apos;s, and{" "}
            <span className="font-mono">oap agent uninstall</span> will delete them.
          </p>
          <ul className="list-disc pl-4 text-xs text-muted-foreground">
            {result.adopted.map((key) => (
              <li key={key} className="font-mono">
                {key}
              </li>
            ))}
          </ul>
        </div>
      )}
      {result.warnings && result.warnings.length > 0 && (
        <ul className="list-disc pl-4 text-xs text-muted-foreground">
          {result.warnings.map((w) => (
            <li key={w}>{w}</li>
          ))}
        </ul>
      )}
      {result.agents && result.agents.length > 0 && (
        <div className="flex flex-col gap-1 rounded-md border border-border p-3">
          <p className="text-xs font-medium text-foreground">Installed dependencies</p>
          <InstalledDependencies agents={result.agents} />
        </div>
      )}
      <DialogFooter>
        <Button variant="outline" onClick={onClose}>
          Close
        </Button>
        <Button
          onClick={() => {
            onClose();
            navigate({ type: "detail", entity: "agent", id: `${namespace}/${result.name}` });
          }}
        >
          View agent
        </Button>
      </DialogFooter>
    </div>
  );
}

export interface InstallAgentDialogProps {
  apiBase: string;
  // onInstalled fires on every successful install (in addition to switching
  // the dialog to InstallSuccess) — AgentsView wires this to its own list
  // refetch so a newly installed AgentClass shows up without a manual reload.
  onInstalled?: () => void;
}

// InstallAgentDialog is the `install_agent` entry point shown on the Agents
// list: a trigger button opens a modal carrying InstallAgentForm, which flips
// to InstallSuccess on a 200. The form itself has no Dialog-specific
// mechanics (see InstallAgentForm's doc comment) — this wrapper owns only
// open/result state, matching the Kill-session Dialog pattern in SessionPage.
export function InstallAgentDialog({ apiBase, onInstalled }: InstallAgentDialogProps) {
  const [open, setOpen] = React.useState(false);
  const [success, setSuccess] = React.useState<{ result: OapInstallResult; namespace: string } | null>(null);

  // Reopening after a prior install starts a fresh form rather than showing
  // the last run's success screen.
  const onOpenChange = (o: boolean) => {
    setOpen(o);
    if (o) setSuccess(null);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          Install agent
        </Button>
      </DialogTrigger>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Install an agent</DialogTitle>
          <DialogDescription>Upload a packaged .oap bundle or pull one from a registry ref.</DialogDescription>
        </DialogHeader>
        {success ? (
          <InstallSuccess result={success.result} namespace={success.namespace} onClose={() => setOpen(false)} />
        ) : (
          <InstallAgentForm
            apiBase={apiBase}
            onInstalled={(result, namespace) => {
              setSuccess({ result, namespace });
              onInstalled?.();
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}
