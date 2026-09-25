package agentstatus

import (
	"strings"

	"github.com/stoewer/go-strcase"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// gatePhrase is the user-facing wording for one not-ready gate reason.
// phrase is lowercase with no trailing punctuation; progress marks ordinary
// startup work (rendered "Doing the thing…") as opposed to a genuine blocker
// (rendered "Can't start yet — …").
type gatePhrase struct {
	phrase   string
	progress bool
}

// gatePhrases maps the known not-ready gate reasons (the ones controllers set
// False on the notReadyGates conditions) to their user-facing phrasing. The
// reasons are set as string literals at their condition-write sites, so there
// is no registry to derive this from; a reason missing here falls back to a
// mechanical CamelCase split — readable, never a raw machine token — so a new
// controller reason degrades gracefully instead of leaking.
var gatePhrases = map[string]gatePhrase{
	// Ordinary startup progress: nothing is wrong, say what is happening.
	"BundlesProvisioning": {phrase: "preparing tools and skills", progress: true},
	"RunnerCreating":      {phrase: "starting the agent", progress: true},
	"AgentClassNotValid":  {phrase: "waiting for agent configuration", progress: true},
	spiceboxv1alpha1.ReasonAgentSessionAwaitingDetector: {phrase: "starting safety checks", progress: true},

	// Genuine blockers: the session cannot start until something changes.
	spiceboxv1alpha1.ReasonSandboxUnschedulable:        {phrase: "waiting for cluster capacity"},
	spiceboxv1alpha1.ReasonRunnerPodRefused:            {phrase: "the agent was not allowed the resources it needs to start"},
	spiceboxv1alpha1.ReasonAgentSessionImagePinDrifted: {phrase: "a pinned image changed and is blocked"},
	"AgentClassMissing":                                {phrase: "the agent's configuration is missing"},
	"MissingStarterSubject":                            {phrase: "the session has no starting user"},
	"FederatedIdPSecretMissing":                        {phrase: "an identity credential is missing"},
	"CredentialLinkTimeout":                            {phrase: "credential linking timed out"},
	"AwaitingUserCredentials":                          {phrase: "waiting for credentials to be linked"},
	"ModelMissingModel":                                {phrase: "no model is configured"},
	"ModelMissingCredential":                           {phrase: "the model has no credential configured"},
	"ModelForbidden":                                   {phrase: "the model is not allowed by settings"},
	"ModelOverrideNotAllowed":                          {phrase: "the agent's model override is not allowed"},
	"ModelNotInCatalog":                                {phrase: "the model is not in the model catalog"},
	"ToolkitNotAllowed":                                {phrase: "a toolkit is not allowed by settings"},
	"MCPServerNotAllowed":                              {phrase: "an MCP server is not allowed by settings"},
	"MCPToolNotAllowed":                                {phrase: "an MCP tool is not allowed by settings"},
	"SkillNotAllowed":                                  {phrase: "a skill is not allowed by settings"},
	"SkillDenied":                                      {phrase: "a skill is denied by settings"},
	"SandboxKindNotPermitted":                          {phrase: "the sandbox kind is not permitted by settings"},
	"SkillPinningRequired":                             {phrase: "a skill must be pinned before use"},
	"PinningRequired":                                  {phrase: "an image must be pinned before use"},
	"SkillBundleDigestMismatch":                        {phrase: "a skill bundle failed its integrity check"},
}

// FriendlyGate renders a not-ready gate reason/message as a LIVE startup
// caption: text for wide surfaces (webchat caption, Slack thread-top), short
// for narrow ones (Slack's loading_messages — the caller fits it to the
// surface's own limit). Progress-shaped reasons read as what is happening and
// deliberately drop the condition message, which is written for operators
// ("waiting for bundle SpiceboxSessions to become Ready") and has no user
// action in it; blocker-shaped reasons keep the message, which carries the
// actionable detail (the scheduler's "Insufficient memory", the denied model
// name).
func FriendlyGate(reason, message string) (text, short string) {
	p := phraseFor(reason)
	if p.progress {
		capt := capitalizeFirst(p.phrase) + "…"
		return capt, capt
	}
	text = "Can't start yet — " + p.phrase
	if message != "" {
		text += ": " + message
	}
	return text, "Can't start — " + p.phrase
}

// FriendlyGateDetail renders the same phrasing as a diagnostic detail
// fragment ("<phrase>: <message>") for terminal notices that wrap it in their
// own sentence (webd's "the agent session couldn't start (…)"). Unlike
// FriendlyGate it always keeps the message: the terminal notice is the last
// thing the user sees and points at the admin console, so detail wins.
func FriendlyGateDetail(reason, message string) string {
	p := phraseFor(reason)
	if message == "" {
		return p.phrase
	}
	return p.phrase + ": " + message
}

// phraseFor resolves a reason to its phrasing, falling back to a mechanical
// CamelCase split for reasons the map does not know. Unknown reasons are
// treated as blockers: the session genuinely has not started, and "can't
// start yet" is the honest default when we cannot say the state is routine.
func phraseFor(reason string) gatePhrase {
	if p, ok := gatePhrases[reason]; ok {
		return p
	}
	phrase := strings.ReplaceAll(strcase.SnakeCase(reason), "_", " ")
	if phrase == "" {
		phrase = "not ready"
	}
	return gatePhrase{phrase: phrase}
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
