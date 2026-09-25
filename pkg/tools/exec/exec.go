// Package exec wraps client-go's remote command executor behind an
// interface so controllers can be tested with an in-process fake.
package exec

import (
	"context"
	"io"
)

// MaxStreamBytes caps captured stdout/stderr per stream.
// Overflow bytes are discarded and the corresponding Truncated flag is set.
const MaxStreamBytes = 1 << 20 // 1 MiB

// Request describes a single exec invocation.
//
// It carries NO sandbox identity. An Executor is already bound to exactly one
// sandbox — obtained from that sandbox's backend — so there is nothing here for
// a caller to get wrong, and no way to address a different sandbox through a
// bound executor.
type Request struct {
	// Command is argv; Command[0] is the binary, rest are args.
	Command []string
	// Env is key=value pairs set in the tool's environment.
	//
	// Values are treated as SECRET: they routinely carry broker-resolved
	// credentials, so a transport must not place them anywhere a third party
	// observes a request — notably not in argv, which the kubelet transport
	// encodes into the exec URL's query string (see pkg/tools/exec/remote/env.go).
	//
	// Keys must be POSIX environment-variable names
	// (`[A-Za-z_][A-Za-z0-9_]*`). A transport MAY reject anything else: a name
	// outside that grammar cannot be referenced by the tool it is set for, and
	// cannot be exported by a shell-based delivery mechanism. Rejection is an
	// error, never a silently dropped variable.
	Env map[string]string
	// Stdin is the bytes piped to the tool's stdin. May be nil.
	//
	// For StreamExec a non-nil Stdin is a finite source: the tool drains it
	// and sees EOF, and Stream.Stdin is a no-op closed writer. A nil Stdin
	// makes StreamExec open a live pipe writable via Stream.Stdin (the
	// interactive-bridge path). Stream-mode callers with no input source
	// should pass an empty reader, not nil, so the tool sees stdin closed
	// instead of an open never-written pipe.
	Stdin io.Reader
}

// Result reports the exec outcome.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	// ExitCode is the process exit status. Zero means success.
	// On transport errors (stream failed before process exit) ExitCode
	// is -1 and Err is non-nil.
	ExitCode int32
}

// Executor runs commands in ONE sandbox, to which it is already bound.
//
// Implementations are obtained from a transport's binding constructor (for
// example pkg/tools/exec/remote.Binder.For) rather than constructed per call, so the
// target cannot vary per Request. This package deliberately knows nothing about
// how a sandbox is addressed — only the transport does.
type Executor interface {
	// Exec runs req and returns Result when the process exits or ctx is cancelled.
	// On context cancellation, implementations should close the exec stream; the
	// running process on the far side may continue but its output is no longer captured.
	Exec(ctx context.Context, req Request) (Result, error)
}

// Stream exposes pipes to a running remote process.
type Stream struct {
	// Stdin is the writer the caller sends bytes to. Close it to signal EOF.
	Stdin io.WriteCloser
	// Stdout and Stderr are readers for the tool's output streams.
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	// Wait blocks until the remote process exits. Returns the final Result
	// (ExitCode only; stdout/stderr bytes were streamed, not buffered).
	Wait func() (Result, error)
	// Close tears down the stream (best-effort). Callers must always call
	// Close — even after Wait — to release underlying goroutines.
	Close func() error
}

// StreamingExecutor is an optional extension of Executor that returns live
// pipes instead of buffered results. Implementations MAY satisfy only Executor;
// callers should type-assert.
type StreamingExecutor interface {
	StreamExec(ctx context.Context, req Request) (*Stream, error)
}
