import type { Metadata } from "next";
import { Landing } from "./Landing";
import "./landing.css";

export const metadata: Metadata = {
  title: {
    absolute: "Open Agent Primitives: a secure way to run enterprise AI agents",
  },
  openGraph: {
    title: "Open Agent Primitives",
    description:
      "Building blocks for running enterprise AI agents in your own cluster. The AI never decides what it's allowed to do: OAP checks every action before it runs.",
    type: "website",
  },
};

export default function Page() {
  return <Landing />;
}
