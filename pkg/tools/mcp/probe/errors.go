package probe

import "fmt"

// HTTPError is returned by ListTools when the MCP server responds with a
// non-2xx status. Callers can use errors.As to distinguish auth failures
// (401/403) from other server errors when reporting controller conditions.
type HTTPError struct {
	StatusCode int
	// Body is the response body, truncated to 512 bytes (with a
	// "...(truncated)" marker when truncation occurred).
	Body string
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("probe: HTTP %d: %s", e.StatusCode, e.Body)
}

// IsAuth reports whether the status indicates an authentication or
// authorization failure (401 Unauthorized or 403 Forbidden).
func (e *HTTPError) IsAuth() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}
