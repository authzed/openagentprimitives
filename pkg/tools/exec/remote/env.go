package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// Environment delivery for the kubelet exec transport.
//
// The kubelet's pods/exec subresource carries PodExecOptions — argv included —
// as QUERY PARAMETERS on the request URL. Anything placed in argv is therefore
// written verbatim to:
//
//   - the apiserver's audit log, as requestURI, and to every proxy/ingress
//     access log in front of it;
//   - /proc/<pid>/cmdline inside the sandbox, readable for the tool's whole
//     lifetime by any OTHER tool call running concurrently in the same
//     container — a tool that was never granted that credential;
//   - a *url.Error from a failed SPDY dial, which stringifies as
//     `Post "<full URL incl. query>": …` and is persisted by the ToolCall
//     controller into ToolCall.status.
//
// Tool env vars are broker-resolved credentials (pkg/controllers/toolcall
// resolves them and masks them for status only), so none of those three may
// see them. They are delivered instead through a per-call file:
//
//  1. stageEnvFile runs ONE extra exec whose argv is only `sh -c … <path>`;
//     the `export KEY='…'` script rides that exec's STDIN.
//  2. envLoaderArgv rewrites the tool's argv to source that file, unlink it,
//     and `exec` the original command — so the tool still receives real
//     environment variables and keeps the same pid, exit status, and
//     stdin/stdout/stderr.
//
// Stdin is deliberately NOT used to carry the values into the tool's own exec:
// every ToolCall mode already occupies it (spec.stdin for sync and stream mode,
// a live channel-fed pipe for interactive mode).
//
// # What the staged file is and is not protected by
//
// Each protection is narrow and bounds a different attacker; NONE bounds a
// same-UID sibling, so do not read one as if it did:
//
//   - `umask 077` bounds only OTHER UIDs — a sidecar or init container sharing
//     the mount. Every exec into the sandbox container runs as the same UID.
//   - The 128-bit nonce bounds only a PREDICTIVE attacker: the path cannot be
//     squatted before creation. It hides nothing after — `/tmp/.ap-env-*`
//     globs it.
//   - The real bound on a same-UID sibling is the STAGING-TO-EXEC WINDOW: one
//     apiserver round trip between stageEnvFile returning and the loader's rm.
//     removeEnvFile is what keeps a FAILED call from widening that to the
//     pod's lifetime.
//
// The window is reachable: a stream-mode or interactive ToolCall keeps running
// across later reconciles, so an agent driving one can poll `/tmp/.ap-env-*`
// for a sibling's credential. Closing it needs per-call filesystem isolation
// (a `mkdir 0700` does not help — the same UID traverses its own directory)
// and belongs to the sandbox backend, not this transport.
//
// # The environ channel: bullet 2 above is NOT closed by this file
//
// Moving the values out of argv closes /proc/<pid>/cmdline. It does not close
// /proc/<pid>/environ, which the loader's `export` puts them into and which
// reproduces the same exposure — for STRICTLY LONGER, since it lasts the tool
// process's whole lifetime rather than the staging window.
//
// Nothing in the platform bounds it. Every pods/exec lands in the one sandbox
// container as uid 1000 (podspec/security.go), the podspec sets
// ShareProcessNamespace, and nothing anywhere sets procMount or hidepid.
// /proc/<pid>/environ is 0400 owned by the process UID, and a same-UID reader
// passes PTRACE_MODE_READ_FSCREDS; AllowPrivilegeEscalation: false with no
// setuid leaves the target dumpable, so ownership stays uid 1000.
//
// The consequence is that per-toolspec credential scoping — synthesize.go
// correctly stamps Credentials per toolspec, so a tool with no credential has
// an empty ToolCall.Spec.Credentials — does not hold INSIDE the container. A
// tool call with code execution can detach `while :; do cat /proc/[0-9]*/environ
// ...; done`, which survives its own exec because the container's main process
// is `sleep infinity` and the pause container reaps the orphan. No concurrency
// is required, and checkTokenUse never sees the second reader.
//
// This is stated rather than fixed because the fix is not available here: it
// needs a distinct UID per credentialed call (so, a per-call container or
// sandbox) or securityContext.procMount / a hidepid=2 /proc — all properties of
// the sandbox BACKEND. Do not read the argv work above as having addressed it.
const (
	// envFileDir is where the per-call env script lands. In the operator's
	// sandbox podspec /tmp is a MEMORY-backed emptyDir, so the value never
	// reaches the node's disk — but it lives for the POD's lifetime, not the
	// call's, which is why an orphaned file is a session-long leak and every
	// failure path unlinks one.
	envFileDir = "/tmp"
	// envFilePrefix is dot-prefixed so a plain `ls /tmp` does not surface it.
	// That is cosmetic only: a glob finds it, and it protects nothing.
	envFilePrefix = ".ap-env-"
)

// stageScript writes stdin to the path in $1. umask 077 keeps the file off
// every other UID (a sidecar, an init container sharing the mount) — see the
// protections note above for what it does not do.
const stageScript = `umask 077 && cat > "$1"`

// unlinkScript removes the path in $1. It is what the transport runs when the
// tool's own exec never reached envLoaderScript, which is the only other thing
// that unlinks a staged file. `rm -f` on an absent path exits 0, so a cleanup
// that crosses a loader which already removed the file is a no-op rather than a
// spurious failure.
const unlinkScript = `rm -f -- "$1"`

// envCleanupTimeout bounds one best-effort unlink. Short on purpose: the
// transport being unreachable is a common reason the unlink is needed at all,
// and a goroutine parked on a dead SPDY dial helps nobody.
const envCleanupTimeout = 15 * time.Second

// envLoaderScript sources the staged file, removes it, and execs the real
// command. `exec` replaces the shell, so the tool keeps this pid: its exit
// status, signals, and stdio are the shell's, and the kubelet sees exactly
// what it would have seen without the wrapper.
//
// The rm runs BEFORE exec, so the file is gone by the time the tool — and any
// sibling tool call in the same sandbox — could read it. The generated script
// also unlinks itself as its first line, which closes the window even if
// sourcing fails partway; POSIX keeps an unlinked file readable through the
// open descriptor, so sourcing completes normally.
const envLoaderScript = `[ -f "$1" ] || exit 125
. "$1"
rm -f "$1"
shift
exec "$@"`

// envNameRE is the POSIX env-var name grammar. A shell cannot `export` a name
// outside it, so a key that fails this cannot be delivered at all. Mirrors
// toolcall.IsValidEnvName; duplicated rather than imported because an exec
// transport must not depend on a controller package.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellSingleQuote renders s as a POSIX single-quoted word. Single quotes
// suppress every expansion, so only the single quote itself needs care: close
// the word, emit an escaped quote, reopen. Total for any value execve can carry
// — newlines (PEM keys, kubeconfigs), dollar signs, backticks and backslashes
// all survive byte-for-byte.
//
// (The splice sequence is spelled only in the code below: gofmt rewrites a
// doubled-quote pair in a doc comment into typographic quotes.)
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildEnvScript renders env as a shell script that exports every pair and
// unlinks itself first. Keys are emitted in sorted order so the script is a
// pure function of its inputs (Go map iteration is randomized).
//
// Fail-closed: an unexportable key, or a value carrying a NUL (which execve
// cannot represent and which would silently truncate the value), is an error
// rather than a dropped variable — a tool running with a credential missing
// looks like an upstream auth outage and costs far more to diagnose than a
// refusal here.
func buildEnvScript(env map[string]string, path string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	// Self-destruct first: from here on the content exists only in this
	// shell's open descriptor.
	fmt.Fprintf(&b, "rm -f -- %s\n", shellSingleQuote(path))
	for _, k := range keys {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("remote: env key %q is not a POSIX name and cannot be exported to the tool", k)
		}
		if strings.ContainsRune(env[k], 0) {
			return nil, fmt.Errorf("remote: env value for %q contains a NUL byte, which no process environment can carry", k)
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shellSingleQuote(env[k]))
	}
	return b.Bytes(), nil
}

// newEnvFilePath returns a fresh per-call path. The 128-bit nonce makes the
// path unpredictable, which is what stops an attacker from creating (or
// symlinking) it BEFORE staging does — it does not conceal the file afterwards,
// since a sibling in the same container globs `/tmp/.ap-env-*` rather than
// guessing. Hex keeps the path shell- and URL-inert, so it is safe to place in
// argv (it is the ONLY thing about the env that goes there).
func newEnvFilePath() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("remote: generate env file nonce: %w", err)
	}
	return envFileDir + "/" + envFilePrefix + hex.EncodeToString(nonce[:]), nil
}

// stageEnvFile writes env into the sandbox and returns the path holding it.
//
// The staging exec's argv names only the destination path; the values travel on
// its stdin, which the kubelet does not put in the URL. It reuses Exec — safely
// non-recursive, because the staging Request carries no Env of its own.
//
// Every failure is returned. There is deliberately no fallback to argv
// delivery: silently degrading to the leaky path on a transient staging error
// would make the leak intermittent and invisible. A failure that may have left
// a partial file behind unlinks it before returning, so a refused call leaves
// nothing readable in the sandbox.
func (e *boundExecutor) stageEnvFile(ctx context.Context, env map[string]string) (string, error) {
	path, err := newEnvFilePath()
	if err != nil {
		return "", err
	}
	script, err := buildEnvScript(env, path)
	if err != nil {
		return "", err
	}

	res, err := e.Exec(ctx, exec.Request{
		Command: []string{"sh", "-c", stageScript, "ap-stage-env", path},
		Stdin:   bytes.NewReader(script),
	})
	// A staging exec that started and then failed may still have created the
	// file: `cat > "$1"` truncates it into existence before the first byte
	// arrives, and a transport drop or a short write leaves whatever landed. A
	// partially written credential is still a credential, and nothing else will
	// ever remove it — the self-unlink is the script's first line, which only
	// runs when the file is SOURCED.
	if err != nil {
		e.removeEnvFile(ctx, path)
		return "", fmt.Errorf("remote: stage env file: %w", err)
	}
	if res.ExitCode != 0 {
		e.removeEnvFile(ctx, path)
		return "", fmt.Errorf("remote: stage env file: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return path, nil
}

// removeEnvFile unlinks a staged env file the tool never consumed. Best effort:
// there is nothing useful a caller can do with a cleanup failure that it is not
// already doing about the failure that caused the cleanup, so this reports
// through the log rather than the return.
//
// It runs on a FRESH, non-cancelled context: a cancelled or expired caller ctx
// is one of the most common reasons this is called at all (ToolCall wraps every
// exec in a per-call deadline), so inheriting it would guarantee the orphan.
// context.WithoutCancel keeps the values, logger included, and drops only the
// cancellation.
//
// Only the path travels in argv, never a key and never a value; the path is a
// hex nonce, so it is safe in the exec URL the apiserver audits.
func (e *boundExecutor) removeEnvFile(ctx context.Context, path string) {
	logger := log.FromContext(ctx).WithValues(
		"namespace", e.namespace, "pod", e.pod, "container", e.container, "path", path)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), envCleanupTimeout)
	defer cancel()

	// This Request carries no Env, so withEnv is a no-op for it and the call
	// cannot recurse into staging.
	res, err := e.Exec(ctx, exec.Request{
		Command: []string{"sh", "-c", unlinkScript, "ap-unstage-env", path},
	})
	if err == nil && res.ExitCode == 0 {
		return
	}
	if err == nil {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	// Loud on purpose: what survives is a plaintext credential readable, as the
	// UID every exec into this container runs as, by every later tool call in
	// the session — for as long as the pod lives.
	logger.Error(err, "could not remove a staged tool-credential file from the sandbox")
}

// envLoaderArgv wraps cmd so the tool runs with the staged file's variables in
// its environment. Only path appears in argv — never a key and never a value.
func envLoaderArgv(path string, cmd []string) []string {
	// "ap-env" is $0 (the shell's name in its own error messages); path is $1;
	// cmd starts at $2 and becomes "$@" after the loader's shift.
	argv := make([]string, 0, len(cmd)+5)
	argv = append(argv, "sh", "-c", envLoaderScript, "ap-env", path)
	return append(argv, cmd...)
}

// withEnv returns the argv to exec for req — unchanged when req carries no env,
// otherwise the loader wrapper around a freshly staged per-call env file — plus
// a cleanup func the caller MUST run whenever the tool's exec did not reach the
// loader.
//
// The loader, and only the loader, unlinks the staged file, so every failure
// between staging and the tool running — a build error, an SPDY dial or auth
// failure, a terminating pod, a ctx deadline — would otherwise leave a
// plaintext credential behind. /tmp is a memory emptyDir with the POD's
// lifetime and every later exec runs as the same UID, so an orphan is readable
// by every subsequent tool call in the session: the cross-tool capability leak
// this file exists to close, unbounded in time rather than by one process.
//
// The cleanup is idempotent (once-guarded) and a no-op when req carried no env,
// so a caller may defer it unconditionally.
func (e *boundExecutor) withEnv(ctx context.Context, req exec.Request) ([]string, func(), error) {
	noCleanup := func() {}
	if len(req.Env) == 0 {
		return req.Command, noCleanup, nil
	}
	path, err := e.stageEnvFile(ctx, req.Env)
	if err != nil {
		// stageEnvFile already removed whatever it may have created.
		return nil, noCleanup, err
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { e.removeEnvFile(ctx, path) }) }
	return envLoaderArgv(path, req.Command), cleanup, nil
}
