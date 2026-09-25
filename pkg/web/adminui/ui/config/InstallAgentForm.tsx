import * as React from "react";
import { Alert, AlertDescription, Button } from "@ap/design";
import {
  ApiError, OapChannelQuestionsError, OapInstallConflictsError, OapInstallQuestionsError,
  oapChannelHandoff, oapChannelSetup, oapInstallByFile, oapInstallByRef,
  type OapInstallChannel, type OapInstallConflict, type OapInstallResult, type OapQuestion, type SkillClone,
} from "../lib/api";
import { httpUrl } from "../lib/safeUrl";
import { Field } from "./detail/shared";

// INPUT_CLASS matches the text-input styling already used across the console
// (ResourceTable's filter box, AuditExplorer's search field, MemoryView's
// search field) — kept as one constant here since this form introduces
// several new inputs and they must all read as the same control.
const INPUT_CLASS =
  "w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50";

type Source = "file" | "ref";
type ChannelPhase = "pending" | "handoff-started" | "staged";

function dependencyLabel(agentPath?: string): string {
  return agentPath ? `Dependency ${agentPath}` : "Root agent";
}

// questionKey is the browser-side half of the graph answer contract. Backend
// paths are display-oriented ("reviewer > helper"); external answer keys use
// the manifest's nested agents.<name> namespace at every dependency edge.
function questionKey(q: OapQuestion): string {
  const path = q.agentPath?.split(" > ").map((part) => part.trim()).filter(Boolean) ?? [];
  if (path.length === 0) return q.name;
  return `${path.map((part) => `agents.${part}`).join(".")}.${q.name}`;
}

function channelKey(channel: OapInstallChannel): string {
  // The token seals the graph/source/target identity. A later planning pass
  // may return the same logical channel with a different token; it must not
  // inherit staged state or credential answers from the earlier identity.
  return `${channel.agentPath ?? ""}\u0000${channel.name}\u0000${channel.setupToken ?? ""}`;
}

function retainKeys<T>(record: Record<string, T>, keys: Set<string>): Record<string, T> {
  const next: Record<string, T> = {};
  for (const key of keys) {
    if (record[key] !== undefined) next[key] = record[key];
  }
  return next;
}

// defaultAsString renders an OapQuestion.default (an untyped JSON value —
// string for most question types, but capacityfit's own quantity defaults are
// always strings; array-valued for a resourceList multi-select) into the
// plain string a text/number/checkbox input needs. Absent (undefined/null)
// renders as "" — the caller then falls back to whatever the operator has
// already typed.
function defaultAsString(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (Array.isArray(v)) return v.join(",");
  return String(v);
}

// enumOptionLabel is what the operator READS for the enum entry at i, mirroring
// oap.Question.EnumLabelFor (pkg/platform/oap/question.go) exactly.
//
// Exactly means: the label is TRIMMED ONLY TO DECIDE whether it counts, and
// returned verbatim when it does. A whitespace-only label is no label and falls
// back to the value; a padded one is the author's, spaces included. Trimming
// the returned string instead would be a second, quieter rule — immaterial in
// HTML, which collapses runs of whitespace, and wrong the moment anything else
// reads this. One renderer disagreeing with another about what a question SAYS
// is the whole class of defect wizardkeys and EnumLabelFor exist to close.
function enumOptionLabel(q: OapQuestion, i: number): string {
  const label = q.enumLabels?.[i];
  if (label !== undefined && label.trim() !== "") return label;
  return q.enum?.[i] ?? "";
}

// QuestionField renders one auto-generated install-question input, dispatching
// on OapQuestion.type: string/resourceList → text, int → number, bool →
// checkbox, enum → select, secret → password (masked — the one type where the
// entered value must never be echoed back or logged). required defaults to
// true when unset, mirroring oap.Question.IsRequired().
function QuestionField({
  q, value, onChange, disabled = false,
}: {
  q: OapQuestion;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const required = q.required !== false;
  const label = `${q.prompt || q.name}${required ? " *" : ""}`;

  return (
    <Field label={label}>
      {q.type === "bool" ? (
        <input
          type="checkbox"
          checked={value === "true"}
          onChange={(e) => { if (!disabled) onChange(e.target.checked ? "true" : "false"); }}
          disabled={disabled}
          aria-label={label}
          className="h-4 w-4 rounded border-border"
        />
      ) : q.type === "enum" ? (
        <select value={value} onChange={(e) => { if (!disabled) onChange(e.target.value); }} disabled={disabled} aria-label={label} className={INPUT_CLASS}>
          <option value="" disabled>
            Select…
          </option>
          {/*
            The VALUE submitted is always the enum entry — a stable answer key
            `--answer <name>=<value>` already depends on. Only the text shown
            varies; see enumOptionLabel for the pairing rule. Dropping the
            labels here would put the operator in front of rows reading "false"
            and "provision" and ask them to pick.
          */}
          {(q.enum ?? []).map((opt, i) => (
            <option key={opt} value={opt}>
              {enumOptionLabel(q, i)}
            </option>
          ))}
        </select>
      ) : (
        <input
          type={q.type === "secret" ? "password" : q.type === "int" ? "number" : "text"}
          value={value}
          onChange={(e) => { if (!disabled) onChange(e.target.value); }}
          disabled={disabled}
          aria-label={label}
          placeholder={q.type === "resourceList" ? "comma-separated" : undefined}
          autoComplete={q.type === "secret" ? "new-password" : "off"}
          className={INPUT_CLASS}
        />
      )}
      {q.description && <p className="text-[11px] text-muted-foreground">{q.description}</p>}
    </Field>
  );
}

export interface InstallAgentFormProps {
  apiBase: string;
  // onInstalled fires on a 200; namespace is passed alongside the result
  // (the response itself only carries the resolved name) so the caller can
  // deep-link to the new agent's detail page.
  onInstalled: (result: OapInstallResult, namespace: string) => void;
}

// InstallAgentForm is the `install_agent` entry point: pick an uploaded .oap
// file OR a registry ref, target namespace/name, submit. The backend answers
// non-interactively — a required install question with no default comes back
// as a 400 carrying the question's full typed schema (OapInstallQuestionsError)
// rather than ever prompting, so a first submit with no answers commonly comes
// back asking for exactly the fields it needs; this component renders those,
// merges the answers into `values`, and re-submits the same source/target.
//
// Split out from the Dialog wrapper (InstallAgentDialog) so it can be tested
// as a plain component — no Radix portal/overlay mechanics to drive in tests.
export function InstallAgentForm({ apiBase, onInstalled }: InstallAgentFormProps) {
  const [source, setSource] = React.useState<Source>("file");
  const [file, setFile] = React.useState<File | null>(null);
  const [ref, setRef] = React.useState("");
  const [namespace, setNamespace] = React.useState("default");
  const [name, setName] = React.useState("");
  const [values, setValues] = React.useState<Record<string, string>>({});
  const [questions, setQuestions] = React.useState<OapQuestion[] | null>(null);
  const [skillClones, setSkillClones] = React.useState<SkillClone[]>([]);
  // notices carries the capacity-check hook's own explanations (e.g. "lowering
  // memory from 4Gi to 768Mi to fit largest node…") alongside the questions
  // asking for confirmation — see OapInstallQuestionsError.warnings.
  const [notices, setNotices] = React.useState<string[]>([]);
  // conflicts is the 409's list; adoptKeys is what the operator has ticked.
  // Secrets are never pre-ticked — adopting one overwrites its data, so it
  // takes a deliberate click.
  const [conflicts, setConflicts] = React.useState<OapInstallConflict[]>([]);
  const [adoptKeys, setAdoptKeys] = React.useState<string[]>([]);
  const [channels, setChannels] = React.useState<OapInstallChannel[]>([]);
  const [channelAnswers, setChannelAnswers] = React.useState<Record<string, Record<string, string>>>({});
  const [channelPhases, setChannelPhases] = React.useState<Record<string, ChannelPhase>>({});
  const [channelGuidance, setChannelGuidance] = React.useState<Record<string, { nextSteps: string[]; warnings: string[] }>>({});
  const [channelBusy, setChannelBusy] = React.useState<string | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [forbidden, setForbidden] = React.useState(false);
  const [submitting, setSubmitting] = React.useState(false);

  const canSubmit = source === "file" ? file !== null : ref.trim() !== "";

  // resetAdoption clears both the rendered conflict list and the operator's
  // ticks. Called whenever ANYTHING that can change which cluster object a
  // bundled resource resolves to changes — the install TARGET (file, ref,
  // namespace, name, or source) or an install-QUESTION ANSWER — so a stale
  // tick from a previous derivation's conflict list can never ride a submit
  // against a new one. See the file's own read of OapInstallConflict.Key()
  // (Kind+"/"+Name, no namespace): a changed namespace can collide on the
  // exact same key; an install-name override renames every bundled object
  // (install.Install → instance.Rename) before conflict detection runs; and a
  // bundle Binding can target metadata.name just as validly as any other path
  // (oap.ParseTarget has no denylist on segment names, and oap.Apply runs
  // before conflict detection too), so an answer edit can retarget the same
  // way a name-field edit does. The form cannot know which bindings a given
  // bundle declares, so this resets unconditionally on every such edit rather
  // than trying to detect which ones actually matter — a tick is only
  // meaningful against the specific conflict list it was rendered for, and
  // re-submitting re-derives that list from scratch. Anything short of a full
  // clear here risks silently authorizing a seizure of an object the operator
  // never saw a conflict row for, or — if that key happens to be a Secret in
  // the new derivation — a checkbox rendering pre-ticked in violation of the
  // never-pre-tick-a-Secret rule below.
  const resetAdoption = () => {
    setConflicts([]);
    setAdoptKeys([]);
  };

  const resetChannelDecisions = () => {
    setChannels([]);
    setChannelAnswers({});
    setChannelPhases({});
    setChannelGuidance({});
    setChannelBusy(null);
  };

  const resetDerivedDecisions = () => {
    resetAdoption();
    resetChannelDecisions();
  };

  const setAnswer = (qname: string, v: string) => {
    setValues((prev) => ({ ...prev, [qname]: v }));
    // An answer can retarget resources, so adoption consent never survives an
    // edit. A staged channel, however, is sealed server-side and authorized by
    // its opaque setup token: retain only that token-scoped state while the
    // server replans. Pending/handoff input (including secrets) is discarded.
    // receiveDecisions below then keeps staged state only when the exact token
    // comes back.
    resetAdoption();
    const stagedKeys = new Set(Object.entries(channelPhases)
      .filter(([, phase]) => phase === "staged")
      .map(([key]) => key));
    setChannels((prev) => prev.filter((channel) => stagedKeys.has(channelKey(channel))));
    setChannelAnswers((prev) => retainKeys(prev, stagedKeys));
    setChannelPhases((prev) => retainKeys(prev, stagedKeys));
    setChannelGuidance((prev) => retainKeys(prev, stagedKeys));
    setChannelBusy(null);
  };

  const receiveDecisions = (
    nextQuestions: OapQuestion[],
    nextConflicts: OapInstallConflict[],
    nextChannels: OapInstallChannel[],
    nextClones: SkillClone[],
    nextWarnings: string[],
    preserveQuestionsWhenEmpty = false,
  ) => {
    if (nextQuestions.length > 0) setQuestions(nextQuestions);
    else if (!preserveQuestionsWhenEmpty) setQuestions(null);
    setConflicts(nextConflicts);
    setChannels(nextChannels);
    setSkillClones(nextClones);
    setNotices(nextWarnings);
    setValues((prev) => {
      const next = { ...prev };
      for (const q of nextQuestions) {
        const key = questionKey(q);
        if (next[key] === undefined && q.default !== undefined && q.default !== null) {
          next[key] = defaultAsString(q.default);
        }
      }
      return next;
    });
    setChannelAnswers((prev) => {
      const next: Record<string, Record<string, string>> = {};
      for (const channel of nextChannels) {
        const key = channelKey(channel);
        const answers = { ...(prev[key] ?? {}) };
        for (const q of channel.questions ?? []) {
          if (answers[q.name] === undefined && q.default !== undefined && q.default !== null) {
            answers[q.name] = defaultAsString(q.default);
          }
        }
        next[key] = answers;
      }
      return next;
    });
    setChannelPhases((prev) => {
      const next: Record<string, ChannelPhase> = {};
      for (const channel of nextChannels) {
        const key = channelKey(channel);
        next[key] = prev[key] ?? "pending";
      }
      return next;
    });
    setChannelGuidance((prev) => {
      const next: Record<string, { nextSteps: string[]; warnings: string[] }> = {};
      for (const channel of nextChannels) {
        const key = channelKey(channel);
        if (prev[key]) next[key] = prev[key];
      }
      return next;
    });
  };

  const setChannelAnswer = (channel: OapInstallChannel, qname: string, value: string) => {
    const key = channelKey(channel);
    setChannelAnswers((prev) => ({ ...prev, [key]: { ...(prev[key] ?? {}), [qname]: value } }));
  };

  const updateChannelQuestions = (channel: OapInstallChannel, nextQuestions: OapQuestion[]) => {
    const key = channelKey(channel);
    setChannels((prev) => prev.map((candidate) => channelKey(candidate) === key
      ? { ...candidate, questions: nextQuestions }
      : candidate));
  };

  const startChannelSetup = async (channel: OapInstallChannel) => {
    const key = channelKey(channel);
    if (!channel.setupToken) {
      setError(`Channel ${channel.name} has no setup token.`);
      return;
    }
    setError(null);
    setChannelBusy(key);
    try {
      const result = await oapChannelSetup(apiBase, channel.setupToken, channelAnswers[key] ?? {});
      setChannelGuidance((prev) => ({ ...prev, [key]: {
        nextSteps: result.nextSteps ?? [], warnings: result.warnings ?? [],
      } }));
      setChannelPhases((prev) => ({ ...prev, [key]: "staged" }));
    } catch (e) {
      if (e instanceof OapChannelQuestionsError) {
        updateChannelQuestions(channel, e.questions);
      } else if (e instanceof ApiError && e.status === 403) {
        setForbidden(true);
      } else {
        setError((e as Error).message);
      }
    } finally {
      setChannelBusy(null);
    }
  };

  const openHandoff = (handoff: { url: string; formFields?: Record<string, string> }): boolean => {
    const safe = httpUrl(handoff.url);
    if (!safe) {
      setError("Channel setup returned an unsafe handoff URL.");
      return false;
    }
    if (!handoff.formFields || Object.keys(handoff.formFields).length === 0) {
      window.open(safe, "_blank", "noopener,noreferrer");
      return true;
    }
    const form = document.createElement("form");
    form.method = "POST";
    form.action = safe;
    form.target = "_blank";
    form.rel = "noopener noreferrer";
    for (const [name, value] of Object.entries(handoff.formFields)) {
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = name;
      input.value = value;
      form.appendChild(input);
    }
    document.body.appendChild(form);
    try {
      form.submit();
    } finally {
      form.remove();
    }
    return true;
  };

  const startChannelHandoff = async (channel: OapInstallChannel) => {
    const key = channelKey(channel);
    if (!channel.setupToken) {
      setError(`Channel ${channel.name} has no setup token.`);
      return;
    }
    setError(null);
    setChannelBusy(key);
    try {
      const handoff = await oapChannelHandoff(apiBase, channel.setupToken, channelAnswers[key] ?? {});
      if (handoff.nextAction === "setup") {
        const result = await oapChannelSetup(apiBase, channel.setupToken, channelAnswers[key] ?? {});
        setChannelGuidance((prev) => ({ ...prev, [key]: {
          nextSteps: result.nextSteps ?? [], warnings: result.warnings ?? [],
        } }));
        setChannelPhases((prev) => ({ ...prev, [key]: "staged" }));
        return;
      }
      if (!handoff.url) {
        setError("Channel setup returned no handoff URL or next action.");
        return;
      }
      if (openHandoff({ url: handoff.url, formFields: handoff.formFields })) {
        setChannelPhases((prev) => ({ ...prev, [key]: "handoff-started" }));
      }
    } catch (e) {
      if (e instanceof OapChannelQuestionsError) {
        updateChannelQuestions(channel, e.questions);
      } else if (e instanceof ApiError && e.status === 403) {
        setForbidden(true);
      } else {
        setError((e as Error).message);
      }
    } finally {
      setChannelBusy(null);
    }
  };

  const submit = async () => {
    setError(null);
    setForbidden(false);
    setSkillClones([]);
    setNotices([]);
    // Reset for the NEXT render, not this call: `adoptKeys` below still reads
    // the value already captured in this closure (the ticks the operator just
    // made), so the in-flight POST is unaffected — only the checkboxes and any
    // later submit start clean.
    setConflicts([]);
    setAdoptKeys([]);
    setSubmitting(true);
    try {
      const result =
        source === "file"
          ? await oapInstallByFile(apiBase, file as File, { namespace, name: name || undefined, values, adopt: adoptKeys })
          : await oapInstallByRef(apiBase, { ref: ref.trim(), namespace, name: name || undefined, values, adopt: adoptKeys });
      const setupMessages = channels.flatMap((channel) => {
        const guidance = channelGuidance[channelKey(channel)];
        if (!guidance) return [];
        return [...guidance.nextSteps, ...guidance.warnings].map((message) => `${channel.name}: ${message}`);
      });
      const completed = setupMessages.length === 0
        ? result
        : { ...result, warnings: [...(result.warnings ?? []), ...setupMessages] };
      setQuestions(null);
      setChannels([]);
      onInstalled(completed, namespace);
    } catch (e) {
      if (e instanceof OapInstallQuestionsError) {
        receiveDecisions(e.questions, e.conflicts, e.channels, e.skillClones, e.warnings);
      } else if (e instanceof OapInstallConflictsError) {
        // A conflict-only replan does not repeat the question schema on legacy
        // responses. Keep the already-rendered fields so an answer can still
        // be edited; aggregate responses replace them when they do carry it.
        receiveDecisions(e.questions, e.conflicts, e.channels, e.skillClones, e.warnings, true);
      } else if (e instanceof ApiError && e.status === 403) {
        setForbidden(true);
      } else {
        setError((e as Error).message);
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex flex-col gap-4">
      {forbidden && (
        <Alert variant="destructive">
          <AlertDescription>Not authorized to install agents.</AlertDescription>
        </Alert>
      )}
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <div className="flex gap-2">
        <Button
          type="button"
          variant={source === "file" ? "default" : "outline"}
          size="sm"
          onClick={() => {
            setSource("file");
            resetDerivedDecisions();
          }}
        >
          Upload a .oap file
        </Button>
        <Button
          type="button"
          variant={source === "ref" ? "default" : "outline"}
          size="sm"
          onClick={() => {
            setSource("ref");
            resetDerivedDecisions();
          }}
        >
          Registry ref
        </Button>
      </div>

      {source === "file" ? (
        <Field label="Bundle file">
          <input
            key="file-input"
            type="file"
            accept=".oap"
            aria-label="Bundle file"
            onChange={(e) => {
              setFile(e.target.files?.[0] ?? null);
              resetDerivedDecisions();
            }}
            className="text-sm text-foreground"
          />
        </Field>
      ) : (
        <Field label="Registry ref">
          <input
            key="ref-input"
            type="text"
            value={ref}
            onChange={(e) => {
              setRef(e.target.value);
              resetDerivedDecisions();
            }}
            placeholder="ghcr.io/acme/support-bot:1.0.0"
            aria-label="Registry ref"
            className={INPUT_CLASS}
          />
        </Field>
      )}

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Field label="Namespace *">
          <input
            type="text"
            value={namespace}
            onChange={(e) => {
              setNamespace(e.target.value);
              resetDerivedDecisions();
            }}
            aria-label="Namespace"
            className={INPUT_CLASS}
          />
        </Field>
        <Field label="Name (optional)">
          <input
            type="text"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              resetDerivedDecisions();
            }}
            placeholder="defaults to the bundled agent's name"
            aria-label="Name"
            className={INPUT_CLASS}
          />
        </Field>
      </div>

      {skillClones.length > 0 && (
        <div className="flex flex-col gap-1 rounded-md border border-dashed border-border p-3">
          <p className="text-xs font-medium text-foreground">
            Installing this agent will clone {skillClones.length === 1 ? "an external repository" : "external repositories"}:
          </p>
          <ul className="list-disc pl-5 text-xs text-muted-foreground">
            {skillClones.map((c) => (
              <li key={c.skillSource}>
                <span className="text-foreground">{c.repoURL}</span> @ {c.ref || "default branch"} (SkillSource {c.skillSource})
              </li>
            ))}
          </ul>
        </div>
      )}

      {notices.length > 0 && (
        <Alert>
          <AlertDescription>
            <ul className="list-disc pl-5 text-xs">
              {notices.map((n) => (
                <li key={n}>{n}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      )}

      {conflicts.length > 0 && (
        <div className="flex flex-col gap-2 rounded-md border border-destructive/50 p-3">
          <p className="text-xs font-medium text-foreground">
            These objects already exist and were not created by this install. Adopting one overwrites its
            spec with this bundle&apos;s, and uninstalling the agent will delete it.
          </p>
          {conflicts.map((c) => {
            const key = `${c.kind}/${c.name}`;
            const label = c.namespace ? `${c.kind} ${c.namespace}/${c.name}` : key;
            return (
              <label key={key} className="flex items-center gap-2 text-xs text-foreground">
                <input
                  type="checkbox"
                  checked={adoptKeys.includes(key)}
                  aria-label={label}
                  onChange={(e) =>
                    setAdoptKeys((prev) => (e.target.checked ? [...prev, key] : prev.filter((k) => k !== key)))
                  }
                  className="h-4 w-4 rounded border-border"
                />
                <span>{label}</span>
                {c.secret && <span className="text-destructive">overwrites this Secret&apos;s data</span>}
              </label>
            );
          })}
        </div>
      )}

      {questions && questions.length > 0 && (
        <div className="flex flex-col gap-3 rounded-md border border-dashed border-border p-3">
          <p className="text-xs text-muted-foreground">
            This bundle needs {questions.length === 1 ? "an answer" : "answers"} before it can install:
          </p>
          {questions.map((q) => (
            <div key={questionKey(q)} className="flex flex-col gap-1">
              <p className="text-[11px] font-medium text-muted-foreground">{dependencyLabel(q.agentPath)}</p>
              <QuestionField
                q={q}
                value={values[questionKey(q)] ?? defaultAsString(q.default)}
                onChange={(v) => setAnswer(questionKey(q), v)}
              />
            </div>
          ))}
        </div>
      )}

      {channels.length > 0 && (
        <div className="flex flex-col gap-3 rounded-md border border-dashed border-border p-3">
          <p className="text-xs text-muted-foreground">Complete the required channel setup before resuming the install:</p>
          {channels.map((channel) => {
            const key = channelKey(channel);
            const phase = channelPhases[key] ?? "pending";
            const answers = channelAnswers[key] ?? {};
            return (
              <div key={key} className="flex flex-col gap-2 rounded-md border border-border p-3">
                <p className="text-[11px] font-medium text-muted-foreground">{dependencyLabel(channel.agentPath)}</p>
                <p className="text-sm font-medium text-foreground">{channel.name}</p>
                <p className="text-xs text-muted-foreground">{channel.kind} · {channel.role}</p>
                {channel.purpose && <p className="text-xs text-foreground">{channel.purpose}</p>}
                {channel.reason && <p className="text-xs text-destructive">{channel.reason}</p>}
                {channel.remedy && <p className="text-xs text-muted-foreground">{channel.remedy}</p>}
                {(channel.questions ?? []).map((q) => (
                  <QuestionField
                    key={q.name}
                    q={q}
                    value={answers[q.name] ?? defaultAsString(q.default)}
                    onChange={(value) => setChannelAnswer(channel, q.name, value)}
                    disabled={phase === "staged"}
                  />
                ))}
                {phase === "staged" ? (
                  <div className="flex flex-col gap-1 text-xs">
                    <p className="text-success">Setup staged</p>
                    {(channelGuidance[key]?.nextSteps ?? []).map((step) => <p key={`next-${step}`} className="text-foreground">{step}</p>)}
                    {(channelGuidance[key]?.warnings ?? []).map((warning) => <p key={`warning-${warning}`} className="text-warning">{warning}</p>)}
                  </div>
                ) : channel.status === "handoff" && phase === "pending" ? (
                  <Button type="button" size="sm" className="self-start" disabled={channelBusy === key}
                    onClick={() => void startChannelHandoff(channel)}>
                    Start browser setup for {channel.name}
                  </Button>
                ) : channel.status === "ask" || phase === "handoff-started" ? (
                  <Button type="button" size="sm" className="self-start" disabled={channelBusy === key}
                    onClick={() => void startChannelSetup(channel)}>
                    {phase === "handoff-started" ? `Resume ${channel.name}` : `Set up ${channel.name}`}
                  </Button>
                ) : (
                  <p className="text-xs text-destructive">This channel cannot be set up from the browser.</p>
                )}
              </div>
            );
          })}
        </div>
      )}

      <Button type="button" onClick={submit} disabled={!canSubmit || submitting} className="self-start">
        {submitting
          ? "Installing…"
          : conflicts.length > 0 && adoptKeys.length > 0 && Object.values(channelPhases).includes("staged")
            ? "Adopt and resume install"
            : conflicts.length > 0 && adoptKeys.length > 0
              ? "Adopt and install"
              : Object.values(channelPhases).includes("staged")
                ? "Resume install"
            : questions
              ? "Submit answers"
              : "Install"}
      </Button>
    </div>
  );
}
