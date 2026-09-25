// ChatEmbed is the whole of the chat-embed page: the session socket, the
// startup line above the chat, and the chat itself — the same three pieces
// the session shell mounts for its conversation tab, and nothing else. It is
// meant to be framed (ap:chat), so it draws no header and no navigation.
import * as React from "react";
import { ChatView } from "../../chat/ui/ChatView";
import { SessionSocketProvider } from "../../chat/ui/SessionSocket";
import { StartupLine } from "../../sessions/ui/StartupLine";

export function ChatEmbed({ ns, name }: { ns: string; name: string }): React.ReactElement {
  return (
    <SessionSocketProvider ns={ns} name={name}>
      <div data-testid="chat-embed-root" className="flex h-full min-h-screen flex-col">
        <StartupLine />
        <ChatView ns={ns} name={name} readOnly={false} />
      </div>
    </SessionSocketProvider>
  );
}
