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
      "Building blocks for constructing and running enterprise AI agents in your own cluster, with every control outside the model.",
    type: "website",
  },
};

export default function Page() {
  return <Landing />;
}
