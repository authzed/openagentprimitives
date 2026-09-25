# `pkg/web/uicomponents`

The **closed, platform-owned vocabulary** an agent-defined UI may be written in,
plus the parser, the validator, and the view-model resolution.

A view model is LLM output. What makes it safe is validation against a schema
the *platform* controls: the model can only select from this set and bind to
sources it was already authorized for. **It can never introduce script, widen a
CSP, or open an egress path.**

A component's Props struct is **both the schema and the validator** — `Validate`
unmarshals into a fresh copy with `DisallowUnknownFields`, and `Schema` emits
the JSON Schema handed to the agent. Keeping them one artifact means they cannot
drift.

## Layout

| File | What it holds |
| ---- | ------------- |
| `component.go` | The package doc and the two aliases (`Component`, `ParamValue`) |
| `components.go` | The v1 vocabulary: the Props structs and the single `init()` registration loop |
| `declaration.go` | Wire types `Declaration`/`Action`/`Slot`/`Node`/`Binding`, `ParseDeclaration`, `ParamRefKey` |
| `prompt.go` | The `{key}` placeholder grammar for a prompt action |
| `schema.go` | `VocabularySchema()` — the whole vocabulary as JSON Schema, for the agent |
| `validate.go` | `Validate` and its per-concern helpers |
| `viewmodel.go` | Tier-1: `ParseNode`, `Fragment`, `Rejection`, `View`, `ResolveView` |
| `walk.go` | `BindingPath`, `WalkBindings`, `ParamNames`/`ParamKeys`, `ParamStates`, `ActionRefs` |
| [`component/`](component) | The leaf `Component` type — a separate package purely to break an import cycle with `registry` |
| [`registry/`](registry) | The vocabulary registry, over `pkg/x/kindregistry` |

**`Component` must never grow a method needing `Node` or `Declaration`** — that
is what would reintroduce the cycle the leaf package exists to avoid.

## Tier 0 and Tier 1 are the same language

`Declaration` carries both. They differ only in **who wrote it and when**: a
human/bundle author writes the CR, the agent writes per-slot fragments at
runtime. A Tier-1 fragment rewrites a *slot*, which is why an agent can **name**
an existing action but can never mint one.

**An action is top-level, never on a node.** Three consequences follow from the
shape of the document rather than from a check: the browser sends only a *name*;
an action can never appear in `Node.Bindings`, so **writes are never a data
binding**; and exactly one of `Action.Tool` and `Action.Prompt` is set. A prompt
action reaches no tool, so **no grant is checked** — it is bounded instead by
the fact that its message is one the viewer could have typed into the transcript
themselves, on the same route with the same authorization and attribution.

## Fail-closed by mechanism, not by intent

`Options.GrantedTools` and `ReadonlyTools` are maps, and **indexing a nil Go map
returns false**.

> Do **not** add a `!= nil` guard — that branch needs its own deny arm to stay
> closed, and a forgotten one flips the default to allow. Do not swap either map
> for a slice + `contains()` either.

The two are asymmetric: `validateActions` consults `GrantedTools` only (an
action exists to mutate), while `validateBindings` requires **both**. The test
fixture deliberately keeps `ReadonlyTools` narrower so a copy-paste of the
data-binding check turns red.

**This whole layer is defense in depth, not the gate.** The unconditional gate
is in the runner, re-evaluated against the live tool on every call.

## Things that surprise people

- **Enum tags tell, they do not enforce.** `jsonschema:"enum=…"`, `minimum=`,
  `maximum=` land in the agent-facing schema, but `validateProps` does a *type*
  check, not a *value* check — names enforced, values not. If a value ever
  becomes security-relevant, it needs a real check.
- **Required-ness is advisory only.** The reflector marks several fields
  required and `validateProps` enforces none of it.
- **`validateProps` needs its explicit exact-name check.** `encoding/json`
  matches case-insensitively, so a decode alone would accept `"Body"` or
  `"BODY"`. Do not remove it believing `DisallowUnknownFields` covers it — it
  does not.
- **Parse through `ParseDeclaration` / `ParseNode`, not `json.Unmarshal`.** Both
  use `DisallowUnknownFields`. The accepted tradeoff is that a bundle authored
  against a newer platform is rejected; fail-closed is deliberate.
- **`Validate` returns the first failure**, not all of them — the agent corrects
  one thing at a time.
- **`ResolveView` re-validates on every read**, not just at write, because both
  the Tier-0 declaration and the tool grant move underneath a stored fragment: a
  redeploy can remove a slot or narrow `GrantedTools`. Fallback is per slot and
  deterministic.
- **`View.AgentComposed` is computed, never stored**, so no agent-written record
  can assert or clear its own marker. **`View.Rejected` must be logged** — a
  dropped fragment renders as "the UI didn't update", which reads as a slow
  agent rather than a bug.
- **`ParamNames` ≠ `ParamKeys`.** A range control declaring `"window"` drives
  `"window.from"` and `"window.to"` and never `"window"`. Validation uses
  `ParamKeys`; `ParamNames` would be wrong in both directions.
- **`Action.Inputs` must be disjoint from `ParamKeys`**, because the handler
  merges filtered params and filtered inputs into one values map — "which map
  wins" has no good answer.
- **Determinism in `WalkBindings` is load-bearing**, not tidiness: props are
  sorted because `Node.Bindings` is a Go map.
- **`MaxDepth`/`MaxNodes` bound the tree, not the data.** One table with 50,000
  rows is a single node, validates clean, and renders every row.

## Two invariants that span Go and TypeScript

**1. The vocabulary must mirror the browser's renderer map.** The Go side is the
`init()` registration; the TypeScript side is a `COMPONENTS` map (never a
switch) in `web/packages/agentui`. The tripwire is a test on the TS side holding
the same list, which asserts both directions — a type registered server-side
with no renderer, and a renderer for a type the server does not know. Without
it, a missing renderer reaches the browser as "Unknown component", which reads
to a user as the agent having done nothing.

Note this one is **three duplicated literals** (Go registration, Go test, TS
test), not a shared artifact — a coordinated sweep of all three would pass.

**2. `BindingPath` must mirror its TypeScript twin — and this one is properly
pinned**, by a **single shared golden file** that both suites read. A per-language
literal would not count:

> a coordinated change sweeping both leaves both suites green while every
> response key misses the browser's lookup, every bound prop falls back to its
> placeholder, and the page stays that way forever on a clean 200.

There is also a deliberately **unpinned** mirror: `ParamStates`, whose browser
twin seeds its parameter map by the same rule. Whoever wires that consumer must
add a pin rather than trust two independently-written literals.

## One drift risk to know about

`validate.go` hand-maintains a closed `bindingSources` slice that **tracks** the
resolvers registered in [`uibindings`](../uibindings) — but nothing asserts the
two agree. Adding a resolver without adding it here makes every binding to it
fail validation.
