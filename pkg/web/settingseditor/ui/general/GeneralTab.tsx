import * as React from "react";
import { Button, Input, Label, Alert, AlertDescription } from "@ap/design";
import {
  getConfig,
  putConfig,
  useKubectl,
  revealConfig,
  revealLogs,
  ApiError,
  type ConfigResponse,
  type FieldOutcome,
} from "../lib/api";
import { OutcomeList } from "../lib/OutcomeList";

export interface GeneralTabProps {
  apiBase: string;
  running: boolean;
  kubectlCurrent: boolean;
}

// MIN_PASSWORD_LENGTH mirrors settingsui's minSettingsPassword (config.go) —
// duplicated here only as a client-side hint/early-block, never the source
// of truth: the server re-checks this on every PUT regardless of what the
// client sent.
const MIN_PASSWORD_LENGTH = 8;

// GeneralTab is the settings app's General tab: the health-notifications
// toggle, the admin password-change form, the kubectl-context section, and
// the reveal-in-Finder actions. The toggle and password form share ONE Save
// action because PUT /api/config is whole-document — see handleSave's
// fetch-then-merge comment for why. kubectl-use and the reveal buttons are
// each their own fire-and-forget action, independent of Save.
export function GeneralTab({ apiBase, running, kubectlCurrent }: GeneralTabProps) {
  const [config, setConfig] = React.useState<ConfigResponse | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const [healthEnabled, setHealthEnabled] = React.useState(true);
  const [currentPassword, setCurrentPassword] = React.useState("");
  const [newPassword, setNewPassword] = React.useState("");
  const [confirmPassword, setConfirmPassword] = React.useState("");

  const [formError, setFormError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [outcomes, setOutcomes] = React.useState<FieldOutcome[]>([]);

  const [kubectlBusy, setKubectlBusy] = React.useState(false);
  const [kubectlError, setKubectlError] = React.useState<string | null>(null);
  const [revealError, setRevealError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let closed = false;
    getConfig(apiBase)
      .then((c) => {
        if (closed) return;
        setConfig(c);
        setHealthEnabled(c.healthNotifications);
      })
      .catch((e: Error) => {
        if (!closed) setLoadError(e.message);
      });
    return () => {
      closed = true;
    };
  }, [apiBase]);

  async function handleSave() {
    setFormError(null);
    setOutcomes([]);

    const changingPassword = newPassword !== "" || confirmPassword !== "";
    if (changingPassword) {
      if (newPassword.length < MIN_PASSWORD_LENGTH) {
        setFormError(`New password must be at least ${MIN_PASSWORD_LENGTH} characters.`);
        return;
      }
      if (newPassword !== confirmPassword) {
        setFormError("New password and confirmation do not match.");
        return;
      }
    }

    setSaving(true);
    try {
      // Fetch-then-merge: PUT /api/config is whole-document, and this tab
      // only owns health/password — the model and ngrok groups belong to
      // ModelTab, so re-fetch the latest snapshot right before building the
      // request and carry those fields back unchanged rather than the stale
      // copy this tab loaded on mount.
      const latest = await getConfig(apiBase);
      const resp = await putConfig(apiBase, {
        model: { provider: latest.model.provider, apiKey: "", name: latest.model.name },
        ngrokAuthToken: "",
        clearNgrok: false,
        healthNotifications: healthEnabled,
        password: changingPassword ? { current: currentPassword, new: newPassword } : undefined,
      });
      setOutcomes(resp.outcomes);
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
      const refreshed = await getConfig(apiBase);
      setConfig(refreshed);
    } catch (e) {
      setFormError(e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  async function handleKubectlUse() {
    setKubectlError(null);
    setKubectlBusy(true);
    try {
      await useKubectl(apiBase);
    } catch (e) {
      setKubectlError(e instanceof Error ? e.message : String(e));
    } finally {
      setKubectlBusy(false);
    }
  }

  async function handleReveal(action: (apiBase: string) => Promise<void>) {
    setRevealError(null);
    try {
      await action(apiBase);
    } catch (e) {
      setRevealError(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <h2 className="text-base font-semibold">General settings</h2>

      {loadError && (
        <Alert variant="destructive">
          <AlertDescription>{loadError}</AlertDescription>
        </Alert>
      )}

      <section className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <input
            id="general-health-notifications"
            type="checkbox"
            checked={healthEnabled}
            onChange={(e) => setHealthEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-border"
          />
          <Label htmlFor="general-health-notifications">Notify on cluster health issues</Label>
        </div>
      </section>

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-medium">Admin password</h3>
        <div className="flex flex-col gap-1">
          <Label htmlFor="general-current-password">Current password</Label>
          <Input
            id="general-current-password"
            type="password"
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
          />
        </div>
        <div className="flex flex-col gap-1">
          <Label htmlFor="general-new-password">New password</Label>
          <Input
            id="general-new-password"
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
          <p className="text-xs text-muted-foreground">Must be at least {MIN_PASSWORD_LENGTH} characters.</p>
        </div>
        <div className="flex flex-col gap-1">
          <Label htmlFor="general-confirm-password">Confirm new password</Label>
          <Input
            id="general-confirm-password"
            type="password"
            value={confirmPassword}
            onChange={(e) => setConfirmPassword(e.target.value)}
          />
        </div>
      </section>

      {formError && (
        <Alert variant="destructive">
          <AlertDescription>{formError}</AlertDescription>
        </Alert>
      )}

      <OutcomeList outcomes={outcomes} />

      <div>
        <Button onClick={() => void handleSave()} disabled={saving || !config}>
          {saving ? "Saving…" : "Save general settings"}
        </Button>
      </div>

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-medium">kubectl</h3>
        <p className="text-sm text-muted-foreground">
          {kubectlCurrent ? "kubectl is pointing at this cluster." : "kubectl is not pointing at this cluster."}
        </p>
        <div>
          <Button
            variant="outline"
            onClick={() => void handleKubectlUse()}
            disabled={!running || kubectlBusy}
            title={!running ? "Start the cluster first." : undefined}
          >
            {kubectlBusy ? "Switching…" : "Point kubectl at this cluster"}
          </Button>
          {!running && <p className="text-xs text-muted-foreground">Start the cluster first.</p>}
        </div>
        {kubectlError && (
          <Alert variant="destructive">
            <AlertDescription>{kubectlError}</AlertDescription>
          </Alert>
        )}
      </section>

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-medium">Diagnostics</h3>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => void handleReveal(revealConfig)}>
            Reveal config file
          </Button>
          <Button variant="outline" onClick={() => void handleReveal(revealLogs)}>
            View logs
          </Button>
        </div>
        {revealError && (
          <Alert variant="destructive">
            <AlertDescription>{revealError}</AlertDescription>
          </Alert>
        )}
      </section>
    </div>
  );
}
