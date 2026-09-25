export interface ChatMessage {
  role: "agent" | "user";
  text: string;
  author: string;
  at: string;
}

const MAX = 200;

// appendMessage adds m and bounds history to the last MAX entries. The outbound
// stream (respond_to_user + user_echo) is the single source of truth, so there is
// no optimistic entry to dedup against.
export function appendMessage(list: ChatMessage[], m: ChatMessage): ChatMessage[] {
  const next = list.concat(m);
  return next.length > MAX ? next.slice(next.length - MAX) : next;
}
