// Package workspacekinds defines the pluggable driver surface for workspace
// sources — the origin a per-session workspace is cut from (a git repo today;
// hg, blob storage, or a local directory tomorrow). Drivers are command
// builders, not executors: they return the argv+env to run, and the framework
// decides where to run it (a Job against the shared base PVC, or a ToolCall in
// the sandbox pod). Keeping this package free of any apis/v1alpha1 import is
// what lets the driver layer be unit-tested without a cluster.
package workspacekinds

import (
	"errors"
	"path"
	"strings"
)

// Spec is the kind-agnostic, Kubernetes-free description of a workspace source a
// driver operates on. The WorkspaceSource controller (Phase 2) translates a
// WorkspaceSource CR into this struct.
type Spec struct {
	// Kind is the registry key selecting the driver (e.g. "git").
	Kind string
	// Locator is the driver-parsed source location: a repo URL, a file:// host
	// path, a gs:///s3:// prefix, etc.
	Locator string
	// Ref is an optional revision to check out; empty = driver default. For the
	// git driver this is a branch or tag (it becomes `git clone --branch`);
	// pinning to a raw commit SHA is a deferred enhancement (needs
	// clone+fetch+checkout), not yet supported.
	Ref string
	// Config carries per-kind extra options (blob include/exclude rules, hg
	// opts). Parsed by the kind; nil is valid.
	Config map[string]string
	// Scope is the exposure boundary.
	Scope Scope
}

// Scope is the allowlist of paths a source exposes, and which are writable. An
// empty Paths means "the whole source, read-only".
type Scope struct {
	Paths []PathRule
}

// PathRule allows a single relative subpath of the source, optionally writable.
type PathRule struct {
	// Path is a slash-separated path relative to the source root. It must not be
	// absolute, contain "..", reference a home dir ("~"), contain a backslash,
	// or have leading/trailing whitespace.
	Path string
	// Writable marks the subtree as eligible for write-back (apply); read-only
	// otherwise.
	Writable bool
}

// ValidateScopePath reports whether p is a safe relative subpath for a scope
// rule: non-empty, no leading/trailing whitespace, no backslash, no home
// reference ("~"), not absolute, and no ".." segment. Every driver's Validate
// funnels its scope paths through this one check — the rules are a property of
// the overlay-mount boundary all drivers share, not of any single driver, so a
// new backend reuses it rather than re-deriving (and subtly diverging on) the
// path-safety rules. Pure; no I/O.
func ValidateScopePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path is empty")
	}
	if p != strings.TrimSpace(p) {
		return errors.New("path must not have leading or trailing whitespace")
	}
	if strings.Contains(p, `\`) {
		return errors.New(`path must not contain a backslash`)
	}
	if strings.HasPrefix(p, "~") {
		return errors.New("path must not reference a home directory")
	}
	if path.IsAbs(p) {
		return errors.New("path must be relative to the source root")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New(`path must not contain ".."`)
		}
	}
	return nil
}

// Command is a single argv-based process invocation (never shell-interpreted, to
// avoid injection through a hostile locator) that the framework runs — inside a
// Job for base materialization, or in the sandbox pod for overlay sync/apply.
type Command struct {
	// Dir is the working directory to run in ("" = the caller's cwd).
	Dir string
	// Env is the complete, locked-down environment for the process. Drivers emit
	// a hermetic env (no ambient credential helpers, restricted protocols).
	Env []string
	// Argv is the program and its arguments; Argv[0] is the program name.
	Argv []string
}

// Kind is the per-workspace-source-kind plug-in surface. A backend implements
// this and self-registers into pkg/platform/workspacekinds/registry via an init().
// Consumers never switch on the kind string — they resolve it from the registry
// by name.
type Kind interface {
	// Name is the registry key, matching WorkspaceSource.spec.source.kind.
	Name() string
	// Validate checks a Spec is structurally well-formed (locator syntax, scope
	// path allowlist). Pure; no I/O.
	Validate(spec Spec) error
	// MaterializeCommands returns the commands that populate destDir with a fresh
	// copy of the source (the shared read-cache base).
	MaterializeCommands(spec Spec, destDir string) ([]Command, error)
	// SyncCommands returns the commands that update an existing workDir to the
	// latest source revision (the async "sync now" path).
	SyncCommands(spec Spec, workDir string) ([]Command, error)
}

// Applier is the optional write-back half of a workspace-source driver, for
// drivers that can reconcile overlay edits to the origin (git push, hg push,
// blob upload). The framework discovers it by type assertion and offers
// apply_workspace only when the bound source's driver is an Applier.
//
// Apply always runs behind the capability plus a per-call human approval, with
// the write-back credential injected by the framework into the run environment
// — never emitted in these Commands. That credential is the session's
// passthrough Secret, not a minted least-privilege token: "per-session"
// describes its delivery, not a narrowing of its authority.
type Applier interface {
	// ApplyCommands returns the commands that reconcile the overlay at workDir
	// back to the source origin (e.g. push the overlay's branch).
	ApplyCommands(spec Spec, workDir string) ([]Command, error)
}

// CredentialedApplier is an Applier whose ApplyCommands authenticate using a
// per-session write-back credential the framework injects as an environment
// variable. The driver declares the env var name its commands read and the
// Secret key that holds the token; the framework wires env[envVar] <-
// secretKeyRef(key) into the reconcile run (never onto argv). A driver that
// pushes anonymously (or to a public/file remote) need not implement it.
type CredentialedApplier interface {
	Applier
	// ApplyCredential returns the env var name ApplyCommands read the token
	// from, and the Secret key (in the session credential Secret) holding it.
	ApplyCredential() (envVar, secretKey string)
}
