// Package provider defines the /providers/ library shape and the
// compile-time embedded catalog of builtin providers.
package provider

// Provider mirrors the YAML shape under /providers/<id>.yaml. Every field's
// load-time rules live in validate.go; the security-relevant ones are documented
// on the fields themselves.
type Provider struct {
	// Catalog-unique id; the filename stem under /providers/ and the key every
	// credential requirement names.
	ID string `yaml:"id" json:"id"`

	// Title is the short, user-facing display name for this credential's
	// service ("GitHub", "Claude"). Shown as the bold header of a
	// credential-request row. Falls back to a humanized credential name
	// when empty.
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
	// One-sentence explanation of what this credential is for, shown under Title.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Link to the provider's own credential-creation docs, offered alongside Instructions.
	DocsURL string `yaml:"docsURL,omitempty" json:"docsURL,omitempty"`

	// Shape is one of: bearer | oauth | kubeconfig. Describes what the
	// user has to do to obtain the credential, not how it's stored.
	Shape string `yaml:"shape" json:"shape"`

	// Builtin is the registry name of a Go flow that handles this
	// provider. Empty → LLM-fallback only.
	Builtin string `yaml:"builtin,omitempty" json:"builtin,omitempty"`

	// Prompt is system-prompt context for the LLM fallback (when no
	// builtin) and a human-readable explanation shown by the builtin
	// before kicking off.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`

	// Instructions is user-facing, multi-line guidance shown in EVERY
	// enter-token UI (the TTY setup flow and the identityd paste-forms):
	// how to obtain this credential. Unlike Prompt (LLM-only), this text
	// is displayed verbatim to the human.
	Instructions string `yaml:"instructions,omitempty" json:"instructions,omitempty"`

	// OAuth is populated only when Shape == "oauth".
	OAuth *OAuthConfig `yaml:"oauth,omitempty" json:"oauth,omitempty"`

	// RunShellAllowlist enumerates regex patterns the LLM fallback may
	// pass to run_shell. Empty → run_shell unavailable for this provider.
	RunShellAllowlist []ShellAllow `yaml:"runShellAllowlist,omitempty" json:"runShellAllowlist,omitempty"`

	// TokenShape is the optional declared token format for this provider —
	// an anchored Go regexp (Pattern) plus a human-readable hint
	// (Description). It is the user-visible source of truth, compile-
	// validated at load (loader.go) and enforced at EVERY point of entry via
	// ValidateToken: the identityd paste forms, the `oap user-identity
	// put-token` CLI, and the builtin setup flows. A nil TokenShape (or empty
	// Pattern) means "no declared format" — validation stays permissive and
	// never blocks the credential.
	TokenShape *TokenShape `yaml:"tokenShape,omitempty" json:"tokenShape,omitempty"`

	// Verify declares an optional live-verification probe for this
	// provider: an authenticated HTTP request whose response tells us
	// whether a just-entered token is actually accepted by the provider.
	// Executed at every entry point AFTER the TokenShape format gate.
	// A nil Verify means "no live check available" — verification reports
	// indeterminate and never blocks the credential.
	Verify *VerifyConfig `yaml:"verify,omitempty" json:"verify,omitempty"`

	// AuthFailure declares what an AUTHENTICATION failure looks like for this
	// provider, so the platform can independently corroborate an agent's claim
	// that a credential has stopped working. It is deliberately separate from
	// Verify: Verify asks the provider directly, AuthFailure recognizes the
	// failure a tool already hit.
	//
	// HTTPStatuses applies to MCP origins (HTTP transport); ExitCodes and
	// StderrPatterns apply to CLI toolkits, whose only failure surface is
	// ToolCall.status.exitCode plus a stderr artifact.
	//
	// A nil AuthFailure means "no corroboration available for this provider" —
	// requests then rely solely on a definitive Verify verdict. That is a safe
	// default, not a broken one.
	AuthFailure *AuthFailure `yaml:"authFailure,omitempty" json:"authFailure,omitempty"`
}

type OAuthConfig struct {
	// AuthorizationServer: "discovery" → RFC 9728/8414 discovery, or a literal URL.
	AuthorizationServer string `yaml:"authorizationServer" json:"authorizationServer"`
	// Space-separated scopes requested when the caller names none; empty leaves
	// the scope unset and takes whatever the provider grants by default.
	DefaultScope string `yaml:"defaultScope,omitempty" json:"defaultScope,omitempty"`
	// True when the provider supports RFC 7591 dynamic registration, so no
	// pre-registered client_id has to be configured.
	DynamicClientRegistration bool `yaml:"dynamicClientRegistration,omitempty" json:"dynamicClientRegistration,omitempty"`
}

type ShellAllow struct {
	// Go regexp a run_shell command must match to be permitted; it is the whole
	// authorization for that command, so keep it as narrow as the task allows.
	Regex string `yaml:"regex" json:"regex"`
	// Why this command is needed, shown to the human approving the flow.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type TokenShape struct {
	// Anchored Go regexp a pasted token must match; empty means "no declared
	// format" and every value is accepted.
	Pattern string `yaml:"pattern" json:"pattern"`
	// Human-readable hint shown when Pattern rejects a pasted value ("starts with ghp_").
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// VerifyConfig describes a declarative live-verification probe for a
// bearer-shaped provider: GET (or HEAD/POST) Endpoint with the token in
// the Authorization header; a success status proves the token is live, a
// 401 proves it is rejected, anything else — including a 403, which says
// the REQUEST was refused rather than that the credential died — is
// indeterminate. Same 401-only asymmetry AuthFailure.HTTPStatuses draws,
// and for the same reason.
type VerifyConfig struct {
	// Endpoint is the absolute https URL to probe (e.g.
	// https://api.github.com/user). Catalog-supplied only — never from
	// user input.
	Endpoint string `yaml:"endpoint" json:"endpoint"`

	// Method defaults to GET. Only GET, HEAD, POST are allowed.
	Method string `yaml:"method,omitempty" json:"method,omitempty"`

	// AuthScheme is the Authorization header scheme: "token" or "Bearer"
	// (default "Bearer").
	AuthScheme string `yaml:"authScheme,omitempty" json:"authScheme,omitempty"`

	// SuccessStatuses are the HTTP statuses that prove the token is live.
	// Defaults to [200].
	SuccessStatuses []int `yaml:"successStatuses,omitempty" json:"successStatuses,omitempty"`

	// SubjectField optionally names a top-level string field in the JSON
	// response body to surface as the verified identity (e.g. "login" →
	// "authenticated as @octocat").
	SubjectField string `yaml:"subjectField,omitempty" json:"subjectField,omitempty"`

	// SubjectIDField optionally names a top-level field in the JSON probe
	// response holding the provider's STABLE identifier for the authenticated
	// principal, which is frequently not the field a human should read.
	//
	// GitHub is the motivating case: `login` is what a person recognizes and is
	// also mutable — a renamed login is released and can be claimed by another
	// account, so an authorization edge keyed on it is a privilege transfer
	// waiting to happen. `id` never moves.
	//
	// Unlike SubjectField this accepts a JSON number as well as a string,
	// because stable ids are commonly numeric.
	SubjectIDField string `yaml:"subjectIDField,omitempty" json:"subjectIDField,omitempty"`
}

// AuthFailure recognizes an authentication failure for one provider.
type AuthFailure struct {
	// HTTPStatuses are the response statuses meaning "this credential was
	// rejected". Empty (with the block present) DEFAULTS TO 401 ONLY.
	//
	// Declaring 403 WEAKENS the gate and must be deliberate: an LLM cannot forge
	// a 401, but it CAN provoke a 403 by requesting resources it is not entitled
	// to, manufacturing corroboration to pair with an indeterminate probe. Add it
	// only for a provider that returns 403 for credential rejection rather than
	// for authorization.
	//
	// LIMIT OF THE ASYMMETRY. "Only the provider's auth layer emits a 401" means
	// "only the MCP ENDPOINT's HTTP layer emits it", and holds only for an
	// endpoint that behaves like a compliant MCP server (transport statuses for
	// transport outcomes; 200 plus a tool-level isError for anything the tool
	// refused). It is load-bearing on both sides — a 200 is also what
	// tool.Result.OriginAuthenticated reads as proof the credential works.
	//
	// True for a first-party MCPServer; an assumption about USER-SUPPLIED CODE
	// for a SidecarToolbox, which is also a corroborable origin. A sidecar may
	// forward an upstream 401 verbatim (corroborating a credential the platform
	// never presented) or answer 200 for a call its upstream rejected (retracting
	// a real observation). Both are bounded — the operator chose to run it, and
	// neither reaches past this session's status — but neither is the guarantee a
	// real provider endpoint gives.
	HTTPStatuses []int `yaml:"httpStatuses,omitempty" json:"httpStatuses,omitempty"`

	// ExitCodes are CLI exit codes meaning "credential rejected".
	//
	// AGENT-SHAPED, exactly as StderrPatterns is: a sandbox tool's `args` is the
	// same freeform []string the model fills in, and an exit code is a function
	// of those arguments as much as of the credential.
	//
	// The concrete hazard is exit code 1: most CLIs exit 1 on ANY error, so
	// `exitCodes: [1]` corroborates nearly every failed call at that origin — the
	// exit-code equivalent of a `.*` stderr pattern. Load-time validation does
	// NOT reject 1 (a few CLIs do use it for auth rejection alone, and refusing
	// it would push authors onto the weaker stderr signal), so it is on the
	// catalog author and reviewer to establish that this CLI's 1 means
	// "credential rejected" rather than "something went wrong". Prefer a distinct
	// provider-specific code; pair a broad code with a stderr pattern.
	//
	// Exit code 0 is rejected at load: it means the process SUCCEEDED, so
	// declaring it inverts the signal rather than merely widening it.
	//
	// The bar is HIGHER for a provider with no verify: probe — corroboration is
	// DECISIVE there (credupdate.Determine's TierUnverified), so an over-broad
	// code turns an agent's unverified claim into a credential-entry form a human
	// is asked to fill in.
	ExitCodes []int `yaml:"exitCodes,omitempty" json:"exitCodes,omitempty"`

	// StderrPatterns are Go regexps matched against a CLI tool's stderr,
	// compile-validated at load. THE WEAKEST SIGNAL HERE — read before adding one.
	//
	// HTTPStatuses is trustworthy because of an asymmetry: a 401 can only come
	// from the provider's auth layer rejecting a credential it validated. Stderr
	// has NO such asymmetry. It is ordinary program output and the agent chooses
	// the argv producing it, so any text a CLI echoes back into its own error
	// output is in effect agent-authored.
	//
	// A pattern must therefore match text ONLY the provider's auth layer emits,
	// and never a substring an argv can be echoed into. `(?i)authentication
	// failed` is provokable on any CLI that quotes your arguments back at you; a
	// pattern tied to the tool's own auth-error preamble is not.
	//
	// The bar is HIGHER, not lower, for a provider with no verify: probe.
	// Corroboration is consulted precisely when a live re-probe was
	// indeterminate, so there a stderr match is the DECISIVE evidence promoting
	// an agent's unverified claim into a credential-entry form
	// (credupdate.Determine's TierUnverified). A provokable pattern there is a
	// phishing primitive: the agent manufactures its own corroboration and the
	// platform asks the user to re-enter a working credential.
	//
	// Anchoring is deliberately NOT enforced (see validateAuthFailureConfig);
	// load-time validation rejects only a pattern matching the EMPTY string.
	// Provokability depends on the CLI's behavior, which this catalog cannot
	// inspect, so the rest is a human-review property.
	StderrPatterns []string `yaml:"stderrPatterns,omitempty" json:"stderrPatterns,omitempty"`
}
