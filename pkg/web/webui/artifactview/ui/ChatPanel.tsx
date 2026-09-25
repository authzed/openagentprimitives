import { useState } from "react";
import { Markdown } from "@ap/design";
import type { ChatMessage } from "./chatMessages";

export function ChatPanel(props: {
  messages: ChatMessage[];
  canCompose: boolean;
  onSend: (text: string) => Promise<void>;
}) {
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const send = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    try { await props.onSend(text); setDraft(""); } finally { setSending(false); }
  };
  return (
    <div className="flex flex-col h-full border-l border-border bg-card">
      <div className="flex-1 overflow-y-auto p-3 space-y-2">
        {props.messages.map((m, i) => (
          <div key={i} className={`flex ${m.role === "user" ? "justify-end" : "justify-start"}`}>
            <div className={`inline-block max-w-[85%] rounded-lg px-3 py-1.5 text-sm text-left ${m.role === "user" ? "bg-primary text-primary-foreground" : "bg-muted text-foreground"}`}>
              <Markdown>{m.text}</Markdown>
            </div>
          </div>
        ))}
      </div>
      {props.canCompose && (
        <div className="flex gap-2 p-2 border-t border-border">
          <input
            className="flex-1 rounded border border-border bg-background px-2 py-1 text-sm"
            value={draft} onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") void send(); }}
            placeholder="Message the agent…" disabled={sending}
          />
          <button className="rounded bg-primary px-3 text-sm text-primary-foreground disabled:opacity-50"
            onClick={() => void send()} disabled={sending}>Send</button>
        </div>
      )}
    </div>
  );
}
