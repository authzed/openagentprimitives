import {
  createContext,
  useContext,
  useMemo,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import type { SimStore } from "../store/simstore";
import type { Scenario, SimChannel, SimUser } from "../store/types";
import type { MrkdwnContext } from "../blockkit/mrkdwn";

const StoreCtx = createContext<SimStore | null>(null);

export function StoreProvider({
  store,
  children,
}: {
  store: SimStore;
  children: ReactNode;
}) {
  return <StoreCtx.Provider value={store}>{children}</StoreCtx.Provider>;
}

export function useStore(): SimStore {
  const s = useContext(StoreCtx);
  if (!s) throw new Error("useStore must be used within a StoreProvider");
  return s;
}

export function useScenario(): Scenario {
  const store = useStore();
  return useSyncExternalStore(store.subscribe, store.getSnapshot);
}

export interface Lookups {
  usersById: Map<string, SimUser>;
  channelsById: Map<string, SimChannel>;
  mrkdwn: MrkdwnContext;
  user: (id: string) => SimUser | undefined;
  channel: (id: string) => SimChannel | undefined;
}

export function useLookups(): Lookups {
  const scenario = useScenario();
  return useMemo(() => {
    const usersById = new Map(scenario.users.map((u) => [u.id, u]));
    const channelsById = new Map(scenario.channels.map((c) => [c.id, c]));
    const mrkdwn: MrkdwnContext = {
      resolveUser: (id) => usersById.get(id)?.name ?? id,
      resolveChannel: (id) => channelsById.get(id)?.name ?? id,
    };
    return {
      usersById,
      channelsById,
      mrkdwn,
      user: (id: string) => usersById.get(id),
      channel: (id: string) => channelsById.get(id),
    };
  }, [scenario.users, scenario.channels]);
}
