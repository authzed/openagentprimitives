declare module "*.mdx" {
  import type { ComponentType } from "react";
  export const meta:
    | {
        title: string;
        section?: string;
        group?: string;
        order?: number;
        description?: string;
      }
    | undefined;
  const MDXComponent: ComponentType;
  export default MDXComponent;
}
