// Package llm defines the Provider interface and shared request/response types.
// Concrete implementations live in sibling packages: llm/promptlib (shipping,
// an adapter over the agent-loop's own pkg/agent/llm.Provider), llm/fake
// (tests).
package llm

import "context"

// Provider is the abstraction over any LLM backend used by gen.
type Provider interface {
	SelectToolkit(ctx context.Context, req SelectRequest) (*SelectResponse, error)
	GenerateSpec(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	GenerateTestCases(ctx context.Context, req TestRequest) (*TestResponse, error)
	RefineSpec(ctx context.Context, req RefineRequest) (*GenerateResponse, error)
}

// --- Selection phase ---

type SelectRequest struct {
	Intent           string
	ToolkitSummaries []ToolkitSummary
}

type SelectResponse struct {
	ToolkitName string
	Reasoning   string
	Unmatched   []Unmatched
}

// --- Generation phase ---

type GenerateRequest struct {
	Intent      string
	ToolkitName string
	ToolkitYAML string
	PriorError  string // non-empty on retry: short-form error from previous attempt
}

type GenerateResponse struct {
	SpecYAML     string
	Warnings     []string
	Unmatched    []Unmatched
	Excluded     []Excluded
	Descriptions map[string]string
}

// --- Test-case generation ---

type TestRequest struct {
	Intent      string
	ToolkitName string
	ToolkitYAML string
}

type TestResponse struct {
	TestCases []TestCase
}

// --- Refinement (convergence loop) ---

type RefineRequest struct {
	Intent            string
	ToolkitName       string
	ToolkitYAML       string
	PriorSpecYAML     string
	PriorDescriptions map[string]string
	Mismatches        []Mismatch
	PriorError        string // non-empty on schema-validation retry: short-form error from previous attempt
}

type Mismatch struct {
	TestCase TestCase
	Actual   bool
	Reason   string
}

// --- Shared types ---

// TestCase is the LLM-facing form of a generated test case. The spec package
// has its own TestCase with a LastRunActual field for persistence.
type TestCase struct {
	Intent        string
	Argv          []string
	Env           map[string]string
	Cwd           string
	BinaryVersion string
	ExpectAllow   bool
}

type Unmatched struct {
	Request string
	Reason  string
}

type Excluded struct {
	Name   string
	Reason string
}

// ToolkitSummary is the compact shape catalogs emit for Phase-1 prompts.
type ToolkitSummary struct {
	Name        string
	Binary      string
	Description string
	Subcommands []SubcommandSummary
}

type SubcommandSummary struct {
	Path                []string
	Description         string
	Destructive         bool
	Reads               []string
	Writes              []string
	NetworkDestinations []string
}
