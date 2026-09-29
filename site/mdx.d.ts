declare module "*.mdx" {
  import type { ComponentType } from "react";
  import type { GuideMeta } from "@/lib/nav";
  export const meta: GuideMeta | undefined;
  const MDXComponent: ComponentType;
  export default MDXComponent;
}
