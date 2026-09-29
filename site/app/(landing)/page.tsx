import type { Metadata } from "next";
import { Landing } from "./Landing";
import "./landing.css";

export const metadata: Metadata = {
  title: { absolute: "Open Agent Primitives — the agent proposes, SpiceDB decides" },
  openGraph: {
    title: "Open Agent Primitives",
    description: "A Kubernetes-native runtime for LLM agents, with authorization decided outside the model.",
    type: "website",
  },
};

export default function Page() {
  return <Landing />;
}
