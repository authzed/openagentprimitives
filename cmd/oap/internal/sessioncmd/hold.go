// `oap session hold` — manually freeze an AgentSession for forensic review.
//
// This is the human-driven counterpart to the automated plan-gate-denial
// tripper (pkg/authz/plangate/hold): the same SessionHold CR, the same
// owner-ref discipline, but tripped on a human's judgement instead of a
// signal read from the durable audit log.
//
// There is deliberately no `oap session release`. Release is a human decision
// made on the resulting approval card, gated to the session's owner — a CLI
// command that lifted a hold directly would be a second, ungated path to the
// exact thing that gate exists to protect.
//
// Creating the CR is gated on agentsession#hold, checked against the caller's
// canonical identity after the target session is confirmed to exist but
// before anything is created. A caller who lacks it is refused with the
// permission name and the session it was checked on — not a bare
// "forbidden".
package sessioncmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8srand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// holdPermissionChecker is the one method of *spicedb.Client this command
// needs: the agentsession#hold gate. Kept narrow and swappable so the
// tightening below (refused ⇒ no CR) is provable from a fake in a unit test,
// without a live SpiceDB connection.
type holdPermissionChecker interface {
	CheckHold(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// newHoldChecker is replaceable by tests. Production dials SpiceDB from the
// standard SPICEDB_* env vars, exactly like every other session subcommand
// that touches SpiceDB (see grant.go's apspicedb.NewClientFromEnv call).
//
// Returns a genuine nil interface on error — NOT a typed-nil *spicedb.Client
// wrapped into a non-nil interface, which would let a failed connection sail
// past a `!= nil` guard and panic on first use.
var newHoldChecker = func() (holdPermissionChecker, error) {
	cl, err := apspicedb.NewClientFromEnv()
	if err != nil {
		return nil, err
	}
	return cl, nil
}

// buildHold constructs the spec of a manually-tripped SessionHold. It is a
// PURE constructor: no clock, no randomness, no I/O, so two calls with
// identical arguments produce byte-identical Spec — required because Spec is
// the part of this object a client server-side-applies, and a volatile value
// there would make re-running this command a non-no-op re-apply.
//
// The object's Name (session name plus a random suffix) and its
// OwnerReferences (which need a live Get of the AgentSession, for its UID)
// are both call-site concerns — set by the caller on the returned object —
// so this constructor stays callable from a test with no cluster and no
// randomness to control.
func buildHold(ns, session, reason, subject string) *spiceboxv1alpha1.SessionHold {
	return &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
		},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: session},
			Reason:     reason,
			Source:     "manual",
			TrippedBy:  identity.Subject(subject),
		},
	}
}

// newHoldCmd builds the `hold` command's shape: usage, argument count, and
// the required --reason flag. It takes no Globals, so a test can drive its
// flag validation directly. cobra only runs ValidateRequiredFlags on a
// Runnable command (RunE non-nil) — a command with no RunE at all short-
// circuits Execute() to a help screen (returning a nil error) before the
// required-flag check ever runs — so a placeholder RunE is set here purely to
// make the command Runnable; newSessionHoldCmd overwrites it with the real,
// Globals-aware implementation before this command is ever reachable as `oap
// session hold`.
func newHoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hold <session>",
		Short: "Freeze an AgentSession for forensic review, pending a human decision on the release card",
		Long: `Creates a SessionHold that freezes the named AgentSession: the runner
stops accepting new turns and a release card is published for a human to
review the session's history and either release it or leave it held.

--reason is required — it is platform-authored text shown on that card, and a
hold with no reason gives the reviewer nothing to act on.

There is no CLI release: release is a human decision on the card, gated to the
session's owner.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("sessioncmd: hold command was not wired to a cluster (use newSessionHoldCmd)")
		},
	}
	cmd.Flags().String("reason", "", "Why this session is being held (required; shown on the release card)")
	// MarkFlagRequired's error ("required flag(s) \"reason\" not set") only
	// checks the flag was SET, not that it is non-empty; runSessionHold rejects
	// a blank --reason "" too, below.
	if err := cmd.MarkFlagRequired("reason"); err != nil {
		// Only fires on a typo'd flag name — a programming error, not a
		// runtime condition — so panicking here (at command-tree construction,
		// long before any user input) is the honest answer.
		panic(fmt.Sprintf("sessioncmd: mark --reason required: %v", err))
	}
	return cmd
}

// newSessionHoldCmd wires newHoldCmd's shape to a real Globals, matching the
// rest of this family's newSession<Verb>Cmd(g) constructors.
func newSessionHoldCmd(g *apcmd.Globals) *cobra.Command {
	cmd := newHoldCmd()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		reason, err := cmd.Flags().GetString("reason")
		if err != nil {
			return err
		}
		subject, err := resolveActorSubject(cmd.Context(), g)
		if err != nil {
			return err
		}
		return runSessionHold(cmd.Context(), cmd.OutOrStdout(), g, args[0], reason, string(subject))
	}
	return cmd
}

// resolveActorSubject resolves the CLI caller's canonical SpiceDB subject —
// the human who typed this command, recorded on the hold as TrippedBy.
// AllowSynthetic is set because the local-install fallback identity (no
// verified email) is still a legitimate hold-tripper on a cluster with no IdP
// configured.
func resolveActorSubject(ctx context.Context, g *apcmd.Globals) (identity.Subject, error) {
	p, err := clilogin.EnsureIdentity(ctx, g)
	if err != nil {
		return "", fmt.Errorf("resolve caller identity: %w", err)
	}
	c, err := p.AllowSynthetic().Canonical()
	if err != nil {
		return "", fmt.Errorf("canonicalize caller identity: %w", err)
	}
	return c.Subject(), nil
}

// runSessionHold is the body of `oap session hold`, factored out of RunE so
// it is reachable from tests without going through cobra flag parsing or
// identity resolution.
func runSessionHold(ctx context.Context, out io.Writer, g *apcmd.Globals, sessionName, reason, subject string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason must not be blank — a hold with no reason produces a card nobody can act on")
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// A genuine OwnerReference is required, not optional: activeHoldFor
	// (pkg/controllers/agentsession/hold.go) discovers holds by listing the
	// namespace and matching spec.sessionRef.Name, NOT by owner-ref, so an
	// ownerless hold left behind by a deleted session would keep matching a
	// differently-provisioned session later recreated under the same name —
	// channel-attached sessions are named deterministically, so that
	// recreation is ordinary — and freeze it on sight with a stale reason.
	// Fetching the session fresh here, rather than trusting the caller's
	// spelling, is what makes the owner-ref possible: it needs the session's
	// UID.
	//
	// This Get runs BEFORE the agentsession#hold check, deliberately: it is
	// gated by the caller's OWN Kubernetes RBAC (b.Controller is built from
	// g's kubeconfig — see apcmd.Globals.Bundle), the same identity the hold
	// check below gates, so ordering opens no information channel either way.
	// With no incremental exposure, existence-first strictly dominates: a
	// typo'd name gets "not found" and a real session with no permission gets
	// "lacks agentsession#hold" — both precise, instead of one of the two
	// being masked behind the other's message.
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			// Unlike the automated tripper (which treats a vanished session as a
			// no-op — nothing was waiting on that trip), a CLI invocation has a
			// human waiting on it who typed the name themselves. Silently doing
			// nothing would look identical to success; a clear, named error is
			// the more useful answer here.
			return fmt.Errorf("AgentSession %s/%s not found — check the session name", b.Namespace, sessionName)
		}
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}

	// Gate the trip on agentsession#hold before creating anything — a CLI
	// user's ability to create the SessionHold CR used to be decided entirely
	// by their Kubernetes RBAC. Fail closed: a check that errors refuses
	// rather than proceeds, exactly like a refusal on the permission itself.
	canonicalID, err := identity.Subject(subject).CanonicalUserID()
	if err != nil {
		return fmt.Errorf("resolve caller %q as a SpiceDB user subject: %w", subject, err)
	}
	checker, err := newHoldChecker()
	if err != nil {
		return fmt.Errorf("connect to SpiceDB to check agentsession#hold: %w", err)
	}
	if closer, ok := checker.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	allowed, err := checker.CheckHold(ctx, b.Namespace, sessionName, canonicalID, true)
	if err != nil {
		return fmt.Errorf("check agentsession#hold for %s on %s/%s: %w", subject, b.Namespace, sessionName, err)
	}
	if !allowed {
		return fmt.Errorf("%s lacks agentsession#hold on %s/%s — only the session's owner (or the owner of an ancestor session) may freeze it for review",
			subject, b.Namespace, sessionName)
	}

	h := buildHold(b.Namespace, sessionName, reason, subject)
	// The name carries a random suffix (generated here, not inside buildHold)
	// so that re-running this command — a human deciding a session still
	// warrants review after already holding it once — creates a second,
	// independent hold rather than colliding with the first.
	h.Name = fmt.Sprintf("hold-%s-%s", sessionName, k8srand.String(5)) // already lowercase-alphanumeric, DNS-1123-safe
	h.OwnerReferences = cosidecar.OwnerRef(&sess)

	if err := b.Controller.Create(ctx, h); err != nil {
		return fmt.Errorf("create SessionHold %s/%s: %w", b.Namespace, h.Name, err)
	}
	fmt.Fprintf(out, "held agentsession %s/%s (hold %s): %s\n", b.Namespace, sessionName, h.Name, reason)
	return nil
}
