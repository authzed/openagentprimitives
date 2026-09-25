package llmagent

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// BuildSystemPrompt assembles the system prompt for one setup-agent invocation,
// in order: role, provider prompt, docsURL, intent, tokenShape,
// runShellAllowlist, stop conditions.
//
// Inline-prompt path: prov is nil, promptOverride is the toolkit's inline prompt,
// and the runShellAllowlist is empty.
func BuildSystemPrompt(prov *provider.Provider, promptOverride, intent string,
	requirement authkind.CredentialRequirement) string {

	var sb strings.Builder

	sb.WriteString("You are a setup agent. Your job is to walk a human through obtaining a credential and storing it via store_credential. Be brief. Never fabricate URLs.\n\n")

	if prov != nil && prov.Prompt != "" {
		sb.WriteString("PROVIDER GUIDANCE:\n")
		sb.WriteString(strings.TrimSpace(prov.Prompt))
		sb.WriteString("\n\n")
	} else if promptOverride != "" {
		sb.WriteString("TOOLKIT GUIDANCE:\n")
		sb.WriteString(strings.TrimSpace(promptOverride))
		sb.WriteString("\n\n")
	}

	if prov != nil && prov.DocsURL != "" {
		fmt.Fprintf(&sb, "DOCS: %s (call fetch_url to read up-to-date details)\n\n", prov.DocsURL)
	}

	if intent != "" {
		sb.WriteString("USER INTENT:\n")
		sb.WriteString(strings.TrimSpace(intent))
		sb.WriteString("\n\n")
	}

	if prov != nil && prov.TokenShape != nil && prov.TokenShape.Pattern != "" {
		fmt.Fprintf(&sb, "TOKEN SHAPE: pattern=%s (%s). Validate any value the user pastes against this pattern before calling store_credential.\n\n",
			prov.TokenShape.Pattern, prov.TokenShape.Description)
	}

	if prov != nil && len(prov.RunShellAllowlist) > 0 {
		sb.WriteString("RUN_SHELL ALLOWLIST:\n")
		for _, a := range prov.RunShellAllowlist {
			fmt.Fprintf(&sb, "  - %s — %s\n", a.Regex, a.Description)
		}
		sb.WriteString("Calls outside this allowlist will fail. Each allowed call still requires user confirmation.\n\n")
	} else {
		sb.WriteString("run_shell is unavailable for this provider.\n\n")
	}

	sb.WriteString("STORE_CREDENTIAL:\nshape must be one of bearer | oauth | kubeconfig.\n")
	if requirement.IsBearer {
		sb.WriteString("This requirement is for an MCP-style bearer credential — typically shape=bearer or shape=oauth.\n")
	} else {
		sb.WriteString("This requirement projects into env vars; shape=bearer is typical (a single token).\n")
	}
	sb.WriteString("\nOAUTH CODE FLOWS: For authorization-code flows, call local_callback with oauth_exchange (token_endpoint, client_id, optional client_secret / PKCE code_verifier). The tool performs the code→token exchange and stores the credential itself — the raw authorization code is never shown to you. When the result shows stored=true, the credential is persisted; do NOT call store_credential again. Without oauth_exchange, any code parameter is redacted and discarded.\n")
	sb.WriteString("\nSTOP CONDITION: Call agent_work_complete after store_credential succeeds OR if the user declines / aborts.\n")

	return sb.String()
}
