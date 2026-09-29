"use client";
import posthog from "posthog-js";
import { useEffect } from "react";
import { posthogOptions } from "@/lib/analytics";

export function Analytics() {
  useEffect(() => {
    const opts = posthogOptions({
      key: process.env.NEXT_PUBLIC_POSTHOG_KEY,
      vercelEnv: process.env.NEXT_PUBLIC_VERCEL_ENV,
    });
    if (opts) posthog.init(opts.key, opts.config);
  }, []);
  return null;
}
