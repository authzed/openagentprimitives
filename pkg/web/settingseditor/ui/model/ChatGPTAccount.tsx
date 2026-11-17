import * as React from "react";
import { Button, Label, Alert, AlertDescription } from "@ap/design";

type Account = { id: string; label: string; signedIn: boolean };
type Model = { slug: string; display_name: string };

async function request<T>(path: string, account?: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, account === undefined ? { signal } : {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ account }), signal,
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json() as Promise<T>;
}

export function ChatGPTAccount({ account, onAccount, model, onModel }: {
  account: string; onAccount: (value: string) => void;
  model: string; onModel: (value: string) => void;
}) {
  const [accounts, setAccounts] = React.useState<Account[]>([]);
  const [models, setModels] = React.useState<Model[]>([]);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [generation, reload] = React.useReducer((n: number) => n + 1, 0);
  const loginAbort = React.useRef<AbortController | null>(null);
  React.useEffect(() => () => loginAbort.current?.abort(), []);
  React.useEffect(() => {
    const abort = new AbortController();
    request<Account[]>("/accounts", undefined, abort.signal).then(setAccounts).catch((e: Error) => {
      if (!abort.signal.aborted) setError(e.message);
    });
    return () => abort.abort();
  }, [generation]);
  React.useEffect(() => {
    const abort = new AbortController(); setModels([]);
    if (account) request<Model[]>("/accounts/models?account=" + encodeURIComponent(account), undefined, abort.signal)
      .then(setModels).catch((e: Error) => { if (!abort.signal.aborted) setError(e.message); });
    return () => abort.abort();
  }, [account, generation]);

  async function authenticate(signOut: boolean) {
    const abort = new AbortController(); loginAbort.current = abort;
    setBusy(true); setError("");
    try {
      const result = await request<{ account?: string }>("/accounts/" + (signOut ? "logout" : "login"), account, abort.signal);
      if (!abort.signal.aborted) { onAccount(result.account || ""); onModel(""); reload(); }
    } catch (e) { if (!abort.signal.aborted) setError(e instanceof Error ? e.message : String(e)); }
    finally { if (!abort.signal.aborted) setBusy(false); }
  }

  return <section className="flex flex-col gap-2">
    <Label htmlFor="chatgpt-account">ChatGPT account</Label>
    <select id="chatgpt-account" className="rounded border p-2 bg-background" value={account} disabled={busy} onChange={(e) => { onAccount(e.target.value); onModel(""); setError(""); }}>
      <option value="">Add an account</option>
      {accounts.map((a) => <option key={a.id} value={a.id}>{a.label}{a.signedIn ? "" : " (signed out)"}</option>)}
    </select>
    <Button data-oai-siwc-button disabled={busy} onClick={() => void authenticate(false)}>{busy ? "Waiting for sign-in…" : "Continue with ChatGPT"}</Button>
    <Button variant="outline" disabled={busy || !account} onClick={() => void authenticate(true)}>Sign out of selected account</Button>
    <Label htmlFor="chatgpt-model">Available model</Label>
    <select id="chatgpt-model" className="rounded border p-2 bg-background" value={model} onChange={(e) => onModel(e.target.value)}>
      <option value="">Select a model</option>
      {models.map((m) => <option key={m.slug} value={m.slug}>{m.display_name}</option>)}
    </select>
    <p className="text-sm text-muted-foreground">Uses your eligible ChatGPT plan and its app usage limits. <a href="https://chatgpt.com/settings/usage" target="_blank" rel="noreferrer">Manage usage in ChatGPT</a>.</p>
    {error && <Alert variant="destructive"><AlertDescription>{error}</AlertDescription></Alert>}
  </section>;
}
