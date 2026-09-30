import type { MDXComponents } from "mdx/types";
import Link from "next/link";
import { Clip, Screenshot, ScreenshotSeries } from "@/components/media";
import { Callout } from "@/components/Callout";
import { Coverage } from "@/components/Coverage";
import { linkKind } from "@/lib/links";

// The component set every guide gets without importing anything. Custom
// components are capitalized so MDX resolves them here, not as HTML tags.
const components: MDXComponents = {
  Clip,
  Screenshot,
  ScreenshotSeries,
  Callout,
  Coverage,
  a: ({ href = "", ...props }) => {
    switch (linkKind(href)) {
      case "docs":
        return <Link href={href} {...props} />;
      case "external":
        return <a href={href} target="_blank" rel="noreferrer" {...props} />;
      default:
        return <a href={href} {...props} />;
    }
  },
};

export function useMDXComponents(): MDXComponents {
  return components;
}
