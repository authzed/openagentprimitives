import { Blocks } from "../blockkit/BlockKit";
import { useLookups } from "../runtime/context";
import type { SimChannel } from "../store/types";
import { ChannelHeader } from "./ChannelHeader";

// The App Home surface (the "hub" screen an OAP app publishes). The home tab is
// a Block Kit `view`, so we render the captured/authored blocks verbatim — the
// same renderer used for messages — inside the wider home column.
export function AppHome({ channel }: { channel: SimChannel }) {
  const { mrkdwn } = useLookups();
  return (
    <section className="sk-pane sk-pane--home">
      <ChannelHeader channel={channel} />
      <div className="sk-home-scroll">
        <div className="sk-home-column">
          {channel.home ? (
            <Blocks blocks={channel.home} ctx={mrkdwn} />
          ) : (
            <div className="sk-home-empty">
              This app has not published a Home tab.
            </div>
          )}
        </div>
      </div>
    </section>
  );
}
