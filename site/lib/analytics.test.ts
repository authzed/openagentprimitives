import { describe, expect, it } from "vitest";
import { posthogOptions } from "./analytics";

describe("posthogOptions", () => {
  it.each([
    ["no key, production", { vercelEnv: "production" }, false],
    ["key, preview deploy", { key: "phc_x", vercelEnv: "preview" }, false],
    ["key, local dev (no VERCEL_ENV)", { key: "phc_x" }, false],
    ["empty key, production", { key: "", vercelEnv: "production" }, false],
    ["key, production", { key: "phc_x", vercelEnv: "production" }, true],
  ])("%s → initializes: %s", (_name, env, want) => {
    expect(posthogOptions(env) !== null).toBe(want);
  });

  it("production config is cookieless, anonymous, and pageview-only", () => {
    const opts = posthogOptions({ key: "phc_x", vercelEnv: "production" })!;
    expect(opts.key).toBe("phc_x");
    expect(opts.config).toMatchObject({
      api_host: "https://i.authzed.com",
      cookieless_mode: "always",
      person_profiles: "never",
      capture_pageview: "history_change",
      capture_pageleave: true,
      autocapture: false,
      disable_session_recording: true,
      capture_heatmaps: false,
      disable_surveys: true,
      advanced_disable_flags: true,
      disable_external_dependency_loading: true,
      respect_dnt: true,
    });
  });
});
