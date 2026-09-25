// pkg/channels/channelkinds/slack/appprovision/errors.go
//
// What Slack refused with, in a shape the wizard can act on.
package appprovision

import "fmt"

// APIError is a Slack `{"ok": false, "error": "..."}` refusal.
//
// The code is kept rather than flattened into a sentence because the caller
// decides different things from different codes: an approval state means the
// run falls back to creating the app by hand, while invalid_manifest means our
// own generation is wrong and no fallback will help.
type APIError struct {
	// Method is the Slack API method that refused, so an error read in a log
	// says which of the two calls failed without the caller wrapping it.
	Method string
	Code   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("slack %s refused: %s", e.Method, e.Code)
}

// approvalCodes are the refusals that mean a workspace administrator, not the
// caller, decides whether this app may be installed. Enterprise Grid always
// has this on; a standalone workspace can turn it on.
var approvalCodes = map[string]bool{
	"app_approval_request_eligible": true,
	"app_approval_request_pending":  true,
	"app_approval_request_denied":   true,
}

// IsApprovalRequired reports whether this refusal is an admin decision rather
// than a defect. Nil-receiver safe so a caller can ask without first proving
// the error was an *APIError.
func (e *APIError) IsApprovalRequired() bool {
	return e != nil && approvalCodes[e.Code]
}
