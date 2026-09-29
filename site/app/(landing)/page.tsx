import type { Metadata } from "next";
import { OG_IMAGE, SITE_DESCRIPTION, SITE_NAME } from "@/lib/site";
import { Landing } from "./Landing";
import "./landing.css";

export const metadata: Metadata = {
  title: {
    absolute: "Open Agent Primitives: a secure way to run enterprise AI agents",
  },
  description: SITE_DESCRIPTION,
  alternates: { canonical: "/" },
  openGraph: {
    title: SITE_NAME,
    description: SITE_DESCRIPTION,
    url: "/",
    siteName: SITE_NAME,
    type: "website",
    images: [OG_IMAGE],
  },
  twitter: { card: "summary_large_image", images: [OG_IMAGE.url] },
};

export default function Page() {
  return <Landing />;
}
