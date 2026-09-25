import { ConfigDetail } from "./ConfigDetail";

// ChannelPage is the Channel detail. Tabs = Overview (kind/role/agent class +
// identity links/session scope) + Connection (credentials secret), plus Health
// when the channel is not Connected.
export function ChannelPage({ apiBase, id, tab }: { apiBase: string; id: string; tab?: string }) {
  return (
    <ConfigDetail
      apiBase={apiBase}
      resource="channels"
      entity="channel"
      id={id}
      tab={tab}
      backRoute={{ type: "view", view: "channels" }}
    />
  );
}
