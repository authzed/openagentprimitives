package meta

import (
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// recallError renders a memory / knowledge-graph failure for the three recall
// meta tools, deciding Result.Trusted from the error's PROVENANCE.
//
// Recall results bypass the PostToolCall pipeline (meta tools are ungated), so
// Trusted is what decides whether the runner inspects the text. Two very
// different things share this one return:
//
//   - A memory SENTINEL, authored entirely inside this repo: a corrective hint
//     ("did you mean %q?"), a fixed authorization refusal, a fixed deployment
//     fact. A content guard must never withhold one — the model would see
//     "content withheld", never learn what it got wrong, and repeat the same bad
//     query — so these keep Trusted. Every sentinel is permanent (4xx, treated
//     as final), so no retry will produce a better message either.
//
//   - A PROVIDER / transport error. The graphiti providers interpolate up to
//     1KiB of the upstream HTTP response body, so an upstream-controlled string
//     reaches the model through it. These drop Trusted and get inspected.
//
// A new memory sentinel belongs in platformAuthored below, next to
// httpsrv.sentinelStatus's arm for it.
func recallError(toolName string, err error) tool.Result {
	return tool.Result{
		Content: fmt.Sprintf("%s: %v", toolName, err),
		IsError: true,
		Trusted: platformAuthored(err),
	}
}

// platformAuthored reports whether err is a memory sentinel whose message this
// codebase writes end to end.
//
// It matches on both paths the runner can take: in-process (memory.Local) and
// over HTTP, where pkg/memory/sentinel's shared table lets httpclient rebuild
// the sentinel from a discriminator only httpsrv sets. A sentinel's message
// composes fixed platform text with request-derived values (the caller's own
// field path, its own entity id), so it is safe to mark Trusted; the errors
// that interpolate an upstream response body are not sentinels, map to 500, and
// arrive untrusted.
func platformAuthored(err error) bool {
	return errors.Is(err, memory.ErrInvalidQuery) ||
		errors.Is(err, memory.ErrMissingApproval) ||
		errors.Is(err, memory.ErrNoSearchProviders) ||
		errors.Is(err, memory.ErrKGScopeMismatch) ||
		errors.Is(err, memory.ErrKGUnsupported)
}
