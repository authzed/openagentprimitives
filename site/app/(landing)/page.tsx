import type { Metadata } from "next";
import { JsonLd } from "@/components/JsonLd";
import { OG_IMAGE, SITE_DESCRIPTION, SITE_NAME, SITE_TITLE } from "@/lib/site";
import { landingGraph } from "@/lib/structured-data";
import { Landing } from "./Landing";
import "./landing.css";

export const metadata: Metadata = {
  title: { absolute: SITE_TITLE },
  description: SITE_DESCRIPTION,
  // The docs index for LLMs and agents (app/llms.txt).
  alternates: { canonical: "/", types: { "text/plain": "/llms.txt" } },
  openGraph: {
    title: SITE_TITLE,
    description: SITE_DESCRIPTION,
    url: "/",
    siteName: SITE_NAME,
    type: "website",
    images: [OG_IMAGE],
  },
  twitter: { card: "summary_large_image", images: [OG_IMAGE.url] },
};

export default function Page() {
  return (
    <>
      <JsonLd data={landingGraph()} />
      <Landing />
    </>
  );
}
