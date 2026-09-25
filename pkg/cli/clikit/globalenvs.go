package clikit

// Canonical names of environment variables read by more than one
// agentprimitives binary. Defined once here so every binary binds the same
// flag to the same env var — drift in these names is exactly how "this pod
// silently used a different NATS / memory URL" footguns happen. Binary-local
// env vars (read by a single binary) stay as string literals in that binary's
// main; they don't need a shared home.
//
// SpiceDB env-var names are NOT redefined here — they already live in
// pkg/authz/spicedb (EnvEndpoint, EnvToken, EnvInsecure, EnvTokenPath) and are
// reused directly at the binding sites.
const (
	// EnvNATSURL is the NATS connection URL. Read by channelsd, webd, authzd,
	// and the runner; the operator passes its own --nats-url through to the
	// runner pods it creates.
	EnvNATSURL = "NATS_URL"

	// EnvOperatorMemoryURL is the operator's memory/artifact base URL. Read by
	// channelsd, webd, and the runner.
	//
	// NOTE: authzd reads MEMORY_URL — a distinct, older name kept for
	// back-compat — so authzd binds its flag to "MEMORY_URL", NOT to this
	// constant. Do not conflate the two.
	EnvOperatorMemoryURL = "OPERATOR_MEMORY_URL"

	// EnvMetaagentSlackBotUserID is the Slack bot user id used to suppress the
	// bot's own messages from re-triggering the metaagent. Read by channelsd
	// and authzd.
	EnvMetaagentSlackBotUserID = "METAAGENT_SLACK_BOT_USER_ID"
)
