import * as React from "react";
import { Alert, AlertDescription, Tabs, TabsList, TabsTrigger, TabsContent } from "@ap/design";
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
  type TokenWrite,
} from "../lib/api";
import { ValidationPanel } from "../lib/ValidationPanel";
import { DriftPanel } from "../lib/DriftPanel";
import { ActionBar } from "../lib/ActionBar";
import type { SettingsSpec } from "./spec";
import { CatalogSection } from "./CatalogSection";
import { LimitsSection } from "./LimitsSection";
import { DefaultsSection } from "./DefaultsSection";

// SUB_TABS is the Cluster tab's vertical sub-navigation: Catalog, Limits,
// and Defaults each used to render as one long stacked section, burying
// Save at the bottom of a very long scroll. They're the SAME three section
// components as before (CatalogSection/LimitsSection/DefaultsSection,
// unmodified) — only the layout around them changed.
const SUB_TABS = [
  { id: "catalog", label: "Catalog" },
  { id: "limits", label: "Limits" },
  { id: "defaults", label: "Defaults" },
] as const;

export interface ClusterTabProps {
  apiBase: string;
  running: boolean;
}

// ClusterTab is the settings app's Cluster tab: the model catalog, ceiling
// limits, and default fallbacks curated forms, backed by ONE spec state
// object that every section reads and writes through (spec, onChange) — see
// spec.ts's module doc for the wire shape and its round-trip guarantees.
export function ClusterTab({ apiBase, running }: ClusterTabProps) {
  const [resp, setResp] = React.useState<ClusterSettingsResponse | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [spec, setSpec] = React.useState<SettingsSpec>({});
  // edited is the dirty flag: an explicit "the user interacted with a form
  // control" boolean, set by the onChange/onTokensChange wrappers below and
  // cleared on load and on every successful save (ordinary or
  // take-ownership). Deliberately NOT a serialization comparison against a
  // baseline: setPath's delete→re-set and replaceCatalogEntry's
  // remove-then-upsert both reorder JSON keys, so comparing JSON strings
  // false-positives on a clear-and-retype revert (dirty stuck true with no
  // way to clear it short of saving) and false-NEGATIVES on an Edit→Save of
  // an unchanged catalog entry whose rebuild is byte-identical.
  const [edited, setEdited] = React.useState(false);
  const [tokens, setTokens] = React.useState<TokenWrite[]>([]);

  const [validation, setValidation] = React.useState<ValidationResult | null>(null);
  const [validating, setValidating] = React.useState(false);
  const [validateError, setValidateError] = React.useState<string | null>(null);

  const [saveResult, setSaveResult] = React.useState<ClusterSettingsUpdateResponse | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [tookOwnership, setTookOwnership] = React.useState(false);

  React.useEffect(() => {
    let closed = false;
    getClusterSettings(apiBase)
      .then((r) => {
        if (closed) return;
        setResp(r);
        setSpec((r.spec as SettingsSpec | undefined) ?? {});
        setEdited(false);
      })
      .catch((e: Error) => {
        if (!closed) setLoadError(e.message);
      });
    return () => {
      closed = true;
    };
  }, [apiBase]);

  async function handleValidate() {
    setValidateError(null);
    setValidating(true);
    try {
      const result = await validateClusterSettings(apiBase, { spec });
      setValidation(result);
    } catch (e) {
      const structured = tryParseValidationResult(e);
      if (structured) setValidation(structured);
      else setValidateError(e instanceof Error ? e.message : String(e));
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
        ? { spec, takeOwnership: true }
        : { spec, tokens };
      const result = await putClusterSettings(apiBase, req);
      setSaveResult(result);
      if (result.applied) {
        setEdited(false);
        if (!takeOwnership) setTokens([]);
      }
    } catch (e) {
      const structured = tryParseUpdateResponse(e);
      if (structured) setSaveResult(structured);
      else setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  }

  const handleSave = () => runPut(false);
  const handleTakeOwnership = () => runPut(true);

  // handleSpecChange/handleTokensChange are the ONLY paths a section has to
  // mutate the spec or the staged tokens, which is what makes the edited
  // flag trustworthy: every user edit funnels through one of them.
  function handleSpecChange(next: SettingsSpec) {
    setSpec(next);
    setEdited(true);
  }
  function handleTokensChange(next: TokenWrite[]) {
    setTokens(next);
    setEdited(true);
  }

  // Pending token writes keep dirty true on their own: a take-ownership
  // save re-applies the spec WITHOUT `tokens` and clears `edited`, so a
  // token staged during the drift window would otherwise read as clean
  // while its Secret write is still unsent. Only the ordinary save that
  // carries the tokens clears both terms.
  const dirty = edited || tokens.length > 0;

  return (
    <div className="flex h-full flex-col gap-6">
      <h2 className="text-base font-semibold">Cluster settings</h2>

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
              The desktop isn&apos;t running — Validate still works, but Save will fail until it&apos;s back up.
            </p>
          )}

          <Tabs defaultValue="catalog" orientation="vertical" className="flex min-h-0 flex-1 gap-6">
            <TabsList className="h-auto w-40 shrink-0 flex-col items-stretch justify-start gap-1 bg-transparent p-0">
              {SUB_TABS.map((t) => (
                <TabsTrigger
                  key={t.id}
                  value={t.id}
                  className="justify-start data-[state=active]:bg-muted"
                >
                  {t.label}
                </TabsTrigger>
              ))}
            </TabsList>

            {/* The panel column is the ONLY scroll region on this tab: the
                sub-tab list, the panels row and the ActionBar all sit outside
                it, so a long section scrolls while Validate/Save stay put. */}
            <div className="min-w-0 flex-1 overflow-y-auto">
              <TabsContent value="catalog" className="mt-0">
                <CatalogSection
                  spec={spec}
                  onChange={handleSpecChange}
                  tokens={tokens}
                  onTokensChange={handleTokensChange}
                />
              </TabsContent>
              <TabsContent value="limits" className="mt-0">
                <LimitsSection spec={spec} onChange={handleSpecChange} />
              </TabsContent>
              <TabsContent value="defaults" className="mt-0">
                <DefaultsSection spec={spec} onChange={handleSpecChange} />
              </TabsContent>
            </div>
          </Tabs>

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
            onSave={() => void handleSave()}
          />
        </>
      )}
    </div>
  );
}
