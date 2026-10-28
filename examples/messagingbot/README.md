<!-- examples/messagingbot/README.md -->

# messagingbot

A senior brand-designer agent, packaged as a `.oap` agent container. Give it a
brief and it scopes the work, reads your design system and messaging from
GitHub (read-only), and produces an on-brand **HTML** artifact: a landing
section, an announcement, a one-pager, an email-ready block. Open the artifact
in the live viewer, pin notes to the elements that are wrong, send them, and it
revises the same artifact in place.

It is the open-source version of the brand agent we run internally, and it is
built entirely from primitives OAP already has — the artifact system, the
viewer's annotation interaction, agent skills, and a read-only toolspec. See
the [case study](https://openap.org/stories/claude-design) for the
walkthrough.

No channel is baked in. A Channel is per-install config, not part of the
container; attach one after installing (see below).

## Bundle layout

```
messagingbot/
  oap.yaml              # manifest: the PAT, brand repos, and skill-menu questions
  manifests/
    agentclass.yaml       # AgentClass messagingbot (prompt, skill menu, config plane, capabilities)
    agentidentity.yaml    # AgentIdentity messagingbot-id (injects the PAT as GITHUB_TOKEN)
    spiceboxclass.yaml    # SpiceboxClass messagingbot-gh (sandbox: gh only, GitHub-only egress)
    toolspec.yaml         # SpiceboxToolspec messagingbot-gh-read (GET-only, allowlisted repos)
    skillsources.yaml     # one SkillSource per selectable skill, pinned to a commit
  assets/logo.svg
  README.md
```

The Secret holding the PAT is **not** in the bundle. It is materialized at
install from the `githubToken` question and never travels with the container.

## Install questions

| Question      | What it sets                                     | Default                           |
| ------------- | ------------------------------------------------ | --------------------------------- |
| `githubToken` | Secret `messagingbot-github`                     | (required)                        |
| `brandRepos`  | appended to `spec.config.readableRepos`          | none: designs from skills alone   |
| `skills`      | which `spec.skills` entries stay (`[?]` filter)  | `frontend-design`, `theme-factory` |

### Selectable skills

`agentclass.yaml` declares the whole menu in `spec.skills`. The `skills`
question binds to `AgentClass/messagingbot#spec.skills[?]`, a **filter** marker:
the list is narrowed to the entries whose `name` you pick, in the order the
AgentClass declares them. Picking a name that isn't on the menu fails the
install instead of silently dropping it, and re-installing with the same
answer is a byte-identical no-op.

| Skill             | Source                                         | Why                                                                         |
| ----------------- | ---------------------------------------------- | --------------------------------------------------------------------------- |
| `frontend-design` | `github.com/anthropics/skills//skills/frontend-design` | A deliberate visual direction instead of templated defaults.        |
| `theme-factory`   | `github.com/anthropics/skills//skills/theme-factory`   | Ten preset palettes and font pairings, or a generated theme, when there is no brand system. |

To add your own skill, such as your company's brand-voice skill, add a
SkillSource for its repository and an entry to `spec.skills` with its
canonical name, then add the name to the question's `enum`.

Both sources are pinned to a commit SHA. A skill is instructions the model
follows, so a branch ref would let those instructions change under an unchanged
install. Bump the SkillSource `ref` and the `spec.skills` refs together.

`load_skill` returns a skill's `SKILL.md` only. Supporting files (for example
theme-factory's `themes/`) are read from `anthropics/skills` with the same
read-only `gh` tool, which is why that repository ships in
`spec.config.readableRepos`.

## Read-only access: three rings

1. **The token.** Create a fine-grained PAT at
   https://github.com/settings/personal-access-tokens/new with **Contents:
   Read-only** on your brand repositories, and nothing else. No call can write,
   whatever the toolspec allows.
2. **The verbs.** `messagingbot-gh-read` allows `gh api`, `gh repo view`, and
   `gh auth status`. `gh api` is GET-only: no `-X`, `-f`/`-F`, `--input`, or
   `--hostname`, and only `repos/OWNER/NAME/...` endpoints, so every call names
   a repository.
3. **The repositories.** Every repository a call names must match
   `spec.config.readableRepos`: `anthropics/skills` plus whatever you answered
   for `brandRepos`. Entries are lowercase `owner/repo` or `owner/*`.

## Prerequisites

- A cluster with OAP installed (`oap install`) and a **cluster default model**
  configured (`oap init` / `oap settings wizard`). This agent pins no model.
- The `oap` CLI on your PATH.
- The read-only PAT described above.

## Install

```bash
oap agent lint    examples/messagingbot
oap agent install examples/messagingbot --namespace default
```

Non-interactively:

```bash
cat > /tmp/messagingbot-values.yaml <<'EOF'
githubToken: github_pat_...
brandRepos: [acme/design, acme/messaging]
skills: [frontend-design]
EOF
oap agent install examples/messagingbot --namespace default --values /tmp/messagingbot-values.yaml
```

Install isolated copies with `--name <prefix>`: every CR and the Secret are
prefixed together.

## Attach a channel

```bash
oap channel create --kind slack
```

Pick `messagingbot` when asked which AgentClass to bind. Slack posts the HTML
artifact with a button that opens it in the live viewer, where the annotations
happen. The browser chat works too.

## Try it

> Design a landing-page hero announcing our autopilot feature for warehouse
> ops leads. Goal: book a demo. Use acme/design and acme/messaging.

1. If the brief is thin, it asks 2–4 clarifying questions first.
2. It posts a plan, loads its skills, and reads your repos.
3. It returns an HTML artifact and offers the live view.
4. In the viewer, annotate the elements you want changed and send the batch.
   It revises the same artifact, and the revision history keeps every version.

## Notes

- **HTML + CSS only.** JavaScript, remote stylesheets, iframes, and remote SVG
  are stripped on render. `data:` images and inline `<style>` are allowed.
- **`authz.toolCalls.mode` is `permissive`**, the bootstrap default for every
  example. Flip it to `enforcing` once SpiceDB grants are populated.
