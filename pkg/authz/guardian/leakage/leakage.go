// Package leakage holds types shared between the runner (which implements the
// information-leakage gate) and the meta respond_to_user tool (which calls the
// gate and must react differently depending on the outcome). It carries no
// logic of its own — keeping it a leaf package avoids an import cycle between
// pkg/agent/runner and pkg/agent/tool/meta.
package leakage

import "errors"

// ErrShareDenied signals that an information-leakage SHARE was denied: the
// approver explicitly rejected sharing the tainted data with the current
// channel audience, or that data was already denied earlier this session.
//
// The respond_to_user tool checks for this (via errors.Is) and treats it as
// terminal rather than retryable: it posts a single user-facing notice asking
// how to proceed and yields the turn to the user. Without this signal the LLM
// re-issues respond_to_user after every denial, re-publishing the "blocked"
// notice on each attempt until the turn budget is exhausted.
//
// Gate failures that are NOT denials (approval timeout, misconfiguration,
// audience-resolution errors) must NOT wrap this error: those remain
// retryable so a transient problem can recover.
var ErrShareDenied = errors.New("information-leakage: sharing denied by approver")
