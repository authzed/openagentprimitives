import { useState, type KeyboardEvent } from "react";
import { SendHorizontal } from "lucide-react";
import { Button, cn } from "@ap/design";

// ChatInput is the message composer: a growing textarea plus a send button.
// Enter sends; Shift+Enter inserts a newline. There is no `Input`/`Textarea`
// primitive in @ap/design yet, so the textarea is styled locally to match the
// existing shadcn input conventions (border-input, bg-transparent, ring on
// focus — see web/packages/design/src/components/ui/select.tsx's trigger).
export function ChatInput({
  disabled,
  placeholder,
  onSend,
}: {
  disabled: boolean;
  placeholder: string;
  onSend: (text: string) => void;
}) {
  const [text, setText] = useState("");

  const submit = () => {
    const trimmed = text.trim();
    if (!trimmed || disabled) return;
    onSend(trimmed);
    setText("");
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="flex items-end gap-2 px-4 py-3 border-t border-border bg-card">
      <textarea
        rows={1}
        value={text}
        disabled={disabled}
        placeholder={placeholder}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={onKeyDown}
        className={cn(
          "flex-1 resize-none max-h-40 rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm",
          "placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring",
          "disabled:cursor-not-allowed disabled:opacity-50",
        )}
      />
      <Button
        size="icon"
        className="h-9 w-9 shrink-0"
        disabled={disabled || !text.trim()}
        onClick={submit}
        title="Send"
      >
        <SendHorizontal className="h-4 w-4" />
      </Button>
    </div>
  );
}
