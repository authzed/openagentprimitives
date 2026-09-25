import * as React from "react";
import { Alert, AlertDescription } from "@ap/design";
import {
  getClusterSettings,
  validateClusterSettings,
  putClusterSettings,
  tryParseValidationResult,
  tryParseUpdateResponse,
  type ClusterSettingsResponse,
  type ClusterSettingsUpdateRequest,
  type ClusterSettingsUpdateResponse,
  type ValidationResult,
} from "../lib/api";
import { ValidationPanel } from "../lib/ValidationPanel";
import { DriftPanel } from "../lib/DriftPanel";
import { ActionBar } from "../lib/ActionBar";

export interface AdvancedTabProps {
  apiBase: string;
  running: boolean;
}

// APPLY_CONFIRM_WINDOW_MS is how long an armed "Apply without validating?"
// stays armed before it silently disarms. Long enough to read the relabeled
// button and click again on purpose, short enough that a click days later
// (after the tab sat open) can't land as an unvalidated apply the user no
// longer means.
const APPLY_CONFIRM_WINDOW_MS = 10_000;

// AdvancedTab is the settings app's raw-YAML escape hatch onto the SAME
// ClusterAgentSettings singleton ClusterTab's curated forms edit — the two
// are different views of one document, never a separate one, which is why
// they share the wire shape (`{yaml}` instead of `{spec}`) and every
// downstream behavior: validation, drift, and take-ownership are identical,
// just with a raw-YAML body. Validate/DriftPanel are the ones extracted to
// `../lib` for exactly this reuse.
export function AdvancedTab({ apiBase, running }: AdvancedTabProps) {
  const [resp, setResp] = React.useState<ClusterSettingsResponse | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  // yamlText is seeded ONCE from the GET's `yaml` field when this tab first
  // loads (Radix unmounts inactive TabsContent, so "first load" is really
  // "each time this tab is switched to fresh") — it is not re-fetched on
  // every activation, so edits in progress survive switching to another tab
  // and back as long as this component instance doesn't unmount.
  const [yamlText, setYamlText] = React.useState("");
  // baselineYaml is the last-loaded-or-applied text: dirty is derived from
  // comparing yamlText against it, set once on load and reset on every
  // successful apply (ordinary or take-ownership) — mirrors ClusterTab's
  // baselineSpec for the same reason (see its doc comment).
  const [baselineYaml, setBaselineYaml] = React.useState("");

  const [validation, setValidation] = React.useState<ValidationResult | null>(null);
  const [validating, setValidating] = React.useState(false);
  const [validateError, setValidateError] = React.useState<string | null>(null);
  // validatedYaml is the text that was last successfully (or definitively —
  // a 422 IS a definitive answer) validated. Apply compares it against the
  // CURRENT yamlText: equal means the on-screen text is exactly what
  // Validate looked at, so Apply proceeds straight away; any difference
  // (never validated, or edited since) requires the double-confirm below.
  const [validatedYaml, setValidatedYaml] = React.useState<string | null>(null);

  const [saveResult, setSaveResult] = React.useState<ClusterSettingsUpdateResponse | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [tookOwnership, setTookOwnership] = React.useState(false);

  // armed is the first-click state of the unvalidated-apply confirm: true
  // while a second click within APPLY_CONFIRM_WINDOW_MS will actually apply.
  const [armed, setArmed] = React.useState(false);
  const armTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  React.useEffect(() => {
    return () => {
      if (armTimer.current) clearTimeout(armTimer.current);
    };
  }, []);

  React.useEffect(() => {
    let closed = false;
    getClusterSettings(apiBase)
      .then((r) => {
        if (closed) return;
        setResp(r);
        const loaded = r.yaml ?? "";
        setYamlText(loaded);
        setBaselineYaml(loaded);
      })
      .catch((e: Error) => {
        if (!closed) setLoadError(e.message);
      });
    return () => {
      closed = true;
    };
  }, [apiBase]);

  function disarm() {
    if (armTimer.current) {
      clearTimeout(armTimer.current);
      armTimer.current = null;
    }
    setArmed(false);
  }

  function handleYamlChange(next: string) {
    setYamlText(next);
    // Editing after arming means the second click would apply text Validate
    // never confirmed, not the text the user armed against — disarm rather
    // than let a stale confirm cover different content.
    if (armed) disarm();
  }

  async function handleValidate() {
    setValidateError(null);
    setValidating(true);
    const text = yamlText;
    try {
      const result = await validateClusterSettings(apiBase, { yaml: text });
      setValidation(result);
      setValidatedYaml(text);
    } catch (e) {
      const structured = tryParseValidationResult(e);
      if (structured) {
        setValidation(structured);
        setValidatedYaml(text);
      } else {
        setValidateError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setValidating(false);
    }
  }

  async function runPut(takeOwnership: boolean) {
    setSaveError(null);
    setSaving(true);
    setTookOwnership(takeOwnership);
    try {
      const req: ClusterSettingsUpdateRequest = takeOwnership
        ? { yaml: yamlText, takeOwnership: true }
        : { yaml: yamlText };
      const result = await putClusterSettings(apiBase, req);
      setSaveResult(result);
      if (result.applied) setBaselineYaml(yamlText);
    } catch (e) {
      const structured = tryParseUpdateResponse(e);
      if (structured) setSaveResult(structured);
      else setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  const handleTakeOwnership = () => runPut(true);

  // dirty compares against baselineYaml (not "has yamlText ever changed"), so
  // it clears the instant an apply lands — mirrors ClusterTab's `dirty`.
  const dirty = yamlText !== baselineYaml;

  function handleApplyClick() {
    if (validatedYaml === yamlText) {
      void runPut(false);
      return;
    }
    if (armed) {
      disarm();
      void runPut(false);
      return;
    }
    setArmed(true);
    armTimer.current = setTimeout(() => setArmed(false), APPLY_CONFIRM_WINDOW_MS);
  }

  const applyLabel = saving ? "Applying…" : armed ? "Apply without validating?" : "Apply";

  return (
    <div className="flex h-full flex-col gap-6">
      <h2 className="text-base font-semibold">Advanced (raw YAML)</h2>

      {loadError && (
        <Alert variant="destructive">
          <AlertDescription>{loadError}</AlertDescription>
        </Alert>
      )}

      {resp?.clusterDown && (
        <Alert variant="destructive">
          <AlertDescription>
            The desktop cluster isn&apos;t running. Start it from the General tab to edit cluster settings.
          </AlertDescription>
        </Alert>
      )}

      {resp && !resp.clusterDown && (
        <>
          {!running && (
            <p className="text-xs text-muted-foreground">
              The desktop isn&apos;t running — Validate still works, but Apply will fail until it&apos;s back up.
            </p>
          )}

          {/* The textarea is this tab's grow-and-scroll region (textareas
              scroll natively): it takes whatever height the bounded shell
              leaves, so the panels row and ActionBar below never leave the
              screen. */}
          <textarea
            aria-label="cluster settings yaml"
            spellCheck={false}
            className="min-h-0 w-full flex-1 rounded-md border border-border bg-background p-3 font-mono text-xs"
            value={yamlText}
            onChange={(e) => handleYamlChange(e.target.value)}
          />

          {armed && (
            <p className="text-xs text-muted-foreground">
              This text hasn&apos;t been validated. Click Apply again within 10 seconds to apply it anyway.
            </p>
          )}

          {(validateError || validation || saveError || saveResult) && (
            // Height-capped so a long violations list can't push the
            // ActionBar off-screen; the list scrolls within the cap instead.
            <div className="flex max-h-48 shrink-0 flex-col gap-2 overflow-y-auto">
              {validateError && (
                <Alert variant="destructive">
                  <AlertDescription>{validateError}</AlertDescription>
                </Alert>
              )}
              {validation && <ValidationPanel result={validation} />}

              {saveError && (
                <Alert variant="destructive">
                  <AlertDescription>{saveError}</AlertDescription>
                </Alert>
              )}
              {saveResult && (
                <div className="flex flex-col gap-2">
                  <p className="text-sm">{saveResult.applied ? "Applied." : "Not applied — see validation below."}</p>
                  <ValidationPanel result={saveResult.validation} />
                  {saveResult.drift && saveResult.drift.length > 0 && (
                    <DriftPanel
                      drift={saveResult.drift}
                      onTakeOwnership={() => void handleTakeOwnership()}
                      busy={saving}
                      afterTakeOwnership={tookOwnership}
                    />
                  )}
                </div>
              )}
            </div>
          )}

          <ActionBar
            dirty={dirty}
            busy={validating || saving}
            onValidate={() => void handleValidate()}
            onSave={handleApplyClick}
            saveLabel={applyLabel}
          />
        </>
      )}
    </div>
  );
}
