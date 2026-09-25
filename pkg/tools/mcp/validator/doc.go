// Package validator orchestrates per-tool-call validation of MCP server
// invocations against an MCPServer spec, producing a Decision that
// mirrors the shape of pkg/tools/toolspec/validator.Decision.
//
// Phases (each phase short-circuits the pipeline on first deny):
//
//	tool → allowedFields → deny.effects → deny.trust → constraints → allow.
//
// The toolspec and MCP validators are deliberately parallel, and share their
// Decision / Trace / Redaction machinery through pkg/authz/validator/core.
package validator
