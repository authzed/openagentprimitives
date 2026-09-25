package approval

import "github.com/authzed/openagentprimitives/pkg/memory"

const (
	SigRequested memory.SignalKind = "approval/requested"
	SigResolved  memory.SignalKind = "approval/resolved"
)
