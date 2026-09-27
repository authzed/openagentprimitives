import type { MDXComponents } from "mdx/types";
import { Clip, Screenshot, ScreenshotSeries } from "./media";
import { Callout } from "./Callout";
import { Coverage } from "./Coverage";

// The component set available to every MDX guide: the media embeds, callouts,
// and lightly-styled base elements. Custom components are capitalized so MDX
// resolves them here rather than as HTML tags.
export const mdxComponents: MDXComponents = {
  Clip,
  Screenshot,
  ScreenshotSeries,
  Callout,
  Coverage,
  a: (props) => (
    <a
      {...props}
      target={props.href?.startsWith("http") ? "_blank" : undefined}
      rel="noreferrer"
    />
  ),
};
