// Package wizardkeys holds what more than one channel kind's wizard asks: the
// answer keys, the prompt text, the shared questions and the validation for
// the things several kinds pose identically — "what do we call this Channel?",
// "which AgentClass does it bind to?" and, for a kind that cannot attribute to
// a human, "which service identity does it act as?"
//
// It exists so that a question every kind must get right is spelled, asked and
// checked by ONE implementation instead of one per kind. A key lands here once
// a second kind needs it; anything specific to one transport (Slack's app
// manifest, bento's Bloblang mapping) stays in that kind's own package.
//
// THE KEYS BIND HARDER THAN THE PROMPTS. An answer key is a contract with the
// client — `--answer agentclass=…`, `--name`, and the collision check
// channelcmd runs against KeyChannelName before a wizard is even built — so a
// kind that retypes the string rather than referencing the constant silently
// stops being seeded the day the constant changes. Prompt text is only read by
// a human, so a kind whose question genuinely reads differently in its own
// flow may declare its own; see ChannelNamePrompt.
package wizardkeys

// KeyChannelName is the answer key the shared Channel-name question lands
// under. Stable: a caller seeding answers from flags for a non-interactive run
// addresses the question by it, `oap channel create --name` writes it, and the
// dispatcher's refuse-an-existing-Channel check reads it back out. EVERY kind
// asking for a Channel name must use this constant — a second spelling is not
// a cosmetic divergence but a question `--name` stops seeding while the
// collision check keeps consulting the other key.
const KeyChannelName = "name"

// ChannelNamePrompt is the question text most kinds show for it.
//
// Unlike KeyChannelName it is a DEFAULT, not an obligation: a kind whose flow
// reads better with its own wording (bento's plain "Channel name") declares
// its own, because the prompt is read only by the operator in front of it and
// nothing downstream matches on it. Reference this one unless there is a
// reason not to, so the kinds that have nothing special to say all say it the
// same way.
const ChannelNamePrompt = "Channel resource name"

// KeyExternalBaseURL is the answer key for "where is this cluster reachable
// from outside?" — the origin an external service (GitHub's webhook delivery
// today) dials back on.
//
// It lives here rather than in the one kind asking it today because it is a
// PLATFORM fact, not a transport one: the same origin webd publishes in the
// spicebox-webd-external-url ConfigMap answers it for every webhook-shaped
// kind that follows. Keeping it unexported in a kind forced anything wanting
// to pre-seed it — the .oap install planner does — to import that kind and
// branch on its name, which is the switch-on-kind this package exists to
// remove.
//
// The value is deliberately unchanged from the string github used privately:
// an answer key is a contract with the client (`--answer external-base-url=…`),
// so respelling it would silently stop seeding a question that still exists.
const KeyExternalBaseURL = "external-base-url"
