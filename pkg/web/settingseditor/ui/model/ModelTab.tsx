import * as React from "react";
import {
  Button,
  Input,
  Label,
  Alert,
  AlertDescription,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@ap/design";
import { getConfig, putConfig, ApiError, type ConfigResponse, type FieldOutcome } from "../lib/api";
import { OutcomeList } from "../lib/OutcomeList";

export interface ModelTabProps {
  apiBase: string;
  running: boolean;
}

// ModelTab is the settings app's Model tab: provider selection, the masked
// API key with a replace-only field, the model-name override, and the
// ngrok auth token (masked-configured indicator + replace/clear). All of it
// PUTs through the same whole-document /api/config as GeneralTab's Save —
// see handleSave's fetch-then-merge comment for why the health field is
// re-fetched rather than assumed.
export function ModelTab({ apiBase, running }: ModelTabProps) {
  const [config, setConfig] = React.useState<ConfigResponse | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const [provider, setProvider] = React.useState("");
  const [apiKeyInput, setApiKeyInput] = React.useState("");
  const [modelName, setModelName] = React.useState("");
  const [ngrokInput, setNgrokInput] = React.useState("");
  const [clearNgrok, setClearNgrok] = React.useState(false);

  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [outcomes, setOutcomes] = React.useState<FieldOutcome[]>([]);

  React.useEffect(() => {
    let closed = false;
    getConfig(apiBase)
      .then((c) => {
        if (closed) return;
        setConfig(c);
        setProvider(c.model.provider);
        setModelName(c.model.name);
      })
      .catch((e: Error) => {
        if (!closed) setLoadError(e.message);
      });
    return () => {
      closed = true;
    };
  }, [apiBase]);

  async function handleSave() {
    setSaveError(null);
    setOutcomes([]);
    setSaving(true);
    try {
      // Fetch-then-merge: PUT /api/config is whole-document, and this tab
      // doesn't own health/password — re-fetch the latest snapshot right
      // before building the request and carry that field back unchanged
      // rather than the stale copy this tab loaded on mount (GeneralTab may
      // have changed it since).
      const latest = await getConfig(apiBase);
      const resp = await putConfig(apiBase, {
        model: { provider, apiKey: apiKeyInput, name: modelName },
        ngrokAuthToken: clearNgrok ? "" : ngrokInput,
        clearNgrok,
        healthNotifications: latest.healthNotifications,
      });
      setOutcomes(resp.outcomes);
      setApiKeyInput("");
      setNgrokInput("");
      setClearNgrok(false);
      const refreshed = await getConfig(apiBase);
      setConfig(refreshed);
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <h2 className="text-base font-semibold">Model settings</h2>

      {loadError && (
        <Alert variant="destructive">
          <AlertDescription>{loadError}</AlertDescription>
        </Alert>
      )}

      {config && (
        <>
          <section className="flex flex-col gap-2">
            <Label htmlFor="model-provider">Provider</Label>
            <Select value={provider} onValueChange={setProvider}>
              <SelectTrigger id="model-provider">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {config.providers.map((p) => (
                  <SelectItem key={p} value={p}>
                    {p}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </section>

          <section className="flex flex-col gap-2">
            <Label htmlFor="model-api-key">API key</Label>
            <p className="text-sm text-muted-foreground">
              {config.model.apiKeyMasked ? config.model.apiKeyMasked : "Not set"}
            </p>
            <Input
              id="model-api-key"
              type="password"
              placeholder="unchanged"
              value={apiKeyInput}
              onChange={(e) => setApiKeyInput(e.target.value)}
            />
          </section>

          <section className="flex flex-col gap-2">
            <Label htmlFor="model-name">Model name</Label>
            <Input id="model-name" value={modelName} onChange={(e) => setModelName(e.target.value)} />
            <p className="text-xs text-muted-foreground">
              Leave blank to use {provider || "the selected provider"}&apos;s recommended default model.
            </p>
          </section>

          <section className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">ngrok</h3>
            <p className="text-sm text-muted-foreground">{config.ngrokConfigured ? "Configured" : "Not configured"}</p>
            <Label htmlFor="model-ngrok-token">ngrok auth token</Label>
            <Input
              id="model-ngrok-token"
              type="password"
              placeholder="unchanged"
              value={ngrokInput}
              onChange={(e) => setNgrokInput(e.target.value)}
              disabled={clearNgrok}
            />
            <div className="flex items-center gap-2">
              <input
                id="model-clear-ngrok"
                type="checkbox"
                checked={clearNgrok}
                onChange={(e) => setClearNgrok(e.target.checked)}
                className="h-4 w-4 rounded border-border"
              />
              <Label htmlFor="model-clear-ngrok">Clear ngrok token</Label>
            </div>
          </section>

          {!running && (
            <p className="text-xs text-muted-foreground">
              Changes apply at the next start — the cluster isn&apos;t running.
            </p>
          )}

          {saveError && (
            <Alert variant="destructive">
              <AlertDescription>{saveError}</AlertDescription>
            </Alert>
          )}

          <OutcomeList outcomes={outcomes} />

          <div>
            <Button onClick={() => void handleSave()} disabled={saving}>
              {saving ? "Saving…" : "Save"}
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
