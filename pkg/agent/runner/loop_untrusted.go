package runner

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
)

// untrustedToolOutputTag is the XML element name wrapping every tool_result's
// Content before it is fed back to the LLM. A fresh random nonce is embedded in
// each open/close tag so a malicious tool output cannot craft a matching close
// tag and escape the wrapper. ComposeSystem (prompt.go) builds its
// prompt-injection rule from this same constant, so the two cannot drift.
//
// Defined in pkg/agent/toolenvelope, which steelthread capture also imports to
// unwrap the same envelope back out of session memory — one definition shared
// by both the writer and the reader.
const untrustedToolOutputTag = toolenvelope.Tag

// newUntrustedOutputNonce returns a 16-character lowercase hex string (8 bytes
// from crypto/rand). It panics on crypto/rand failure: a predictable or empty
// nonce would silently defeat the wrapper's injection defence, which is worse
// than a loud crash. In practice crypto/rand never fails on supported
// platforms; a failure here indicates a catastrophic OS-level problem.
func newUntrustedOutputNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("runner: crypto/rand unavailable — cannot generate safe tool-output nonce: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// wrapUntrustedToolOutputWithNonce wraps content in nonce-bearing delimiters
// using the supplied nonce. It is a pure function intended for testing.
//
//	<untrusted-tool-output nonce="NONCE">
//	...content...
//	</untrusted-tool-output nonce="NONCE">
func wrapUntrustedToolOutputWithNonce(content, nonce string) string {
	return toolenvelope.Wrap(content, nonce)
}

// wrapUntrustedToolOutput generates a fresh crypto-random nonce and wraps
// content so the LLM receives a clear signal that the enclosed text is
// external data that must never be treated as instructions.
func wrapUntrustedToolOutput(content string) string {
	return wrapUntrustedToolOutputWithNonce(content, newUntrustedOutputNonce())
}
