package scope

// ColdStartExtraction is the two-part product of the cold-start extractor:
// the permission-scope changes parsed from the user's first message, and
// the cleaned task (that message with scope-setting language removed) that
// becomes the primary agent's first user turn.
//
// CleanedTask is liquid output derived from user text and so is produced by
// the extractor (which already ingests user text), NOT the composer (which
// stays text-free to preserve the prompt-injection boundary). It is shown
// verbatim to the approver before the agent runs it.
type ColdStartExtraction struct {
	// ScopeDelta is the permission change parsed out of the message; may be empty.
	ScopeDelta ScopeDelta `json:"scopeDelta"`
	// CleanedTask is the message with scope-setting language stripped; empty means nothing to run.
	CleanedTask string `json:"cleanedTask"`
}
