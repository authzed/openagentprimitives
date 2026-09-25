import { describe, expect, it } from "vitest";
import {
  updateSpec,
  getPath,
  boolTriState,
  boolFromTriState,
  listTriState,
  parseLines,
  joinLines,
  setUnionList,
  defaultTokenRefFor,
  upsertCatalogEntry,
  removeCatalogEntry,
  replaceCatalogEntry,
  setDefaultCatalogEntry,
  summarizePinning,
  summarizeToolGuardCeiling,
  summarizeContentInspectors,
  summarizeAllowedMCPServers,
  type SettingsSpec,
  type ModelCatalogEntry,
} from "./spec";

describe("updateSpec", () => {
  it("does not mutate the original spec object", () => {
    const original: SettingsSpec = { limits: { minPlanGateMode: "logging" } };
    const snapshot = JSON.stringify(original);
    updateSpec(original, ["limits", "minPlanGateMode"], "enforcing");
    expect(JSON.stringify(original)).toBe(snapshot);
  });

  it("sets a deeply nested value, creating intermediate objects that don't exist yet", () => {
    const spec: SettingsSpec = {};
    const next = updateSpec(spec, ["limits", "budget", "maxTurns"], 10);
    expect(next.limits?.budget?.maxTurns).toBe(10);
    // original untouched
    expect(spec.limits).toBeUndefined();
  });

  it("preserves sibling keys not on the edited path, including ones this TS file doesn't type", () => {
    const spec = {
      limits: {
        minPlanGateMode: "logging",
        requireStandingFor: ["agentclass"], // not modeled in SettingsLimits here
      },
      defaults: { reportSessionCost: true },
    } as unknown as SettingsSpec;
    const next = updateSpec(spec, ["limits", "minPlanGateMode"], "enforcing");
    expect(next.limits?.minPlanGateMode).toBe("enforcing");
    expect((next.limits as unknown as Record<string, unknown>).requireStandingFor).toEqual(["agentclass"]);
    expect(next.defaults?.reportSessionCost).toBe(true);
  });

  it("deletes the key when value is undefined, rather than setting a literal undefined", () => {
    const spec: SettingsSpec = { limits: { allowModelOverride: true, minPlanGateMode: "logging" } };
    const next = updateSpec(spec, ["limits", "allowModelOverride"], undefined);
    expect("allowModelOverride" in (next.limits ?? {})).toBe(false);
    expect(next.limits?.minPlanGateMode).toBe("logging");
  });

  it("sets a whole array value at a path (modelCatalog-style)", () => {
    const spec: SettingsSpec = {};
    const next = updateSpec(spec, ["modelCatalog"], [{ name: "a" }]);
    expect(next.modelCatalog).toEqual([{ name: "a" }]);
  });

  it("throws on an empty path", () => {
    expect(() => updateSpec({}, [], "x")).toThrow();
  });
});

describe("getPath", () => {
  it("reads a nested value that exists", () => {
    const spec: SettingsSpec = { limits: { budget: { maxTurns: 5 } } };
    expect(getPath(spec, ["limits", "budget", "maxTurns"])).toBe(5);
  });

  it("returns undefined past a missing intermediate key", () => {
    const spec: SettingsSpec = {};
    expect(getPath(spec, ["limits", "budget", "maxTurns"])).toBeUndefined();
  });

  it("agrees with updateSpec: reading back what was just written", () => {
    const spec = updateSpec({}, ["defaults", "authz", "scopeMaxLlmLatencyMs"], 250);
    expect(getPath(spec, ["defaults", "authz", "scopeMaxLlmLatencyMs"])).toBe(250);
  });
});

describe("bool tri-state (undefined | true | false), both directions", () => {
  it("undefined -> 'unset'", () => expect(boolTriState(undefined)).toBe("unset"));
  it("true -> 'true'", () => expect(boolTriState(true)).toBe("true"));
  it("false -> 'false'", () => expect(boolTriState(false)).toBe("false"));

  it("'unset' -> undefined", () => expect(boolFromTriState("unset")).toBeUndefined());
  it("'true' -> true", () => expect(boolFromTriState("true")).toBe(true));
  it("'false' -> false", () => expect(boolFromTriState("false")).toBe(false));

  it("round-trips every state through updateSpec both directions", () => {
    const spec: SettingsSpec = {};
    const toTrue = updateSpec(spec, ["limits", "nativeFileHandling"], boolFromTriState("true"));
    expect(toTrue.limits?.nativeFileHandling).toBe(true);
    const toFalse = updateSpec(toTrue, ["limits", "nativeFileHandling"], boolFromTriState("false"));
    expect(toFalse.limits?.nativeFileHandling).toBe(false);
    const toUnset = updateSpec(toFalse, ["limits", "nativeFileHandling"], boolFromTriState("unset"));
    expect("nativeFileHandling" in (toUnset.limits ?? {})).toBe(false);
  });
});

describe("list tri-state (undefined | [] | [items]), both directions", () => {
  it("undefined -> 'unrestricted'", () => expect(listTriState(undefined)).toBe("unrestricted"));
  it("[] -> 'deny-all'", () => expect(listTriState([])).toBe("deny-all"));
  it("['x'] -> 'restricted'", () => expect(listTriState(["x"])).toBe("restricted"));

  it("round-trips unrestricted -> restricted(empty, deny-all) -> restricted(items) -> unrestricted via updateSpec", () => {
    let spec: SettingsSpec = {};
    expect(listTriState(spec.limits?.allowedToolkits)).toBe("unrestricted");

    spec = updateSpec(spec, ["limits", "allowedToolkits"], []);
    expect(listTriState(spec.limits?.allowedToolkits)).toBe("deny-all");

    spec = updateSpec(spec, ["limits", "allowedToolkits"], ["spicebox-toolkit-a"]);
    expect(listTriState(spec.limits?.allowedToolkits)).toBe("restricted");
    expect(spec.limits?.allowedToolkits).toEqual(["spicebox-toolkit-a"]);

    spec = updateSpec(spec, ["limits", "allowedToolkits"], undefined);
    expect(listTriState(spec.limits?.allowedToolkits)).toBe("unrestricted");
    expect("allowedToolkits" in (spec.limits ?? {})).toBe(false);
  });
});

describe("parseLines / joinLines", () => {
  it("splits on newlines, trims, and drops blank lines", () => {
    expect(parseLines("a\n  b  \n\nc\n")).toEqual(["a", "b", "c"]);
  });
  it("joins with newlines, treating undefined as empty", () => {
    expect(joinLines(undefined)).toBe("");
    expect(joinLines(["a", "b"])).toBe("a\nb");
  });
});

describe("setUnionList", () => {
  it("sets the array when the text has entries", () => {
    const spec: SettingsSpec = {};
    const next = setUnionList(spec, ["limits", "deniedModels"], "model-a\nmodel-b");
    expect(next.limits?.deniedModels).toEqual(["model-a", "model-b"]);
  });

  it("deletes the key (not [])  when the text is empty — plain lists have no deny-all tri-state", () => {
    const spec: SettingsSpec = { limits: { deniedModels: ["model-a"] } };
    const next = setUnionList(spec, ["limits", "deniedModels"], "   \n  \n");
    expect("deniedModels" in (next.limits ?? {})).toBe(false);
  });
});

describe("model catalog helpers", () => {
  const anthropicEntry: ModelCatalogEntry = {
    name: "claude-opus",
    provider: "anthropic",
    tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-claude-opus", key: "token" },
    default: true,
  };
  const openaiEntry: ModelCatalogEntry = {
    name: "gpt-x",
    provider: "openai",
    tokenRef: { namespace: "agentprimitives-system", name: "model-default-token-gpt-x", key: "token" },
  };

  it("defaultTokenRefFor produces the documented convention", () => {
    expect(defaultTokenRefFor("my-model")).toEqual({
      namespace: "agentprimitives-system",
      name: "model-default-token-my-model",
      key: "token",
    });
  });

  it("upsertCatalogEntry appends a new entry", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry] };
    const next = upsertCatalogEntry(spec, openaiEntry);
    expect(next.modelCatalog).toEqual([anthropicEntry, openaiEntry]);
    // original untouched
    expect(spec.modelCatalog).toEqual([anthropicEntry]);
  });

  it("upsertCatalogEntry replaces an existing entry by name in place", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry, openaiEntry] };
    const edited: ModelCatalogEntry = { ...openaiEntry, outputPerMTok: 12 };
    const next = upsertCatalogEntry(spec, edited);
    expect(next.modelCatalog).toEqual([anthropicEntry, edited]);
  });

  it("upsertCatalogEntry enforces at-most-one default: setting a new default clears every other entry's", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry, openaiEntry] };
    const next = upsertCatalogEntry(spec, { ...openaiEntry, default: true });
    const names = new Map((next.modelCatalog ?? []).map((e) => [e.name, e.default]));
    expect(names.get("gpt-x")).toBe(true);
    expect(names.get("claude-opus")).toBe(false);
  });

  it("removeCatalogEntry drops the named entry and preserves the rest", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry, openaiEntry] };
    const next = removeCatalogEntry(spec, "claude-opus");
    expect(next.modelCatalog).toEqual([openaiEntry]);
  });

  it("removeCatalogEntry on an unknown name is a no-op (full array preserved)", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry] };
    const next = removeCatalogEntry(spec, "nonexistent");
    expect(next.modelCatalog).toEqual([anthropicEntry]);
  });

  it("replaceCatalogEntry renames an entry without leaving the old-named duplicate behind", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry, openaiEntry] };
    const renamed: ModelCatalogEntry = { ...openaiEntry, name: "gpt-y" };
    const next = replaceCatalogEntry(spec, "gpt-x", renamed);
    expect(next.modelCatalog).toEqual([anthropicEntry, renamed]);
  });

  it("setDefaultCatalogEntry marks exactly the named entry default", () => {
    const spec: SettingsSpec = { modelCatalog: [anthropicEntry, openaiEntry] };
    const next = setDefaultCatalogEntry(spec, "gpt-x");
    const names = new Map((next.modelCatalog ?? []).map((e) => [e.name, e.default]));
    expect(names.get("gpt-x")).toBe(true);
    expect(names.get("claude-opus")).toBe(false);
  });
});

describe("summarizePinning", () => {
  it("renders '—' when there are no rules and no bypasses", () => {
    expect(summarizePinning(undefined)).toBe("—");
    expect(summarizePinning({})).toBe("—");
  });

  it("renders each rule as kind(min=...,mode=...) defaulting min/mode, plus a bypass count", () => {
    const s = summarizePinning({
      rules: [
        { kind: "skill", minStrength: "frozen", mode: "block" },
        { kind: "mcp" }, // both defaulted
      ],
      bypass: [{ kind: "skill", name: "x" }],
    });
    expect(s).toBe("skill(min=frozen,mode=block), mcp(min=any,mode=approve) · 1 bypass");
  });
});

describe("summarizeToolGuardCeiling", () => {
  it("renders '—' when unset or empty", () => {
    expect(summarizeToolGuardCeiling(undefined)).toBe("—");
    expect(summarizeToolGuardCeiling({})).toBe("—");
  });

  it("renders each set field, pairing maxCalls with window", () => {
    const s = summarizeToolGuardCeiling({
      maxFailureThreshold: 5,
      minAction: "deny",
      maxCalls: 10,
      window: "1m",
      maxEgressBytes: 2048,
    });
    expect(s).toBe("maxFailureThreshold=5, minAction=deny, maxCalls=10/1m, maxEgressBytes=2048");
  });
});

describe("summarizeContentInspectors", () => {
  it("distinguishes unset from an empty (present) list", () => {
    expect(summarizeContentInspectors(undefined)).toBe("not set");
    expect(summarizeContentInspectors([])).toBe("(none)");
  });
  it("joins inspector ids", () => {
    expect(summarizeContentInspectors([{ id: "url-allowlist" }, { id: "pii-scan" }])).toBe(
      "url-allowlist, pii-scan",
    );
  });
});

describe("summarizeAllowedMCPServers", () => {
  it("distinguishes no-constraint from deny-all", () => {
    expect(summarizeAllowedMCPServers(undefined)).toBe("no constraint");
    expect(summarizeAllowedMCPServers([])).toBe("deny-all");
  });
  it("joins server names", () => {
    expect(summarizeAllowedMCPServers([{ name: "github" }, { name: "jira" }])).toBe("github, jira");
  });
});
