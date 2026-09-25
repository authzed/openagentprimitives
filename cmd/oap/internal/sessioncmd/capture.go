package sessioncmd

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
	"github.com/authzed/openagentprimitives/toolkits"

	// The asset renderers the RUNNER links (internal/cmd/runner/main.go). The
	// artifacts capability asks the renderer registry which asset kinds exist
	// before offering artifact_prepare and friends, so a capture assembling
	// meta tools against an empty registry would be told the session had none —
	// and every artifact_* call in the transcript would then read as a sandbox
	// call the bundle cannot replay. mcpui is deliberately absent here too: it
	// is operator-only and not agent-selectable.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/image"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"

	// The channel kinds, for the same reason and in two places. resolve.ForSession
	// refuses a binding whose kind is unregistered, which would put the whole
	// assembly on the ResolveErr path and drop every channel-sourced meta tool;
	// and RewriteFixture asks the same registry which Secret keys a trigger
	// Channel's kind requires, falling back to a generic placeholder key for one
	// it does not know. Both degrade quietly, so the imports live HERE rather
	// than being inherited from whichever sibling command happens to link them.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// captureFlags is what the command was invoked with, gathered into a struct
// rather than threaded through as positional arguments — the redaction pair
// took the count past where a reader can tell which string is which at a call
// site.
type captureFlags struct {
	out        string
	name       string
	desc       string
	overwrite  bool
	redact     []string
	redactFrom string
	elideSkill []string
}

func newSessionCaptureCmd(g *apcmd.Globals) *cobra.Command {
	var f captureFlags
	cmd := &cobra.Command{
		Use:   "capture <session>",
		Short: "Capture a session as a replayable steelthread bundle",
		Long: `Read a finished session's durable records and write a steelthread bundle:
a transcript, the fixture manifests to boot it, the SpiceDB relationships its
authorization depended on, and a golden authorization trace.

Mechanical throughout — no model is invoked, and every emitted field is derived
from a record or left empty. Exits non-zero when the capture cannot be made
faithfully, rather than writing a bundle that would replay differently than it
recorded.

Every byte that would be written is scanned before anything is: once against
the live credential values the referenced Secrets hold, and once for anything
SHAPED like a credential — a private key, a JWT, a provider token, a kubeconfig
client key. A hit refuses the capture, and no flag turns that off.

--redact removes identifiers a scan cannot recognize and a heuristic must not
guess at: a customer, a partner, an internal hostname. Replacements run over the
final bytes BEFORE the scans, so a redaction can remove a secret but can never
suppress a finding about one.

Write the original ALONE (--redact acme-corp) and a stand-in is generated for
you: the same byte length, derived from the original so re-capturing the session
produces the same token, unique across the rules, and spelled so a reader of the
committed fixture can see it is a placeholder.

Name your own (--redact acme-corp=COMPANY-A) and it must be the SAME BYTE LENGTH
as what it replaces. That is enforced, not advised. The bundle records values
DERIVED from the redacted text — an artifact's size, a content length — which
the replay recomputes from the redacted bytes; a shorter token moved one real
capture's artifact size from 5291 to 5276 and the replay diverged at the
artifact, many turns from the rule that caused it.

Give it the same SHAPE too — same case, same character set. A redacted value
still goes through whatever validated it the first time, and an upper-case token
standing in for a lowercase slug makes the replayed call fail its own
constraint. A generated stand-in is lowercase alphanumeric for that reason.

--elide-skill is the structural counterpart for a third-party skill repo, which
a textual rule cannot rewrite: the authority appears in a SkillSource's
repoURL, in each Skill's canonicalName and provenance, in the object names, and
in the controller owner-ref that binds them — and desynchronising any one of
those fails the skill's provenance gate. This moves them together and drops the
skill content that came from that repo.

It REFUSES unless every AgentClass link to the elided source targets the
sandbox. A sandbox-targeted skill is staged to disk and reaches neither the
system prompt nor load_skill, so the run depended only on its existence; an
agent-targeted one's description IS part of the recorded prompt, and emptying
it would replay a different prompt with nothing to notice.

The stand-in authority is held to the SAME BYTE LENGTH as the one it replaces,
exactly as --redact is and for the same reason: the rewrite puts that string
into every canonical name, every repoURL and every skill ref the fixture emits,
and a length change desynchronises any recorded value derived from a byte count.
Write the authority ALONE (--elide-skill github.com/someorg/somerepo) and a
same-length one is generated — counting an exact byte total out of a plausible
repo locator by hand is how an operator ends up picking a shorter one.

The skill CONTENT it drops is deliberately not padded back to its original size.
Nothing derives a count from it: a sandbox-targeted skill's body and description
reach neither the prompt nor load_skill, which is the same fact the target
refusal above rests on.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionCapture(cmd, g, args[0], f)
		},
	}
	// --output/-o, not --out: the tree settled that spelling, and
	// TestFlagSpellingIsConsistentAcrossTheTree in cmd/oap enforces it so a
	// user who learned one command does not have to re-learn it on the next.
	cmd.Flags().StringVarP(&f.out, "output", "o", "", "Directory to write the bundle into (required)")
	cmd.Flags().StringVar(&f.name, "name", "", "Bundle name (default: the session name)")
	cmd.Flags().StringVar(&f.desc, "description", "", "Bundle description")
	cmd.Flags().BoolVar(&f.overwrite, "overwrite", false, "Replace an existing bundle in --output (clears the directory first)")
	cmd.Flags().StringArrayVar(&f.redact, "redact", nil,
		"Replace every occurrence of a string in the emitted bytes, as old=new (the replacement must be the "+
			"same byte length), or as the original alone to have a same-length stand-in generated. Repeatable")
	cmd.Flags().StringVar(&f.redactFrom, "redact-from", "",
		"File of redactions, one per line in either --redact form; # comments and blank lines are ignored")
	cmd.Flags().StringArrayVar(&f.elideSkill, "elide-skill", nil,
		"Rewrite a skill repo authority to a stand-in and drop that source's skill content, as old=new "+
			"(github.com/someorg/somerepo=github.com/exampleorg/examplerepo, and the replacement must be "+
			"the same byte length), or as the authority alone to have a same-length one generated. "+
			"Refused for any skill the AgentClass does not target at the sandbox. Repeatable")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// resolveRedactions gathers the rules from both flags, file first, and fills in
// a stand-in for every rule that named none.
//
// FILE first so a shared, reviewed list of an agent's recurring identifiers is
// applied before the one-off names an operator adds on the command line. The
// order matters when two rules' originals overlap — applyRedactions runs them
// in sequence and the earlier one wins the shared text — and a checked-in list
// is the half that should be predictable. It matters for generation too, since
// a generated token is checked against every rule already resolved.
//
// Resolving HERE rather than leaving it to Capture is what puts a length
// refusal, and a rule set too crowded to generate a unique token for, in front
// of the port-forward instead of after it. Capture resolves again; the second
// pass is a no-op on an already-resolved set.
func resolveRedactions(f captureFlags) ([]steelthread.Redaction, error) {
	var out []steelthread.Redaction
	if f.redactFrom != "" {
		content, err := os.ReadFile(f.redactFrom)
		if err != nil {
			return nil, fmt.Errorf("read --redact-from %s: %w", f.redactFrom, err)
		}
		rules, err := steelthread.ParseRedactionFile(string(content))
		if err != nil {
			return nil, fmt.Errorf("--redact-from %s: %w", f.redactFrom, err)
		}
		out = append(out, rules...)
	}
	for _, spec := range f.redact {
		r, err := steelthread.ParseRedaction(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return steelthread.ResolveRedactions(out)
}

// resolveSkillElisions parses the --elide-skill rules and fills in a stand-in
// authority for every rule that named none.
//
// No file form, unlike --redact-from. A redaction file exists because an
// agent's recurring identifiers are a list an operator keeps and reuses across
// captures; a skill elision names the repos ONE class opts into, so there is
// nothing to reuse and a file would be a second spelling of the same flag.
//
// Resolving HERE rather than leaving it to Capture is what puts a length
// refusal, and a rule set too crowded to generate a unique authority for, in
// front of the port-forward instead of after it — the same reasoning, and the
// same placement, as resolveRedactions above. Capture resolves again; the second
// pass is a no-op on an already-resolved set.
func resolveSkillElisions(f captureFlags) ([]steelthread.SkillElision, error) {
	var out []steelthread.SkillElision
	for _, spec := range f.elideSkill {
		e, err := steelthread.ParseSkillElision(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return steelthread.ResolveSkillElisions(out)
}

func runSessionCapture(cmd *cobra.Command, g *apcmd.Globals, sessionName string, f captureFlags) error {
	ctx := cmd.Context()
	stdout := cmd.OutOrStdout()
	name := f.name
	if name == "" {
		name = sessionName
	}

	// Parsed BEFORE the cluster is touched: a malformed rule should cost the
	// operator a re-run of the flag, not a port-forward, a SpiceDB dial and a
	// full records read first.
	redactions, err := resolveRedactions(f)
	if err != nil {
		return err
	}
	elisions, err := resolveSkillElisions(f)
	if err != nil {
		return err
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}

	// The deployment's own read source, unoverridden: a capture must read what
	// the session wrote, and `oap memory --backend` exists to inspect a shadow
	// backend's halves against each other, which is a different question.
	conn, err := memclient.Connect(ctx, b, sessionName, "")
	if err != nil {
		return err
	}
	defer conn.Close()

	recs, err := steelthread.RecordsFromMemory(ctx, conn.Client, conn.Scope)
	if err != nil {
		return err
	}

	fixture, err := gatherManifests(ctx, b, &sess, recs.Trigger != nil)
	if err != nil {
		return err
	}

	grants, err := gatherSessionGrants(ctx, b, fixture.Class, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	// Read the referenced Secrets' VALUES for the sole purpose of proving they
	// are absent from what gets written. RewriteFixture never sees one, so
	// nothing else in the pipeline could do this — and an empty set does not
	// pass the scan, it raises secret-check-skipped.
	liveSecrets := gatherLiveSecrets(ctx, b, &sess, fixture, cmd.ErrOrStderr())

	sdb, cleanup, err := apspicedb.NewAuthzClient(ctx, apspicedb.ClusterAuthzDialer(g))
	if err != nil {
		return fmt.Errorf("connect to SpiceDB to expand the session's permissions: %w", err)
	}
	defer cleanup()

	// Resolved ONCE and handed to both assemblies below. The live answer is what
	// the session ran on; the fixture answer is what the replay will run on, and
	// the two are only comparable if they came from the same resolution.
	liveChannel := resolveLiveChannel(ctx, b.Controller, &sess, cmd.ErrOrStderr())

	// One prediction, used twice: the tools the fixture will offer, and the
	// stand-in state it has to be seeded with to offer them. Assembled once so
	// the bundle cannot be seeded with something other than what the prediction
	// was made against.
	prediction := fixtureMetaTools(ctx, fixture, fixture.Class, &sess, liveChannel,
		conn.Client, cmd.ErrOrStderr())

	res, findings, err := steelthread.Capture(recs, steelthread.CaptureInput{
		Name:          name,
		Description:   f.desc,
		Session:       conn.Scope.ID,
		Cluster:       clusterName(g, cmd.ErrOrStderr()),
		OapVersion:    oapVersion(),
		Now:           time.Now().UTC(),
		Fixture:       fixture,
		Expand:        steelthread.NewSpiceDBExpander(sdb.Writer(steelthread.CaptureSource)),
		SessionGrants: grants,
		MetaTools: assembleMetaTools(ctx, fixture.Class, &sess, sess.Spec.InputChannel, liveChannel,
			conn.Client, cmd.ErrOrStderr()),
		FixtureTools:         prediction.Tools,
		MentionStandIn:       prediction.Mentions,
		FakeDeliverySurfaces: prediction.DeliverySurfaces,
		// The input Channel kind's own reporter, resolved through the registry
		// the runner's trigger-status tools resolve through. It is what reads
		// the provider's values back out of text that kind composed; nil for a
		// kind that reports no trigger status, which is most of them.
		TriggerProvider: triggerStatusReporterFor(&sess),
		LiveSecrets:     liveSecrets,
		Redact:          redactions,
		ElideSkills:     elisions,
	})
	if err != nil {
		return err
	}

	return reportThenWrite(stdout, f.out, res, findings, f.overwrite)
}

// reportThenWrite prints every finding and only then decides whether anything
// is written.
//
// The order is the point twice over. EVERY finding is printed before the first
// failure, because fixing five problems one command invocation at a time —
// against a live cluster, with a port-forward and a SpiceDB dial each round —
// is the slow way to learn about five problems. And NOTHING is written when a
// hard finding is present: a partially-written bundle in --output is a scenario
// that replays wrong, and the next reader has no way to tell it from one that
// was captured cleanly.
func reportThenWrite(
	stdout io.Writer, dir string, res steelthread.Result, findings []steelthread.Finding, overwrite bool,
) error {
	printRedactions(stdout, res)

	report, hardCount := steelthread.FormatFindings(findings)
	if report != "" {
		fmt.Fprint(stdout, report)
	}
	if steelthread.HasHardFinding(findings) {
		return fmt.Errorf("capture has %d blocking finding(s); nothing written", hardCount)
	}

	var opts []steelthread.WriteOption
	if overwrite {
		opts = append(opts, steelthread.Overwrite())
	}
	if err := steelthread.WriteResult(dir, res, opts...); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote bundle %q to %s (%d file(s))\n", res.Bundle.Name, dir, len(res.Emitted))
	return nil
}

// printRedactions reports what each supplied rule replaced, BEFORE the findings
// and before the write.
//
// Before, because a rule that matched NOTHING is the one an operator most needs
// to hear about and the one they are least likely to notice: they named a
// customer, believed it removed, and the capture then either refuses on some
// unrelated finding or writes a bundle still carrying the name. Printed on
// every run rather than only on the zero case, so the counts are there to
// compare against when the same list is reused on the next session.
func printRedactions(stdout io.Writer, res steelthread.Result) {
	if res.Bundle.Capture == nil || len(res.Bundle.Capture.Redactions) == 0 {
		return
	}
	for _, r := range res.Bundle.Capture.Redactions {
		if r.Count == 0 {
			fmt.Fprintf(stdout, "redaction %-24s MATCHED NOTHING — check the original for a typo; "+
				"nothing was replaced by it\n", r.Replacement)
			continue
		}
		fmt.Fprintf(stdout, "redaction %-24s replaced %d occurrence(s)\n", r.Replacement, r.Count)
	}
}

// gatherManifests reads the live CRs the fixture is rewritten from: the class,
// every MCPServer it references, its AgentIdentity, and every Channel bound to
// it.
//
// A referenced object that is MISSING is an error, not an omission. The fixture
// would still emit — RewriteFixture only requires the class — and the bundle it
// produced would boot an agent with fewer tools or no channel than the session
// had, replaying a different scenario under the captured session's name.
func gatherManifests(
	ctx context.Context, b *kube.Bundle, sess *spiceboxv1alpha1.AgentSession, triggered bool,
) (steelthread.FixtureInput, error) {
	var class spiceboxv1alpha1.AgentClass
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		return steelthread.FixtureInput{}, fmt.Errorf("get AgentClass %q (the session's class): %w", sess.Spec.Class, err)
	}
	in := steelthread.FixtureInput{Class: &class}

	for _, ref := range class.Spec.MCPServers {
		var server spiceboxv1alpha1.MCPServer
		if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref.Ref}, &server); err != nil {
			return steelthread.FixtureInput{}, fmt.Errorf("get MCPServer %q (referenced by the class as %q): %w",
				ref.Ref, ref.Name, err)
		}
		in.MCPServers = append(in.MCPServers, &server)
	}

	// Sidecar toolboxes come from the SESSION's resolved status, never from
	// class.Spec.SidecarToolboxes: the status entry carries the spec snapshot
	// the operator took at session start, so a SidecarToolbox CR edited since
	// the session ran still yields the tool allowlist the transcript was
	// produced against. See steelthread.FixtureInput.SidecarToolboxes.
	//
	// A class that DECLARES a sidecar the status never resolved is not an error
	// here. It is the ordinary shape of a session whose sidecar never came up
	// (a secret-gated toolbox whose producer never ran), and the transcript then
	// contains no call to it — so there is nothing to under-declare. A call the
	// status cannot account for is caught downstream by the self-check, which
	// sees it resolve to no declared prefix.
	in.SidecarToolboxes = append(in.SidecarToolboxes, sess.Status.ResolvedSidecarToolboxes...)

	if err := gatherSandboxCRs(ctx, b, &class, &in); err != nil {
		return steelthread.FixtureInput{}, err
	}

	if err := gatherSkills(ctx, b, &class, &in); err != nil {
		return steelthread.FixtureInput{}, err
	}

	if class.Spec.AgentIdentity != "" {
		var identity spiceboxv1alpha1.AgentIdentity
		if err := b.Controller.Get(ctx, client.ObjectKey{
			Namespace: b.Namespace, Name: class.Spec.AgentIdentity,
		}, &identity); err != nil {
			return steelthread.FixtureInput{}, fmt.Errorf("get AgentIdentity %q: %w", class.Spec.AgentIdentity, err)
		}
		in.Identity = &identity
	}

	// The session's own SessionUserIdentity, present exactly when the run
	// resolved to identityMode userPassthrough — the operator names it after
	// the AgentSession and owns it. Read like the resolved sidecar toolboxes
	// above and for the same reason: it holds the credentials THIS run
	// resolved, while the live user's UserIdentity holds everything that person
	// has ever linked.
	//
	// A NotFound is the ordinary agent-identity session, not an omission: the
	// CR exists only for a passthrough run. Any other error is returned — a
	// passthrough capture that silently skipped it would emit a fixture whose
	// replayed session parks in AwaitingCredentials forever.
	var suid spiceboxv1alpha1.SessionUserIdentity
	switch err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: sess.Namespace, Name: sess.Name,
	}, &suid); {
	case err == nil:
		in.SessionUserIdentity = &suid
	case !apierrors.IsNotFound(err):
		return steelthread.FixtureInput{}, fmt.Errorf(
			"get SessionUserIdentity %q (the session's resolved passthrough catalog): %w", sess.Name, err)
	}

	var channels spiceboxv1alpha1.ChannelList
	if err := b.Controller.List(ctx, &channels, client.InNamespace(b.Namespace)); err != nil {
		return steelthread.FixtureInput{}, fmt.Errorf("list Channels: %w", err)
	}
	for i := range channels.Items {
		if channels.Items[i].Spec.AgentClass == class.Name {
			in.Channels = append(in.Channels, &channels.Items[i])
		}
	}

	// Only a session that actually opened on a delivery names a trigger
	// Channel. Naming one for a session a person typed into would keep that
	// Channel's real kind in the fixture — the ONE thing RewriteFixture does
	// not rewrite — for a bundle with no delivery to sign.
	if triggered && sess.Spec.InputChannel != nil {
		in.TriggerChannel = sess.Spec.InputChannel.Name
	}
	return in, nil
}

// gatherSessionGrants reads the AgentSessionGrants CR the session's AgentClass
// owns — the class's own declaration of its grant pairs and its authz SLOTS.
//
// Found by OWNER REFERENCE rather than by rebuilding the "<class>-grants" name
// the AgentClass reconciler composes. That name is written in exactly one place
// (pkg/controllers/agentclass writeAgentSessionGrants); spelling it a second
// time here would make a rename silently return NotFound, and NotFound is the
// permissive-looking answer — the capture would go on believing the class
// declares no slots. status.agentSessionGrantsRef looks like the right handle
// and is not: nothing writes it.
//
// NOT part of gatherManifests, and not on FixtureInput: every manifest gathered
// there is one RewriteFixture emits into the bundle, and this CR must not be
// emitted. See steelthread.CaptureInput.SessionGrants.
//
// A class with no CR yields nil, WARNED to stderr rather than returned as an
// error. Nil is the strict answer (no slot is exempt, every unseeded allow
// still hard-fails), so it can only cost a refusal, never a wrong bundle — but
// it is also the shape an unreconciled class has, and a refusal whose real
// cause is a missing CR is worth a line at the moment it is noticed rather than
// a puzzle later.
func gatherSessionGrants(
	ctx context.Context, b *kube.Bundle, class *spiceboxv1alpha1.AgentClass, stderr io.Writer,
) (*spiceboxv1alpha1.AgentSessionGrants, error) {
	var list spiceboxv1alpha1.AgentSessionGrantsList
	if err := b.Controller.List(ctx, &list, client.InNamespace(class.Namespace)); err != nil {
		return nil, fmt.Errorf("list AgentSessionGrants in %s (the class's own grant declaration): %w",
			class.Namespace, err)
	}
	for i := range list.Items {
		owner := metav1.GetControllerOf(&list.Items[i])
		if owner != nil && owner.Kind == "AgentClass" && owner.Name == class.Name {
			return &list.Items[i], nil
		}
	}
	fmt.Fprintf(stderr, "warning: AgentClass %q owns no AgentSessionGrants, so no declared authz slot could be "+
		"consulted; an allowed permission held only by a slot binding collected at session end will be "+
		"reported as unseeded\n", class.Name)
	return nil, nil
}

// gatherSandboxCRs follows the class's spec.toolBundles to the three CRs a
// SANDBOX tool is synthesized from: the SpiceboxClass naming the tool catalog,
// each SpiceboxToolspec narrowing it, and each toolspec's SpiceboxToolkit.
//
// All three are CLUSTER-scoped, so none is looked up in the session's namespace.
//
// A missing SpiceboxClass or SpiceboxToolspec is an ERROR, not a skip. The
// session demonstrably ran tools synthesized from them, and a fixture emitted
// without them boots an agent whose class offers no sandbox tool at all — so
// the scripted call comes back "unknown tool" minutes later, naming the tool
// and saying nothing about the manifest that was never written.
//
// A missing SpiceboxToolkit is an error only when the toolspec's toolkit is not
// a BUILT-IN. Built-ins are compiled into the binary and resolve at replay with
// no CR at all (test/e2e's resolveToolkit checks them first), so requiring a CR
// for one would refuse every capture of an agent using the shipped gh or git
// toolkits. Matched on (name, revision) exactly as that resolution does, rather
// than on name alone: a toolspec pinned to a revision the built-in does not
// carry needs the CR that does.
func gatherSandboxCRs(
	ctx context.Context, b *kube.Bundle, class *spiceboxv1alpha1.AgentClass, in *steelthread.FixtureInput,
) error {
	seenClass := map[string]bool{}
	seenSpec := map[string]bool{}
	seenToolkit := map[string]bool{}

	for _, bundle := range class.Spec.ToolBundles {
		if bundle.Class != "" && !seenClass[bundle.Class] {
			seenClass[bundle.Class] = true
			var cls spiceboxv1alpha1.SpiceboxClass
			if err := b.Controller.Get(ctx, client.ObjectKey{Name: bundle.Class}, &cls); err != nil {
				return fmt.Errorf("get SpiceboxClass %q (toolBundle %q's sandbox class): %w",
					bundle.Class, bundle.Name, err)
			}
			in.SandboxClasses = append(in.SandboxClasses, &cls)
		}

		for _, tsName := range bundle.Toolspecs {
			if tsName == "" || seenSpec[tsName] {
				continue
			}
			seenSpec[tsName] = true
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := b.Controller.Get(ctx, client.ObjectKey{Name: tsName}, &ts); err != nil {
				return fmt.Errorf("get SpiceboxToolspec %q (listed by toolBundle %q): %w",
					tsName, bundle.Name, err)
			}
			in.Toolspecs = append(in.Toolspecs, &ts)

			tk := ts.Spec.Toolkit
			if tk.Name == "" || seenToolkit[tk.Name] || isBuiltinToolkit(tk.Name, tk.Revision) {
				continue
			}
			seenToolkit[tk.Name] = true
			var toolkitCR spiceboxv1alpha1.SpiceboxToolkit
			if err := b.Controller.Get(ctx, client.ObjectKey{Name: tk.Name}, &toolkitCR); err != nil {
				return fmt.Errorf("get SpiceboxToolkit %q revision %q (named by SpiceboxToolspec %q, and not "+
					"a built-in): %w", tk.Name, tk.Revision, tsName, err)
			}
			in.Toolkits = append(in.Toolkits, &toolkitCR)
		}
	}
	return nil
}

// gatherSkills follows the class's spec.skills to the Skill CRs that carry each
// opted-in canonical name, and to the SkillSource that owns each of those.
//
// Both halves are required, and the pairing is not a convenience: a replayed
// Skill only reaches Valid=True when its provenance gate passes, and for a
// git-authority canonical name that gate demands a controller owner-ref to a
// same-namespace SkillSource that EXISTS and whose repoURL matches the name's
// authority (pkg/tools/skills/materialize). Emitting the Skill alone would swap
// one park for another. See steelthread.rewriteSkills.
//
// A ref that resolves to no namespaced Skill is NOT an error here, and that is
// the one place this differs from gatherSandboxCRs. Two live shapes produce it
// — a skill backed by a cluster-scoped ClusterSkill, which this does not gather,
// and a Skill deleted or renamed since the class was validated — and neither is
// improved by aborting mid-gather with a bare Get error. The self-check reports
// every unresolved ref together, by name, as CodeSkillsNotCaptured, so the
// operator sees the whole gap in one refusal instead of one ref per re-run.
//
// A LIST failure IS returned: it says nothing about the class and would
// otherwise be reported as though every skill were missing.
func gatherSkills(
	ctx context.Context, b *kube.Bundle, class *spiceboxv1alpha1.AgentClass, in *steelthread.FixtureInput,
) error {
	if len(class.Spec.Skills) == 0 {
		return nil
	}

	var list spiceboxv1alpha1.SkillList
	if err := b.Controller.List(ctx, &list, client.InNamespace(b.Namespace)); err != nil {
		return fmt.Errorf("list Skills in namespace %s (the class opts into %d): %w",
			b.Namespace, len(class.Spec.Skills), err)
	}
	byCanonical := make(map[string]*spiceboxv1alpha1.Skill, len(list.Items))
	for i := range list.Items {
		byCanonical[list.Items[i].Spec.CanonicalName] = &list.Items[i]
	}

	seenSkill := map[string]bool{}
	seenSource := map[string]bool{}
	for _, want := range class.Spec.Skills {
		sk, ok := byCanonical[want.Ref]
		if !ok || seenSkill[sk.Name] {
			continue
		}
		seenSkill[sk.Name] = true
		in.Skills = append(in.Skills, sk)

		// The owning SkillSource, when there is one. A hand-authored local//
		// skill legitimately has no owner and needs none — its provenance gate
		// passes on the reserved authority alone.
		//
		// A NotFound here IS an error: the live Skill is Valid=True (the class
		// referencing it is, or the session could not have spawned), which
		// means this source existed when that verdict was reached. Emitting the
		// Skill with an owner-ref to a source the fixture cannot write would
		// produce a bundle that parks at Valid=False/InvalidSpec.
		srcName := controllerSkillSourceName(sk)
		if srcName == "" || seenSource[srcName] {
			continue
		}
		seenSource[srcName] = true
		var src spiceboxv1alpha1.SkillSource
		if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: srcName}, &src); err != nil {
			return fmt.Errorf("get SkillSource %q (the controller owner of Skill %q, which carries the "+
				"class's skill %q — its existence is what the skill's provenance gate checks): %w",
				srcName, sk.Name, want.Ref, err)
		}
		in.SkillSources = append(in.SkillSources, &src)
	}
	return nil
}

// controllerSkillSourceName is the name of the SkillSource that controls sk, or
// "" when it has none. Matched on exactly the fields pkg/tools/skills/materialize
// matches on, so a name this returns is a name that gate will look up.
func controllerSkillSourceName(sk *spiceboxv1alpha1.Skill) string {
	for _, ref := range sk.OwnerReferences {
		if ref.Kind == "SkillSource" &&
			ref.APIVersion == spiceboxv1alpha1.SchemeGroupVersion.String() &&
			ref.Controller != nil && *ref.Controller {
			return ref.Name
		}
	}
	return ""
}

// isBuiltinToolkit reports whether (name, revision) names a toolkit compiled
// into the binary. Mirrors the resolution order a replay performs: built-ins
// win, and only a non-built-in needs a CR emitted alongside the fixture.
func isBuiltinToolkit(name, revision string) bool {
	for _, tk := range toolkits.All() {
		if tk.Name == name && tk.ToolkitRevision == revision {
			return true
		}
	}
	return false
}

// secretSource is one Secret the capture must read the value of, and how it
// learned the Secret exists. See gatherLiveSecrets on why the distinction
// changes only how a NotFound is reported.
type secretSource struct {
	Key client.ObjectKey
	// Referenced is true when a gathered manifest or a resolved status field
	// POINTS AT this Secret by name. A DERIVED source (a per-session Secret
	// whose name follows from the session's own) may legitimately not exist:
	// a session that produced no secret outputs has no <name>-secret-outputs.
	Referenced bool
}

// gatherLiveSecrets reads the value of every Secret this session's credentials
// could have come from, and returns one entry per non-empty data key.
//
// # Why the RESOLVED status, not just the spec
//
// The class's spec is not where a session's credentials necessarily are. An
// AgentClass whose spec.model is absent entirely still runs against a model,
// with a real API key: the 4-tier settings resolver picks both and stamps the
// answer on AgentSession.status.effectiveSettings, and the operator
// materializes the token into a per-session Secret. Reading only the spec
// therefore gathers NOTHING for such a session, the leak scan runs with nothing
// to look for, and a key echoed by an upstream tool into the transcript is
// written into the repo with the capture reporting clean.
//
// This is the third time this capture has had to learn the same lesson —
// PlanGateActive and the sidecar prefixes were the first two. The spec value can
// be stale, unset or unclamped; the resolved status is what the run consulted.
//
// # Derived, not transcribed
//
// The per-session Secrets come from v1alpha1.PerSessionSecretSuffixes, which is
// the canonical list every owner of such a Secret is required to register in, so
// a per-session Secret kind added later is read here without this function being
// edited. The sidecar credential Secret comes from cosidecar.CredentialSecretName,
// the same function the AgentSession reconciler builds it with. The identity
// credentials dispatch through the credkind registry rather than switching on
// cred.Type.
//
// # Which keys are actually secret
//
// Reading a value out of a Secret says where it came from, not what it is. A
// GitHub App's Secret holds a private key and a webhook secret beside an app id
// and an installation id, and GitHub publishes both of the latter — the
// installation id arrives in the body of every webhook delivery, so a capture
// that treats Secret membership as secrecy refuses every triggered GitHub
// session forever, over a value the delivery record cannot omit.
//
// So each entry is marked Public or not, and the answer comes from the type
// that OWNS the Secret's shape: credkind/registry.PublicSecretKeysFor for an
// identity credential, channelkinds/registry.PublicSecretKeysFor for a
// Channel's credentials Secret. Both halves are asked because one Secret is
// named by both. Nothing here knows a key name, and nothing here needs editing
// when a credential type or a channel kind is added — the same property the
// SecretNameFor dispatch above has.
//
// FAIL CLOSED at every step: an unregistered type errors and declares nothing,
// a type that declares nothing declares nothing, and an unmarked key is
// scanned. The declaration is an opt-out for named public keys, never an opt-in
// for protection.
//
// # Reporting
//
// A Secret that cannot be read is WARNED about, never silently skipped: a
// missing value is a value the leak scan cannot look for. The one exception is
// a NotFound on a DERIVED name, which is not a failure to read — the Secret was
// never created, so it holds nothing that could have leaked. A referenced name
// that is NotFound still warns: something pointed at it.
//
// An empty data value is dropped: "" is a substring of every file, and
// steelthread.LiveSecret documents that such an entry does not count as
// scannable.
func gatherLiveSecrets(
	ctx context.Context, b *kube.Bundle, sess *spiceboxv1alpha1.AgentSession,
	in steelthread.FixtureInput, warn io.Writer,
) []steelthread.LiveSecret {
	ns := sess.Namespace
	if ns == "" {
		ns = b.Namespace
	}

	sources := map[client.ObjectKey]bool{} // key -> referenced
	add := func(namespace, name string, referenced bool) {
		if name == "" {
			return
		}
		if namespace == "" {
			namespace = ns
		}
		k := client.ObjectKey{Namespace: namespace, Name: name}
		sources[k] = sources[k] || referenced
	}

	// public[secret][dataKey] records that the type owning that Secret's SHAPE
	// declared the key a public identifier. Nothing is entered here by default,
	// and an absent entry means "secret" — see steelthread.LiveSecret.Public.
	//
	// Unioned across declarers, deliberately. One Secret is named by both
	// registries: a github Channel's credentials Secret is the very object a
	// type=githubApp credential points at, and either reference may be present
	// without the other. A referencer that declares nothing has said nothing —
	// it neither grants publicness nor vetoes another declarer's answer about
	// the same key. Making silence a veto would put us back where we started,
	// since every githubApp Secret is also some github Channel's.
	public := map[client.ObjectKey]map[string]bool{}
	addPublic := func(namespace, name string, keys []string) {
		if name == "" || len(keys) == 0 {
			return
		}
		if namespace == "" {
			namespace = ns
		}
		k := client.ObjectKey{Namespace: namespace, Name: name}
		if public[k] == nil {
			public[k] = map[string]bool{}
		}
		for _, dataKey := range keys {
			public[k][dataKey] = true
		}
	}

	// The deterministic per-session Secrets, derived from the canonical suffix
	// list rather than named here. Between them they hold the resolved model
	// API key, the memory bearer token, the audit-signing seed, the args-hash
	// HMAC key, the projected userPassthrough credentials a toolBundle's
	// credentialRemap draws on, and any captured tool secret outputs.
	for _, suffix := range spiceboxv1alpha1.PerSessionSecretSuffixes {
		add(ns, sess.Name+suffix, false)
	}

	// The resolved model credential and the central token it was materialized
	// from. Both are references the run really used, and the token source is
	// the one Secret here that lives in another namespace.
	if es := sess.Status.EffectiveSettings; es != nil {
		add(ns, es.Model.APIKey.Name, true)
		if ts := es.ModelTokenSource; ts != nil {
			add(ts.Namespace, ts.Name, true)
		}
	}

	// Each sidecar toolbox's materialized upstream credentials, from the
	// session's RESOLVED sidecars — the same list the fixture rewrite reads.
	for _, rt := range sess.Status.ResolvedSidecarToolboxes {
		add(ns, cosidecar.CredentialSecretName(sess.Name, rt.Ref), false)
	}

	if in.Class != nil && in.Class.Spec.Model != nil {
		add(ns, in.Class.Spec.Model.APIKey.Name, true)
	}
	for _, ch := range in.Channels {
		add(ns, ch.Spec.CredentialsRef.SecretName, true)
		// Dispatched through the channel-kind registry rather than switched on
		// ch.Spec.Kind. A kind that declares nothing, and an unknown kind that
		// errors, are the same fail-closed answer — everything in that Secret
		// stays secret — so the error is reported and the loop continues.
		keys, err := channelregistry.PublicSecretKeysFor(ch)
		if err != nil {
			fmt.Fprintf(warn, "warning: Channel %q: cannot ask its kind which Secret keys are public "+
				"identifiers (%v); every key of %q will be treated as secret\n",
				ch.Name, err, ch.Spec.CredentialsRef.SecretName)
			continue
		}
		addPublic(ns, ch.Spec.CredentialsRef.SecretName, keys)
	}
	if in.Identity != nil {
		for _, cred := range in.Identity.Spec.Credentials {
			// Dispatched through the credkind registry rather than switched on
			// cred.Type, so a credential type added later is read here without
			// this function being edited.
			secret, err := credkindregistry.SecretNameFor(cred)
			if err != nil {
				fmt.Fprintf(warn, "warning: credential %q: cannot resolve its Secret name (%v); "+
					"its value will not be scanned for\n", cred.Name, err)
				continue
			}
			add(ns, secret, true)

			// The same registry, for the neighbouring question: which of that
			// Secret's keys does this credential type call a PUBLIC identifier?
			// An error here is the same wiring bug SecretNameFor reports and
			// gets the same fail-closed treatment — warn, declare nothing, and
			// let every key of that Secret be scanned as if it were material.
			keys, err := credkindregistry.PublicSecretKeysFor(cred)
			if err != nil {
				fmt.Fprintf(warn, "warning: credential %q: cannot ask its type which Secret keys are "+
					"public identifiers (%v); every key of %q will be treated as secret\n",
					cred.Name, err, secret)
				continue
			}
			addPublic(ns, secret, keys)
		}
	}

	keys := slices.SortedFunc(maps.Keys(sources), func(a, b client.ObjectKey) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})

	var out []steelthread.LiveSecret
	for _, key := range keys {
		var sec corev1.Secret
		if err := b.Controller.Get(ctx, key, &sec); err != nil {
			if apierrors.IsNotFound(err) && !sources[key] {
				// A derived per-session Secret that was never created holds
				// nothing that could have leaked. Not a failure to read.
				continue
			}
			fmt.Fprintf(warn, "warning: Secret %q could not be read (%v); its value will not be "+
				"scanned for in the emitted fixture\n", key.Name, err)
			continue
		}
		var scannable int
		for _, dataKey := range slices.Sorted(maps.Keys(sec.Data)) {
			if len(sec.Data[dataKey]) == 0 {
				continue
			}
			scannable++
			out = append(out, steelthread.LiveSecret{
				Name:   key.Name + "/" + dataKey,
				Value:  string(sec.Data[dataKey]),
				Public: public[key][dataKey],
			})
		}
		if scannable == 0 {
			// The Secret exists and holds nothing. Recorded, because "nobody
			// read it" and "it was read and was empty" are different answers to
			// the leak scan's gate and only the first is a failure. Routine on a
			// live cluster: a browser-started session's Channel credentials
			// Secret is created EMPTY, since the browser surface needs no
			// credential, and the fixture still emits a placeholder for it.
			out = append(out, steelthread.LiveSecret{Name: key.Name, EmptyRead: true})
		}
	}
	return out
}

// boundChannel is the resolved-channel half of capability.RunnerEnv: the
// Channel, its credentials Secret, its registered Kind, and the error that
// tells the channel-sourced capabilities not to dereference the first.
//
// It exists so the SAME assembly can be driven against the live cluster and
// against the fixture the capture is about to emit. Those two answers are what
// the self-check compares; a second assembly written separately for the fixture
// could differ from the live one for reasons that have nothing to do with the
// rewrite, which is the one thing the comparison must not confuse.
type boundChannel struct {
	Channel *spiceboxv1alpha1.Channel
	Secret  *corev1.Secret
	Kind    channelkinds.Kind
	Err     error
}

// resolveLiveChannel resolves the session's bound Channel off the cluster,
// through the same resolve.ForSession the runner calls once at startup — so
// this answers with the kind the session actually ran on rather than a second
// resolution free to disagree with it.
//
// A failure is WARNED, never swallowed: MetaTools is fail-closed, so a short
// assembly refuses the capture with a finding that names a tool when the truth
// is that the Channel could not be read.
func resolveLiveChannel(
	ctx context.Context, cli client.Client, sess *spiceboxv1alpha1.AgentSession, warn io.Writer,
) boundChannel {
	if sess.Spec.InputChannel == nil {
		return boundChannel{}
	}
	var out boundChannel
	if cli == nil {
		out.Err = fmt.Errorf("no cluster client available to resolve the bound Channel")
	} else {
		ch, sec, kind, err := resolve.ForSession(ctx, cli, sess)
		out.Channel, out.Secret, out.Kind, out.Err = ch, sec, kind, err
		if err == nil && (ch == nil || kind == nil) {
			out.Err = fmt.Errorf("the bound Channel resolved to no channel or no kind")
		}
	}
	if out.Err != nil {
		fmt.Fprintf(warn, "warning: could not resolve the session's bound Channel %q (%v); "+
			"the channel-sourced meta tools will be missing from the capture's tool list, and a "+
			"transcript that called one is refused as a sandbox tool call naming that tool\n",
			sess.Spec.InputChannel.Name, out.Err)
	}
	return out
}

// assembleMetaTools answers "which meta tools does this class offer on this
// binding?" through capability.Assemble — the same seam internal/cmd/runner and
// the e2e in-process factory both drive.
//
// Assembled rather than listed. Which meta tools a session gets is decided by
// its capabilities, so a list written here would be a snapshot that drifts
// silently the first time a capability gains a tool, and every call to the new
// tool would then read to the self-check as an unreplayable sandbox call.
//
// The availability booleans are set TRUE because the question is what the CLASS
// was offered, not what this CLI process happens to have wired: a capture run
// from a laptop has no knowledge-graph endpoint and no NATS connection, and
// answering "no" on those grounds would drop query_knowledge from a session
// that genuinely had it. The grants themselves still gate everything — an
// ungranted capability contributes nothing however available its backend is.
//
// binding is passed separately from sess rather than read off it because the
// fixture assembly has to vary it: it binds to the Channel as the REWRITE will
// emit it, which is not the kind the session recorded.
//
// # Why this function is never silent
//
// MetaTools is fail-closed: a name missing from it turns every call to that
// tool into a hard sandbox-tool-call finding. So every way the assembly can
// come back SHORT has to reach the operator, or the capture is refused with a
// finding that names the wrong thing entirely — "read_channel_history has no
// route back into a replay" when the truth is that the bound Channel could not
// be read. Two sinks, both writing to warn:
//
//   - a resolve failure, warned about by resolveLiveChannel;
//   - every capability that was granted and declined, through the Logger
//     capability.Assemble already calls logSkip on. The runner logs those; a
//     discard logger here would throw away the one explanation the operator
//     gets, including the agent_ui_handoff decline that an assembly-only site
//     always takes (see viewerCanInteractWiring in the capability package).
//
// The one caller that DOES discard those lines is externalSurfaceTools, which
// re-offers capabilities this assembly has already offered and logged.
func assembleMetaTools(
	ctx context.Context,
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	binding *spiceboxv1alpha1.ChannelBinding,
	ch boundChannel,
	mem memory.Memory,
	warn io.Writer,
) []string {
	tools := capability.Assemble(ctx, metaToolDeps(class, sess, binding, ch, mem, warn))
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

// metaToolDeps builds the assembly input assembleMetaTools drives.
//
// Factored out because externalSurfaceTools has to ask the capability registry
// a second question about the SAME assembly, and the only honest way to ask it
// is with byte-identical deps: a separately-built RunnerEnv would let the two
// answers disagree about which tools this session was offered, and the
// disagreement would reach the bundle as a tool the replay believes it may
// call.
func metaToolDeps(
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	binding *spiceboxv1alpha1.ChannelBinding,
	ch boundChannel,
	mem memory.Memory,
	warn io.Writer,
) capability.AssembleDeps {
	env := capability.RunnerEnv{
		ChannelAttached: binding != nil,
		MemoryAvailable: true,
		SearchAvailable: true,
		KGAvailable:     true,
		// The artifacts capability refuses to offer its tools without a
		// service, and would otherwise skip on a session that had them.
		// Constructed over the same memory the records came from; no tool is
		// ever executed here, only offered.
		Artifacts: artifacts.NewService(mem, nil),
		// fetch_artifact exists whenever a reader does, and the replay driver
		// wires one for EVERY bundle (threadrun's unconditional
		// SetArtifactStore). Left nil, this assembly answers "the fixture will
		// not offer fetch_artifact" about a replay that certainly will —
		// which is a hard finding on a session with nothing wrong with it.
		//
		// Empty and in-memory for the reason Artifacts above is constructed
		// here at all: the question is what the class was OFFERED, no tool is
		// ever executed, and a laptop running a capture has no artifact store
		// of its own to answer with.
		ArtifactReader: files.StoreReader{Store: blobstore.NewMem()},
		// Gates select_phase and complete_phase, and read from the SAME place
		// the runner reads it — the session's resolved settings, not the class
		// spec, which is unclamped by the tier floors.
		//
		// Unset, this field is false, and false is silent: the two tools drop
		// out of the assembled list, and because MetaTools is fail-closed every
		// select_phase call in a captured transcript then reads as a sandbox
		// tool the bundle cannot replay. That refused every plan-gated session
		// — the sessions this feature most exists to capture — with a finding
		// naming the wrong thing entirely.
		PlanGateActive: sess.Status.EffectiveSettings.PlanGateActive(),
	}
	if env.ChannelAttached {
		// The channel-sourced capabilities read the RESOLVED Channel, Secret
		// and kind, and two of them dereference the Channel without a nil check
		// — ResolveErr is the field that tells them not to.
		env.ResolvedChannel, env.ResolvedSecret, env.ResolvedKind, env.ResolveErr = ch.Channel, ch.Secret, ch.Kind, ch.Err
	}

	return capability.AssembleDeps{
		Class:   class,
		Session: sess,
		Binding: binding,
		Env:     env,
		Logger:  warnLogger(warn),
	}
}

// fixtureMetaTools answers "which meta tools will the EMITTED fixture offer?" —
// the same assembly metaToolNames drives, run a second time against the Channel,
// Secret and kind steelthread.ReplayChannel returns instead of the live ones.
//
// It exists because the fixture rewrite replaces the collaborator the
// channel-sourced capabilities are assembled against: every non-trigger Channel
// becomes kind=fake, and a meta tool the live kind contributed has no fake
// counterpart. Nothing in the transcript records that; the recorded tool catalog
// says what the run WAS offered, and only a second assembly can say what the
// replay WILL be offered. See steelthread.SelfCheckInput.FixtureTools.
//
// Returns a zero prediction — which the self-check refuses on rather than reads
// as "the fixture offers nothing" — whenever it cannot be made. Every such path
// warns first.
//
// # The seeded stand-in, and why the prediction is made WITH it
//
// A directory the rewrite took away is put back by seeding the fake kind with
// what the live kind advertised, so the prediction has to be made with that seed
// in place or it would refuse a tool the replay really will offer. The seed is
// enabled around BOTH the prediction and the difference below, and restored
// immediately: it is process-wide state a serving channelsd would share.
//
// Which tools the seed put back is computed by DIFFERENCE, the same way the
// provider-surface set is: assemble once seeded, once not, and whatever appears
// was restored by the seed. Those are the tools a bundle may be OFFERED and must
// not have CALLED, because only their offer is reproduced.
func fixtureMetaTools(
	ctx context.Context,
	fixture steelthread.FixtureInput,
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	live boundChannel,
	mem memory.Memory,
	warn io.Writer,
) fixturePrediction {
	if sess.Spec.InputChannel == nil {
		// Not channel-attached: no channel-sourced capability contributes
		// anything to either assembly, so the fixture offers exactly what the
		// live run was offered.
		return fixturePrediction{
			Tools: asFixtureTools(assembleMetaTools(ctx, class, sess, nil, boundChannel{}, mem, warn), nil, nil),
		}
	}
	if live.Err != nil || live.Channel == nil {
		// resolveLiveChannel already warned. There is no live kind to rewrite
		// FROM, so no honest prediction to make.
		return fixturePrediction{}
	}

	// The channel the replay's runner will BIND to, and the channel it will
	// RESOLVE for its outbound surface, are not necessarily the same one — this
	// session's input is the trigger and its output is a chat thread. Both are
	// taken through the rewrite that will actually emit them.
	inCh, _, err := steelthread.ReplayChannel(fixture, sess.Spec.InputChannel.Name)
	if err != nil {
		fmt.Fprintf(warn, "warning: could not work out what the fixture's input Channel will be (%v); "+
			"the capture cannot compare the recorded tool catalog against what the fixture will offer\n", err)
		return fixturePrediction{}
	}
	outCh, outSec, err := steelthread.ReplayChannel(fixture, live.Channel.Name)
	if err != nil {
		fmt.Fprintf(warn, "warning: could not work out what the fixture's bound Channel will be (%v); "+
			"the capture cannot compare the recorded tool catalog against what the fixture will offer\n", err)
		return fixturePrediction{}
	}
	outKind, ok := channelregistry.Get(outCh.Spec.Kind)
	if !ok {
		fmt.Fprintf(warn, "warning: the fixture's bound Channel declares kind %q, which is not registered in "+
			"this binary; the capture cannot compare the recorded tool catalog against what the fixture will "+
			"offer\n", outCh.Spec.Kind)
		return fixturePrediction{}
	}

	binding := sess.Spec.InputChannel.DeepCopy()
	binding.Kind = inCh.Spec.Kind
	replay := boundChannel{Channel: outCh, Secret: outSec, Kind: outKind}

	// What the fixture offers with NO stand-in seeded — the baseline the
	// difference below is taken against, and what every capture emitted before
	// the seed existed.
	bare := assembleMetaTools(ctx, class, sess, binding, replay, mem, io.Discard)

	mentions := mentionStandInFor(live)
	if mentions == nil {
		names := bare
		// Re-assembled through warn so the operator still sees every granted
		// capability that declined; the run above discarded them to keep the
		// difference from reporting the same skips twice.
		names = assembleMetaTools(ctx, class, sess, binding, replay, mem, warn)
		return fixturePrediction{
			Tools: asFixtureTools(names, externalSurfaceTools(ctx, class, sess, binding, replay, names, mem), nil),
		}
	}

	restore := fakekind.EnableMentionLookups(mentionLookupKinds(mentions), nil)
	defer restore()

	names := assembleMetaTools(ctx, class, sess, binding, replay, mem, warn)
	stoodIn := addedBy(bare, names)
	if len(stoodIn) == 0 {
		// The seed changed nothing: the class never granted the capability, or
		// the fixture's bound kind was not the one the seed applies to. Emitting
		// it anyway would put state in the bundle that the replay would seed and
		// nothing would read — and the driver's own honesty check would then be
		// asserting about a surface no tool came from.
		mentions = nil
	}
	return fixturePrediction{
		Tools:            asFixtureTools(names, externalSurfaceTools(ctx, class, sess, binding, replay, names, mem), stoodIn),
		Mentions:         mentions,
		DeliverySurfaces: needsFakeDeliverySurfaces(live, outKind),
	}
}

// needsFakeDeliverySurfaces reports whether the emitted fixture has to turn the
// fake kind's artifact-delivery surfaces on.
//
// The third thing the rewrite to kind=fake takes away, and the same shape as the
// mention directory: the live kind advertised asset capabilities, the fake kind
// advertises none by default, and respond_to_user hides its `attached` field
// behind one. A run that delivered an artifact replays into
// "`attached` is not supported on this channel" — a refusal from OUR code, about
// a capability the recorded channel really had.
//
// Derived by comparing the two kinds' own Capabilities, never from a list here.
// bt.FakeDeliverySurfaces already exists as the opt-in a hand-authored bundle
// uses for exactly this; the capture just has to notice it is needed.
//
// False when the fixture's bound kind is not the fake one — a trigger Channel
// keeps its own kind and loses nothing — and false when the live kind advertised
// no asset capability either, which is most conversational sessions.
func needsFakeDeliverySurfaces(live boundChannel, replayKind channelkinds.Kind) bool {
	if live.Kind == nil || replayKind == nil {
		return false
	}
	if hasAssetCapability(replayKind.Capabilities()) {
		return false
	}
	return hasAssetCapability(live.Kind.Capabilities())
}

// hasAssetCapability reports whether any capability names an asset MIME type —
// the class of capability respond_to_user's `attached` field is gated on.
func hasAssetCapability(caps []string) bool {
	for _, c := range caps {
		if strings.HasPrefix(c, "asset:") {
			return true
		}
	}
	return false
}

// fixturePrediction is what one run of the fixture assembly concluded: the tools
// the emitted fixture will offer, and the stand-in state it has to be seeded
// with to offer them.
//
// The two travel together because they are one answer. A bundle seeded with
// something other than what the prediction was made against would offer a tool
// set the capture never predicted, and the catalog check would then fail on a
// difference nobody introduced.
type fixturePrediction struct {
	Tools    []steelthread.FixtureTool
	Mentions *bt.MentionStandIn
	// DeliverySurfaces is bt.Bundle.FakeDeliverySurfaces: the fake kind's
	// artifact-delivery capabilities, which a rewritten Channel needs when the
	// live one advertised assets.
	DeliverySurfaces bool
}

// mentionStandInFor is the directory the fixture's fake kind will be seeded
// with, or nil when the live bound kind advertised none.
//
// Lookups come from the LIVE kind, which is the whole point: the seed restores
// what the rewrite took away, so the recorded channel's own advertisement is the
// only defensible source for it. A list written here would be this capture's
// opinion about what a channel offers.
//
// Users is deliberately EMPTY, and that is not a gap. A directory entry is
// (value → provider id), and the provider id reaches a record only inside the
// tool's reply — which is the stand-in kind's own rendering, in its own dialect,
// and therefore not reproducible anyway. So a run that CALLED the tool is
// refused outright (CodeStoodInToolCalled) rather than served a guessed entry,
// and a run that was merely offered it needs no entry at all.
func mentionStandInFor(live boundChannel) *bt.MentionStandIn {
	if live.Kind == nil {
		return nil
	}
	lookups := live.Kind.SupportedMentionLookups()
	if len(lookups) == 0 {
		return nil
	}
	out := &bt.MentionStandIn{}
	for _, l := range lookups {
		out.Lookups = append(out.Lookups, string(l))
	}
	return out
}

// mentionLookupKinds converts the bundle's plain strings back into the channel
// kind's own type. The bundle format holds strings so no consumer of it drags a
// channel-kind import along; the conversion happens at each end.
func mentionLookupKinds(m *bt.MentionStandIn) []channelkinds.MentionLookupKind {
	if m == nil {
		return nil
	}
	out := make([]channelkinds.MentionLookupKind, 0, len(m.Lookups))
	for _, l := range m.Lookups {
		out = append(out, channelkinds.MentionLookupKind(l))
	}
	return out
}

// addedBy is the set of names in with that are not in without.
func addedBy(without, with []string) map[string]bool {
	had := make(map[string]bool, len(without))
	for _, n := range without {
		had[n] = true
	}
	out := map[string]bool{}
	for _, n := range with {
		if !had[n] {
			out[n] = true
		}
	}
	return out
}

// triggerStatusReporterFor is the input binding kind's own trigger-status
// reporter, or nil.
//
// Resolved through channelregistry.TriggerStatusReporterFor — the one lookup
// the runner's own trigger-status tools go through, and the same one
// externalSurfaceTools gates its difference on. Two routes to a kind would let
// the capture extract a provider's values with one kind's rules while deciding
// which tools reach a provider with another's.
func triggerStatusReporterFor(sess *spiceboxv1alpha1.AgentSession) channelkinds.TriggerStatusReporter {
	if sess.Spec.InputChannel == nil {
		return nil
	}
	r, ok := channelregistry.TriggerStatusReporterFor(sess.Spec.InputChannel.Kind)
	if !ok {
		return nil
	}
	return r
}

// externalSurfaceTools is the subset of names that act on a THIRD PARTY's own
// surface — for a pull-request trigger, the tools that claim and conclude the
// check run on the pull request itself. Those are the tools a fixture can only
// stand in for, so a bundle whose transcript CALLED one is refused.
//
// ASKED OF THE CAPABILITIES THAT OWN THE SET, through
// capability.ProviderSurfaceTools, over the same deps this session's assembly
// ran on. No tool is named here: a third trigger-status tool, or a second
// capability that declares capability.ProviderSurface, is picked up with no
// change to this file.
//
// # Why it is asked, and not differenced
//
// The difference this could be — assemble once as the replay will, once with
// the input binding's kind blanked, and take whatever disappeared — no longer
// isolates anything, because an UNREGISTERED kind is consulted by more than one
// capability. The outbound-shaping ones cannot represent a session on a kind
// they do not know and withhold respond_to_user as well, so a blanked kind
// moves more than one thing and the difference attributes the reply every
// triggered session owes its user to the provider surface. That is a hard
// external-surface finding on the entire triggered class, which is the class
// this bundle format exists for. Asking the owner cannot misattribute a tool it
// did not offer, whatever else a kind turns out to gate.
//
// The skips of the capabilities consulted here go to a discard logger: this
// session's own assembly already offered them and already reported them, and
// warning twice about one decline reads as two problems.
func externalSurfaceTools(
	ctx context.Context,
	class *spiceboxv1alpha1.AgentClass,
	sess *spiceboxv1alpha1.AgentSession,
	binding *spiceboxv1alpha1.ChannelBinding,
	ch boundChannel,
	names []string,
	mem memory.Memory,
) map[string]bool {
	contributed := capability.ProviderSurfaceTools(ctx, metaToolDeps(class, sess, binding, ch, mem, io.Discard))
	if len(contributed) == 0 {
		return nil
	}
	offered := make(map[string]bool, len(names))
	for _, n := range names {
		offered[n] = true
	}
	out := map[string]bool{}
	for _, t := range contributed {
		// Only what the assembly actually offered. A capability can contribute
		// a tool a name-dedup upstream of it already claimed, and marking a
		// name this session was never offered would describe a surface no tool
		// in the bundle came from.
		if offered[t.Name()] {
			out[t.Name()] = true
		}
	}
	return out
}

// asFixtureTools pairs each assembled name with the two facts the self-check
// reads about it: whether it reaches the input kind's own provider surface, and
// whether it exists at all only because a stand-in was seeded.
//
// Both maps are DIFFERENCES the caller computed, never lists written here.
func asFixtureTools(names []string, external, stoodIn map[string]bool) []steelthread.FixtureTool {
	out := make([]steelthread.FixtureTool, 0, len(names))
	for _, n := range names {
		out = append(out, steelthread.FixtureTool{
			Name:            n,
			ExternalSurface: external[n],
			StoodIn:         stoodIn[n],
		})
	}
	return out
}

// warnLogger adapts an io.Writer to the logr.Logger capability.Assemble logs
// its skips through. Every line is a granted capability that contributed no
// tool, which is exactly the information a reader needs when the capture is
// then refused for calling one.
func warnLogger(warn io.Writer) logr.Logger {
	return funcr.New(func(prefix, args string) {
		if prefix != "" {
			fmt.Fprintf(warn, "warning: %s: %s\n", prefix, args)
			return
		}
		fmt.Fprintf(warn, "warning: %s\n", args)
	}, funcr.Options{})
}

// clusterName records which cluster the session came from, for a reader trying
// to find it again. Provenance only: a kubeconfig that cannot be read costs the
// capture a label, not the capture.
func clusterName(g *apcmd.Globals, warn io.Writer) string {
	kubectx, err := g.CurrentContext()
	if err != nil {
		fmt.Fprintf(warn, "warning: could not read the current kubecontext (%v); "+
			"the bundle will not record which cluster it came from\n", err)
		return ""
	}
	return kubectx
}

// oapVersion reports the build that produced the bundle, so a capture can be
// traced to the code that made it. Read from the build info rather than a
// hand-set constant; an unstamped local build reports Go's own "(devel)".
func oapVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return ""
	}
	return info.Main.Version
}
