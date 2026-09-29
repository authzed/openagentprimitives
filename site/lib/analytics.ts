import type { PostHogConfig } from "posthog-js";

// This is a public repo, so tracking is deliberately minimal and legible in
// this one file: page views and page leaves, counted without cookies, storage,
// or identity (PostHog's cookieless server-hash mode, which the PostHog project
// must have enabled). No autocapture, replay, heatmaps, surveys, or flags.
// Only a production deploy with a key configured sends anything; forks,
// previews and local dev never do.
export function posthogOptions(env: {
  key?: string;
  vercelEnv?: string;
}): { key: string; config: Partial<PostHogConfig> } | null {
  if (!env.key || env.vercelEnv !== "production") return null;
  return {
    key: env.key,
    config: {
      api_host: "https://i.authzed.com",
      defaults: "2026-01-30",
      cookieless_mode: "always",
      person_profiles: "never",
      capture_pageview: "history_change",
      capture_pageleave: true,
      autocapture: false,
      capture_heatmaps: false,
      capture_dead_clicks: false,
      capture_exceptions: false,
      disable_session_recording: true,
      disable_surveys: true,
      disable_web_experiments: true,
      advanced_disable_flags: true,
      disable_external_dependency_loading: true,
      respect_dnt: true,
    },
  };
}
