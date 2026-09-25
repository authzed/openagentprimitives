// Package celbudget holds the runtime budget every compiled CEL program in this
// repo carries.
//
// CEL is user-authored — and in this system "user" is broader than it sounds:
// toolspec constraints are LLM-generated from documents that may be hostile,
// permission `when` expressions and resource-id expressions come off CRDs a
// tenant writes, and scope expressions arrive from a metaagent. None of them was
// bounded. A single expensive expression therefore ran to completion however
// long that took.
//
// Where that lands worst is the operator: toolspec constraints are evaluated
// during ToolCall reconciliation, which is serialized to one worker, so one
// runaway expression stalls ToolCall reconciliation cluster-wide with no error
// and no terminal condition. A merely quadratic constraint over a model-supplied
// list reaches that by accident.
//
// This lives in its own leaf package because BOTH pkg/authz and pkg/tools
// compile CEL and neither may import the other. A tenth compile site added
// later gets the budget by calling ProgramOptions, which is the only spelling
// worth having — a constant each site remembers to pass is a constant one site
// forgets.
package celbudget

import "github.com/google/cel-go/cel"

// CostLimit bounds the runtime cost of ONE evaluation.
//
// Cost is cel-go's own metric for operations performed — indicative of CPU, not
// memory. It is the bound that works with NO context plumbing, which matters
// because most evaluation here uses the non-context form and therefore has no
// deadline to interrupt.
//
// Sized for "an expression over one call's arguments" with a wide margin: an
// ordinary flag or argument predicate costs on the order of tens. A limit low
// enough to clip real expressions would be an outage that reads as a flaky
// spec, which is worse than the problem it was meant to solve.
const CostLimit = 200_000

// InterruptCheckFrequency is how often a running program tests for
// cancellation, for the call sites that DO evaluate with a context
// (Program.ContextEval). Cheap — a counter comparison — and it is what makes a
// deadline actually stop an expression rather than merely be recorded.
const InterruptCheckFrequency = 1_000

// ProgramOptions returns the options every env.Program call in this repo
// passes. Call it rather than spelling the constants: the point is that adding
// a compile site cannot silently opt out of the budget.
func ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{
		cel.CostLimit(CostLimit),
		cel.InterruptCheckFrequency(InterruptCheckFrequency),
	}
}
