# Pirate Captain — an agent that delegates to a private OAP

This example contains two complete OAPs: `pirate-captain`, which translates
into pirate dialect, and `vampire-speak`, the only class it is permitted to
delegate to. The pirate explicitly embeds the vampire as a private dependency,
so installing the captain automatically installs both AgentClasses and rewrites
the captain's roster to the private class name
`pirate-captain-vampire-speak`.

The vampire remains a normal OAP in its own nested folder. You can lint,
package, or install it independently when you want the translator without the
captain.

## Install

```bash
./bin/oap agent install examples/pirate-subagent/pirate-captain
./bin/oap agent chat pirate-captain
```

No raw `kubectl apply` is required. Neither class names a model, so both inherit
the cluster default — the `ClusterAgentSettings` model-catalog entry marked
`default: true` and the token it already references, which `oap init` sets up.
A cluster that can run any agent can run these.

To validate or install only the child OAP:

```bash
./bin/oap agent lint examples/pirate-subagent/pirate-captain/dependencies/vampire-speak
./bin/oap agent install examples/pirate-subagent/pirate-captain/dependencies/vampire-speak
```

## Try delegation

Ask the captain:

> The treasure is buried under the old oak tree. Give me your pirate version,
> and also ask the vampire for its version.

You should receive both dialects. OAP creates a second AgentSession for the
private vampire class and an agent Channel for the task-mode conversation, then
returns the child's result to the captain.

## How you know delegation actually happened

The two dialects are **mechanically distinguishable**, because an example that
cannot fail proves nothing. The vampire's substitutions are literal, and the
captain's prompt forbids it from attempting them:

- `w` → `v` at the start of a word — "vant", "ve", "vith", "vill"
- `th` → `z` — "ze", "zis", "zere"
- a closing "One... two... ah-ah-ah!"

So a **"vant"** or a **"ze"** in the answer was produced by the private child
AgentSession and carried back over the agent Channel. It could not have come
from the captain.

The negative case reads too: on a failed delegation the captain says so plainly
and gives only its pirate version. **No vampire markers means no delegation** —
you are never left guessing whether it quietly did the work itself.

## The three opt-ins

An AgentClass with none of these cannot delegate to anything, which is the
default. `vampire-speak` has none of them, so the tree stops there.

| Field | What it grants | Default |
| --- | --- | --- |
| `spec.capabilities.subagents` | the `delegate` tool itself | absent — no tool |
| `spec.subagents` | the closed set of classes it may delegate TO | absent — nobody |
| `spec.subagentModes` | per-member ceiling on how conversational the child may be | absent — `single_turn` only |

## The modes

Mode is an **attack-surface declaration**, not a preference.

| Mode | The child can | Human-facing exchanges |
| --- | --- | --- |
| `single_turn` | one bounded task, then return | none — it is headless |
| `task` | ask its parent ONE clarifying question | one |
| `chat` | hold an open back-and-forth | unbounded |

The delegating agent **asks** for a mode; the roster is the **ceiling**. It may
choose narrower than granted, never wider.

### Seeing the refusal

The refusal is the half worth watching, because it is easy to get wrong and
hard to notice. Edit
`pirate-captain/manifests/agentclass.yaml` to drop `task`:

```yaml
  subagentModes:
    vampire-speak:
      - single_turn
```

Reinstall the pirate OAP and repeat the request. The same delegation now fails
with a reason naming the mode it asked for and the modes it was allowed.
Nothing is silently narrowed behind the agent's back.
