package oauth_mcp

import "github.com/authzed/openagentprimitives/pkg/cli/tui"

// ClientGuidanceForTest exposes clientGuidance to the package's external test
// so it can be measured against the note's column budget.
//
// It lives in export_test.go rather than beside clientGuidance so that the
// production file carries no test-only export, and so godoc attributes the
// guidance block's rationale to the function that composes it.
func ClientGuidanceForTest(st *tui.State) string { return clientGuidance(st) }

// RedirectHostForTest exposes redirectHost so the announced callback address
// can be tested against the environment override that changes it.
func RedirectHostForTest() (string, error) { return redirectHost() }
