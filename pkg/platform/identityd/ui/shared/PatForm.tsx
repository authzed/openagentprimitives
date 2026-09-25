import { Button } from "@ap/design";

// PatForm is a native-submitting (POST) form for pasting a personal-access token.
// hidden carries any extra hidden fields (e.g. link + credential for /link/submit).
// instructions/docsUrl are optional setup guidance rendered above the form.
export function PatForm({ action, hidden, submitLabel, instructions, docsUrl }: {
  action: string;
  hidden?: Record<string, string>;
  submitLabel: string;
  instructions?: string;
  docsUrl?: string;
}) {
  return (
    <>
      {instructions && (
        <div className="text-xs text-muted-foreground mb-3 whitespace-pre-line">
          {instructions}
          {docsUrl && (
            <>
              {" "}
              <a href={docsUrl} target="_blank" rel="noreferrer" className="underline">
                How to get this token
              </a>
            </>
          )}
        </div>
      )}
      <form method="POST" action={action} autoComplete="off" className="flex gap-2 items-center">
        {hidden &&
          Object.entries(hidden).map(([k, v]) => <input key={k} type="hidden" name={k} value={v} />)}
        <input
          type="password"
          name="token"
          placeholder="Token"
          required
          className="flex-1 rounded-md bg-background border border-input px-2.5 py-1.5 text-sm outline-none focus:ring-2 focus:ring-ring"
        />
        <Button type="submit" size="sm">{submitLabel}</Button>
      </form>
    </>
  );
}
