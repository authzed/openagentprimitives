import * as React from "react";
import {
  Alert, AlertDescription, Badge, Button,
  Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger,
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@ap/design";
import {
  ApiError, OapInstallConflictsError, OapInstallQuestionsError,
  del, getWorkshops, installWorkshop, postJSON,
  type OapInstallConflict, type OapQuestion, type WorkshopDecisionResponse, type WorkshopRow,
} from "../lib/api";

// INPUT_CLASS matches the text-input styling already used across the console
// (InstallAgentForm, ResourceTable's filter box, AuditExplorer's search field).
const INPUT_CLASS =
  "w-full rounded-md border border-border bg-background px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50";

// Field is the small labeled-row wrapper InstallAgentForm's own fields use,
// kept local here (rather than a cross-import from config/detail/shared) so
// this view stays self-contained — it is an 8-line wrapper, not a seam.
function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">{label}</span>
      <div className="text-sm text-foreground">{children}</div>
    </div>
  );
}

// defaultAsString renders an OapQuestion.default (an untyped JSON value) into
// the plain string a text/number/checkbox input needs. Mirrors
// InstallAgentForm's own helper of the same name and behavior exactly, so the
// two forms never disagree about what a question's default means.
function defaultAsString(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (Array.isArray(v)) return v.join(",");
  return String(v);
}

// QuestionField renders one auto-generated install-question input, dispatching
// on OapQuestion.type exactly as InstallAgentForm's QuestionField does:
// string/resourceList → text, int → number, bool → checkbox, enum → select,
// secret → password (masked — the one type whose value must never be echoed
// back, logged, or pre-filled). required defaults to true when unset.
function QuestionField({
  q, value, onChange,
}: {
  q: OapQuestion;
  value: string;
  onChange: (v: string) => void;
}) {
  const required = q.required !== false;
  const label = `${q.prompt || q.name}${required ? " *" : ""}`;

  return (
    <Field label={label}>
      {q.type === "bool" ? (
        <input
          type="checkbox"
          checked={value === "true"}
          onChange={(e) => onChange(e.target.checked ? "true" : "false")}
          aria-label={label}
          className="h-4 w-4 rounded border-border"
        />
      ) : q.type === "enum" ? (
        <select value={value} onChange={(e) => onChange(e.target.value)} aria-label={label} className={INPUT_CLASS}>
          <option value="" disabled>
            Select…
          </option>
          {(q.enum ?? []).map((opt, i) => {
            const el = q.enumLabels?.[i];
            const label2 = el !== undefined && el.trim() !== "" ? el : opt;
            return (
              <option key={opt} value={opt}>
                {label2}
              </option>
            );
          })}
        </select>
      ) : (
        <input
          type={q.type === "secret" ? "password" : q.type === "int" ? "number" : "text"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
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

// InstallDialog is the Install action for a workshop row. It POSTs with no
// answers first; a 400 carrying `questions` (OapInstallQuestionsError) renders
// exactly those fields and re-POSTs — the same non-interactive-first flow
// InstallAgentForm drives against /agents/oap-install. A 409 carrying
// `conflicts` (OapInstallConflictsError) renders the adopt-confirm checkboxes.
// No secret answer is ever pre-filled: `values` starts empty and only a
// non-secret manifest default seeds a field.
function InstallDialog({
  apiBase, row, onDone,
}: {
  apiBase: string;
  row: WorkshopRow;
  onDone: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [namespace, setNamespace] = React.useState(row.namespace);
  const [name, setName] = React.useState(row.suggestedName ?? "");
  const [values, setValues] = React.useState<Record<string, string>>({});
  const [questions, setQuestions] = React.useState<OapQuestion[] | null>(null);
  const [conflicts, setConflicts] = React.useState<OapInstallConflict[]>([]);
  const [adoptKeys, setAdoptKeys] = React.useState<string[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);

  // resetAdoption clears both the rendered conflict list and the operator's
  // ticks — mirrors InstallAgentForm.resetAdoption: a tick is only meaningful
  // against the specific conflict list it was rendered for, and any edit that
  // can change what an object resolves to (target, or an answer) invalidates it.
  const resetAdoption = () => {
    setConflicts([]);
    setAdoptKeys([]);
  };

  const setAnswer = (qname: string, v: string) => {
    setValues((prev) => ({ ...prev, [qname]: v }));
    resetAdoption();
  };

  const reset = () => {
    setError(null);
    setQuestions(null);
    resetAdoption();
  };

  const submit = async () => {
    setError(null);
    setSubmitting(true);
    try {
      await installWorkshop(apiBase, row.namespace, row.name, { namespace, name, values, adopt: adoptKeys });
      setOpen(false);
      reset();
      onDone();
    } catch (e) {
      if (e instanceof OapInstallQuestionsError) {
        setQuestions(e.questions);
        // Seed `values` from each question's own suggested Default — never
        // overwrites an answer already typed, and never applies to a value the
        // admin must type themselves (a manifest simply never sets a Default
        // on a secret-typed question).
        setValues((prev) => {
          const next = { ...prev };
          for (const q of e.questions) {
            if (next[q.name] === undefined && q.default !== undefined && q.default !== null) {
              next[q.name] = defaultAsString(q.default);
            }
          }
          return next;
        });
      } else if (e instanceof OapInstallConflictsError) {
        setConflicts(e.conflicts);
      } else if (e instanceof ApiError) {
        setError(e.message);
      } else {
        setError((e as Error).message);
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) reset();
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm">Install</Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Install {row.suggestedName || row.name}</DialogTitle>
          <DialogDescription>
            Installs this workshop&apos;s drafted bundle under the operator identity. No secret answer is ever
            pre-filled — type it below.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label="Namespace *">
              <input
                type="text"
                value={namespace}
                onChange={(e) => {
                  setNamespace(e.target.value);
                  resetAdoption();
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
                  resetAdoption();
                }}
                placeholder="defaults to the bundle's own name"
                aria-label="Name"
                className={INPUT_CLASS}
              />
            </Field>
          </div>

          {conflicts.length > 0 && (
            <div className="flex flex-col gap-2 rounded-md border border-destructive/50 p-3">
              <p className="text-xs font-medium text-foreground">
                These objects already exist and were not created by this install. Adopting one overwrites its
                spec with this bundle&apos;s.
              </p>
              {conflicts.map((c) => {
                const key = `${c.kind}/${c.name}`;
                const rowLabel = c.namespace ? `${c.kind} ${c.namespace}/${c.name}` : key;
                return (
                  <label key={key} className="flex items-center gap-2 text-xs text-foreground">
                    <input
                      type="checkbox"
                      checked={adoptKeys.includes(key)}
                      aria-label={rowLabel}
                      onChange={(e) =>
                        setAdoptKeys((prev) => (e.target.checked ? [...prev, key] : prev.filter((k) => k !== key)))
                      }
                      className="h-4 w-4 rounded border-border"
                    />
                    <span>{rowLabel}</span>
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
                <QuestionField
                  key={q.name}
                  q={q}
                  value={values[q.name] ?? defaultAsString(q.default)}
                  onChange={(v) => setAnswer(q.name, v)}
                />
              ))}
            </div>
          )}
        </div>

        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">Cancel</Button>
          </DialogClose>
          <Button onClick={submit} disabled={submitting || namespace.trim() === ""}>
            {submitting
              ? "Installing…"
              : conflicts.length > 0 && adoptKeys.length > 0
                ? "Adopt and install"
                : questions
                  ? "Submit answers"
                  // Deliberately NOT "Install" — the row's own trigger button
                  // (which stays mounted while the dialog is open) already
                  // carries that label, and two identically-named buttons on
                  // screen at once is an accessible-name collision.
                  : "Confirm install"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// DeclineButton POSTs .../decline — a terminal, non-destructive decision (no
// confirm dialog, unlike Kill: declining leaves the Workshop CR and its
// builder session untouched, only recording that an admin looked and said no).
function DeclineButton({
  apiBase, row, onDone,
}: {
  apiBase: string;
  row: WorkshopRow;
  onDone: () => void;
}) {
  const [declining, setDeclining] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const decline = async () => {
    setError(null);
    setDeclining(true);
    try {
      await postJSON<WorkshopDecisionResponse>(`${apiBase}/workshops/${row.namespace}/${row.name}/decline`, {});
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setDeclining(false);
    }
  };

  return (
    <span className="flex flex-col items-end gap-1">
      <Button variant="outline" size="sm" onClick={decline} disabled={declining}>
        {declining ? "Declining…" : "Decline"}
      </Button>
      {error && <span className="text-[11px] text-destructive">{error}</span>}
    </span>
  );
}

// KillDialog is the confirm-then-DELETE affordance, mirroring SessionPage's
// own kill() action + confirm dialog shape exactly.
function KillDialog({
  apiBase, row, onDone,
}: {
  apiBase: string;
  row: WorkshopRow;
  onDone: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [killing, setKilling] = React.useState(false);

  const kill = async () => {
    setError(null);
    setKilling(true);
    try {
      await del(`${apiBase}/workshops/${row.namespace}/${row.name}`); // 404 = already gone — treated as success by del()
      setOpen(false);
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setKilling(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="destructive" size="sm">
          Kill
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Kill workshop {row.name}?</DialogTitle>
          <DialogDescription>
            Deletes the Workshop — its provisioned builder namespace is torn down. This cannot be undone.
          </DialogDescription>
        </DialogHeader>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">Cancel</Button>
          </DialogClose>
          <Button variant="destructive" onClick={kill} disabled={killing}>
            {killing ? "Killing…" : "Kill it"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// WorkshopsView lists every Workshop cluster-wide (newest-first): an
// agent-builder session's drafted bundle, awaiting an admin's install /
// decline / kill decision. Install and Decline show only while pendingInstall
// is true; Kill is always available. Every action refetches the list so the
// row's phase/pending flags reflect the outcome.
export function WorkshopsView({ apiBase }: { apiBase: string }) {
  const [rows, setRows] = React.useState<WorkshopRow[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loaded, setLoaded] = React.useState(false);

  const load = React.useCallback(() => {
    getWorkshops(apiBase)
      .then((r) => {
        setRows(r);
        setError(null);
        setLoaded(true);
      })
      .catch((e: Error) => {
        setError(e.message);
        setLoaded(true);
      });
  }, [apiBase]);

  React.useEffect(() => {
    load();
  }, [load]);

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>Could not load workshops: {error}</AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="space-y-3">
      {loaded && rows.length === 0 && <p className="text-sm text-muted-foreground">No workshops yet.</p>}
      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Workshop</TableHead>
              <TableHead>Session</TableHead>
              <TableHead>Starter</TableHead>
              <TableHead>Phase</TableHead>
              <TableHead>Install</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => {
              const key = `${row.namespace}/${row.name}`;
              return (
                <TableRow key={key}>
                  <TableCell className="font-mono text-xs">
                    {key}
                    {row.exported && (
                      <Badge variant="outline" className="ml-2">
                        exported
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{row.session}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{row.starter}</TableCell>
                  <TableCell>
                    <Badge variant="outline">{row.phase || "—"}</Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {row.installPhase ? (
                      <span className="flex items-center gap-2">
                        <Badge variant={row.installPhase === "Failed" ? "destructive" : "outline"}>
                          {row.installPhase}
                        </Badge>
                        {row.installedRef && <span>{row.installedRef}</span>}
                      </span>
                    ) : row.pendingCapability ? (
                      "capability requested"
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-2">
                      {row.pendingInstall && (
                        <>
                          <InstallDialog apiBase={apiBase} row={row} onDone={load} />
                          <DeclineButton apiBase={apiBase} row={row} onDone={load} />
                        </>
                      )}
                      <KillDialog apiBase={apiBase} row={row} onDone={load} />
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
      <p className="text-[11px] text-muted-foreground">
        Workshop — an agent-builder session&apos;s drafted bundle, awaiting an admin&apos;s install/decline/kill
        decision.
      </p>
    </div>
  );
}
