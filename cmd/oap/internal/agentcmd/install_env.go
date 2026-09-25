package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/buildx"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagebuild"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagemode"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// buildSets carries the CLI-supplied image build inputs.
type buildSets struct {
	secrets  map[string]string // recipe secret name -> "env:VAR" or a literal value
	contexts map[string]string // recipe build-context name -> host path
	platform string            // --platform (default: docker host arch)
	vmSSHKey string            // --vm-ssh-key override for the oap-desktop VM key
	registry string            // --image-registry: push target for NeedsRegistry contexts
}

// resolveBuildSecret resolves a --build-secret value: "env:VAR" reads VAR (a
// missing/empty VAR is an error), anything else is a literal.
func resolveBuildSecret(name string, sets map[string]string) (string, error) {
	raw, ok := sets[name]
	if !ok {
		return "", fmt.Errorf("build secret %q not supplied (pass --build-secret %s=env:VAR)", name, name)
	}
	if v, isEnv := strings.CutPrefix(raw, "env:"); isEnv {
		got := os.Getenv(v)
		if got == "" {
			return "", fmt.Errorf("build secret %q references env %s which is unset/empty", name, v)
		}
		return got, nil
	}
	return raw, nil
}

// installEnv is the production imagebuild.Env for `oap agent install`. Which of
// the three delivery paths it uses is decided once, by imageload.For on the
// current kube-context, and recorded in disp:
//
//   - NeedsRegistry — the cluster's nodes are unreachable from this machine, so
//     images are cross-built and pushed to registry (resolved at construction),
//     and the cluster pulls the mirrored ref. This is the path a remote cluster
//     takes, and the only one that rewrites the ref the AgentClass records.
//   - VMLoad — the oap-desktop VM: present/deliver over SSH (docker save →
//     `k3s ctr images import`).
//   - LocalLoad / NotNeeded — a kind/k3d/minikube node load, or nothing at all
//     for docker-desktop, whose node shares this machine's Docker daemon.
//
// bundleDir is "" when the install source isn't a buildable folder (a packed
// .oap / registry ref carries no images/ dir).
type installEnv struct {
	out io.Writer
	// driver presents the build-time questions, and is the SAME driver every
	// other SCREEN-BASED question this command asks goes over — the bundle's
	// manifest questions and the adopt decision (see
	// installQuestionPresentation, which records the one question that still
	// reads os.Stdin outside it).
	//
	// Shared rather than resolved here for two reasons. huh builds a fresh
	// greedy scanner per field, so a second driver over the same stdin would
	// start a second buffer over bytes the first had already pulled in, and
	// every later question would read EOF and take its default — which the
	// accessible renderer reports as a completed form with a nil error. And
	// the alt-screen decision travels with the driver, so a command with two
	// of them could take the screen for half its questions and not the other
	// half.
	//
	// A nil driver is the signal that nobody is at stdin, which is why it is
	// also this file's whole interactivity gate — see canAsk.
	driver tui.Driver
	// theme styles those questions. Built by the same presentation as the
	// driver, so the two cannot disagree about color or width.
	theme *tui.Theme

	bundleDir string
	kubeCtx   string
	sets      buildSets

	// runner executes the docker builds; the seam that makes them testable.
	runner   imagebuild.Runner
	disp     imageload.Disposition
	registry string // set iff disp == imageload.NeedsRegistry
	// pushPlatform is the --platform value for a registry push, set iff
	// disp == imageload.NeedsRegistry. See resolvePushPlatform for why this
	// cannot default to the docker host's architecture.
	pushPlatform string

	// VM path only (disp == imageload.VMLoad).
	guestIP string
	keyPath string
	ssh     desktop.SSHRunner
	saver   desktop.ImageSaver
}

// newInstallEnv builds the production image-reconcile Env. driver and theme are
// this command's own presentation (installQuestionPresentation), handed in
// rather than resolved here so that every question one `oap agent install` asks
// is asked over one terminal — see installEnv.driver. Both are nil when nobody
// is at stdin, which is what makes canAsk the single interactivity gate.
func newInstallEnv(ctx context.Context, g *apcmd.Globals, bundleDir string, sets buildSets, out io.Writer, driver tui.Driver, theme *tui.Theme) (imagebuild.Env, error) {
	kctx, err := g.CurrentContext()
	if err != nil {
		return nil, fmt.Errorf("resolve kube-context for image reconcile: %w", err)
	}
	// The questions are presented over the stream this command was handed
	// rather than os.Stdout: a test or a shell pipeline supplies a plain
	// io.Writer, and taking over a screen the command is not actually writing
	// to would leave the question invisible.
	e := &installEnv{
		out:       out,
		driver:    driver,
		theme:     theme,
		bundleDir: bundleDir,
		kubeCtx:   kctx,
		sets:      sets,
		runner:    imagebuild.ExecRunner{},
	}
	e.disp = imageload.For(kctx, "").Disposition

	if e.disp == imageload.VMLoad {
		raw, err := g.KubeconfigRaw()
		if err != nil {
			return nil, err
		}
		ip, err := desktop.GuestIPFromKubeconfig(raw, kctx)
		if err != nil {
			return nil, err
		}
		key := sets.vmSSHKey
		if key == "" {
			if key, err = desktop.VMKeyPath(); err != nil {
				return nil, err
			}
		}
		e.guestIP, e.keyPath = ip, key
		e.ssh, e.saver = desktop.NewSSHRunner(), desktop.NewDockerSaver()
		return e, nil
	}

	// A context with no local load path can only receive an image through a
	// registry. Resolve one NOW, before anything is built: failing here costs
	// the user a flag, whereas failing after the build costs an ImagePullBackOff
	// they have to diagnose from the cluster.
	if e.disp == imageload.NeedsRegistry {
		// An explicit --image-registry already IS the answer, so nothing below
		// can change it (resolveRegistryForCluster returns the flag verbatim).
		// Short-circuit before touching the cluster: a namespace-scoped
		// kubeconfig cannot list nodes, and letting cloud.Detect run anyway
		// would fail a fully-specified invocation with "nodes is forbidden".
		if sets.registry != "" {
			e.registry = sets.registry
			// No cluster read here means no node arch to detect, so this takes
			// resolvePushPlatform's fallback rather than the host's.
			e.pushPlatform = imagemode.ResolvePushPlatform(sets.platform, "")
			return e, nil
		}
		b, err := g.Bundle()
		if err != nil {
			return nil, fmt.Errorf("connect to the cluster to resolve an image registry for context %q: %w", kctx, err)
		}
		strat, err := cloud.Detect(ctx, b.Typed)
		if err != nil {
			return nil, fmt.Errorf("detect cloud provider: %w", err)
		}
		reg, err := imagemode.ResolveForCluster(ctx, b.Typed, strat, kctx, sets.registry, out,
			imagemode.Prompt(out, os.Stdin), e.canAsk())
		if err != nil {
			return nil, err
		}
		e.registry = reg
		e.pushPlatform = imagemode.ResolvePushPlatform(sets.platform, imagemode.DetectNodeArch(ctx, b.Typed))
	}
	return e, nil
}

func (e *installEnv) Resolve(ctx context.Context, ref string) (string, bool, error) {
	if e.disp == imageload.VMLoad {
		present, err := desktop.HasImage(ctx, e.ssh, e.keyPath, e.guestIP, ref)
		return ref, present, err
	}
	if e.disp == imageload.NeedsRegistry {
		mirrored := apimage.MirrorRef(ref, e.registry)
		dig, present, err := registryImageDigest(ctx, mirrored)
		if err != nil {
			// A probe failure (no local credentials, offline, a registry that
			// answers 401 rather than 404) must not block a build+push — but it
			// must not be silent either, because it changes whether we rebuild.
			if status, isAuth := authRejection(err); isAuth {
				// The probe authenticated with the very Docker credential
				// helpers `docker buildx --push` will use, so a refusal here is
				// strong evidence the push will be refused too — and the user
				// would otherwise learn that only after a full cross-build.
				host := buildx.RegistryHost(e.registry)
				cliout.Warn(e.out, "%s refused the credentials while checking whether %s exists (HTTP %d) — `docker buildx --push` uses those same Docker credentials, so the push will likely fail the same way. Authenticate to %s first (for GKE Artifact Registry: `gcloud auth configure-docker %s`), or pass --image-registry pointing at a registry you can push to. Continuing anyway: treating the image as absent and rebuilding",
					host, mirrored, status, host, host)
			} else {
				cliout.Warn(e.out, "could not check whether %s already exists (%v) — treating it as absent and rebuilding", mirrored, err)
			}
			return mirrored, false, nil
		}
		if !present {
			// Nothing in the registry yet, so there is no digest to pin. The
			// mirrored tag is only a push TARGET here; Deliver returns the
			// pinned ref once the push has produced one.
			return mirrored, false, nil
		}
		pinned, err := oci.PinnedRef(mirrored, dig)
		if err != nil {
			// mirrored already parsed inside registryImageDigest and dig came
			// from the registry itself, so this is a genuine defect rather than
			// an environmental failure — surface it instead of silently
			// recording the mutable tag this whole path exists to avoid.
			return "", false, fmt.Errorf("pin %s to the digest the registry reports: %w", mirrored, err)
		}
		return pinned, true, nil
	}
	// LocalLoad (kind/k3d/minikube) and NotNeeded (docker-desktop): best-effort —
	// treat as absent so the confirm / --build-missing gate decides (a redundant
	// local load is cheap + idempotent).
	return ref, false, nil
}

// authRejection reports the HTTP status when err is a registry AUTH refusal
// (401/403) rather than an absent image (404) or an unreachable registry (no
// HTTP status at all). Callers use it to distinguish "your credentials are the
// problem" — which the subsequent push will hit too, since both use the ambient
// Docker credential helpers — from a probe that merely failed to answer.
func authRejection(err error) (int, bool) {
	var te *transport.Error
	if errors.As(err, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden) {
		return te.StatusCode, true
	}
	return 0, false
}

// registryImageDigest reports whether ref resolves in its registry and, when it
// does, the digest the registry currently holds for it — using the ambient
// Docker credentials. Presence and identity come from the same HEAD, so a
// caller never has to re-query to learn what the tag it just probed points at.
//
// The presence half is evidence, not proof: cluster nodes pull with their OWN
// service account, not the operator's laptop credentials. A false negative
// costs an unnecessary rebuild+push; a false positive yields ImagePullBackOff.
// Pushing to the registry the cluster already pulls its control plane from is
// what makes the gap small — callers must not describe this as verifying
// node-side pullability. The digest half has no such caveat: it is whatever the
// registry served, which is exactly what a digest-pinned ref will resolve to.
func registryImageDigest(ctx context.Context, ref string) (string, bool, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", false, fmt.Errorf("parse %s: %w", ref, err)
	}
	desc, err := remote.Head(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && te.StatusCode == http.StatusNotFound {
			return "", false, nil // definitively absent
		}
		return "", false, err
	}
	return desc.Digest.String(), true, nil
}

func (e *installEnv) Deliver(ctx context.Context, b oap.ImageBuild, in imagebuild.Inputs, ref string) (string, error) {
	if e.bundleDir == "" {
		return "", fmt.Errorf("cannot build image %s: install from the bundle's folder source (with its images/ dir), or make the ref already present/pullable", ref)
	}

	if e.disp == imageload.NeedsRegistry {
		target := apimage.MirrorRef(ref, e.registry)
		// A push targets the cluster's nodes, not this machine. pushPlatform
		// already honors an explicit --platform, so it is the single answer;
		// leaving in.Platform as the flag's empty default would let docker pick
		// the host arch and produce an image the nodes cannot exec.
		in.Platform = e.pushPlatform
		digest, err := imagebuild.BuildAndPush(ctx, e.runner, e.out, e.bundleDir, b, in, target)
		if err != nil {
			return "", err
		}
		// The push targets the TAG (so the registry keeps a human-readable
		// pointer), but the ref recorded on the class is pinned to the digest
		// just pushed. A tag is mutable and, worse, cached per-node under the
		// default IfNotPresent pull policy: replacing an image under the same
		// tag leaves every node that already has it running the old layers. A
		// digest ref is content-addressed, so a stale cache cannot satisfy it.
		//
		// This does not break SSA idempotency, because Reconcile only reaches
		// Deliver when the image was ABSENT. A re-install whose image is
		// unchanged short-circuits at Resolve, which re-derives the identical
		// digest from the registry — so the applied ref stays byte-identical
		// and the apply stays a no-op. The digest changes only when the content
		// does, which is a real change of desired state.
		pinned, err := oci.PinnedRef(target, digest)
		if err != nil {
			return "", fmt.Errorf("pin %s to its pushed digest: %w", target, err)
		}
		fmt.Fprintf(e.out, "  pushed %s (recorded as %s)\n", target, pinned)
		return pinned, nil
	}

	if err := imagebuild.BuildLocal(ctx, e.runner, e.out, e.bundleDir, b, in, ref); err != nil {
		return "", err
	}
	switch e.disp {
	case imageload.VMLoad:
		if err := desktop.LoadImage(ctx, e.ssh, e.saver, e.keyPath, e.guestIP, ref); err != nil {
			return "", err
		}
	case imageload.LocalLoad:
		plan := imageload.For(e.kubeCtx, ref)
		fmt.Fprintf(e.out, "==> %s\n", strings.Join(plan.Argv, " "))
		load := exec.CommandContext(ctx, plan.Argv[0], plan.Argv[1:]...)
		load.Stdout, load.Stderr = e.out, e.out
		if err := load.Run(); err != nil {
			return "", fmt.Errorf("image-load %s: %w", ref, err)
		}
	case imageload.NotNeeded:
		// docker-desktop: the node shares the laptop's daemon, so the build is
		// already visible to the cluster.
	default:
		// imageload's classification is total, so this is unreachable today.
		// It exists so that ADDING a disposition surfaces here as a loud error
		// instead of falling through to "return ref, nil" — the silent success
		// (an AgentClass pointing at an image the cluster cannot pull) this
		// whole path was built to eliminate.
		return "", fmt.Errorf("unhandled image-load disposition %s for context %q: cannot deliver %s", e.disp, e.kubeCtx, ref)
	}
	return ref, nil
}

// canAsk reports whether there is somebody at stdin to answer a build-time
// question.
//
// It is the driver's own presence rather than a second flag: the presentation
// is built only for a run somebody is at, so "there is a driver" and "there is
// an operator" are the same fact, and a separate bool would be one more thing
// that can disagree with it.
func (e *installEnv) canAsk() bool { return e.driver != nil }

// ask presents one build-time question and returns the answered State.
//
// The Env contract carries no context — Reconcile calls these three methods
// with nothing but their argument — so the run is rooted at Background. A
// question here neither blocks on the network nor outlives the answer it
// collects.
func (e *installEnv) ask(screens ...tui.Screen) (*tui.State, error) {
	if !e.canAsk() {
		// Unreachable through the three methods below, which all check canAsk
		// first. Loud rather than silent anyway: presenting with no driver would
		// let the sequencer resolve one from a nil reader, which is huh's "read
		// os.Stdin" signal — a question asked of whatever terminal the process
		// happened to inherit.
		return nil, errors.New("no terminal is attached to ask about the image build")
	}
	return tui.Run(context.Background(), screens, tui.Options{
		Theme: e.theme,
		Out:   e.out,
		// Inline is not set here, and must not be: this run presents over the
		// command's own driver, which already carries that decision. Stating it
		// twice is how the two halves of one command come to disagree.
		Driver: e.driver,
	})
}

// Confirm asks whether to build an image that is not available.
//
// A run that cannot ask declines, which is the safe answer: Reconcile turns a
// decline into a refusal naming the image, while a defaulted "yes" would start
// a multi-minute cross-build nobody asked for.
func (e *installEnv) Confirm(prompt string) bool {
	if !e.canAsk() {
		return false
	}
	answered, err := e.ask(newBuildConfirmQuestion(prompt))
	if err != nil {
		// Ctrl+C is the operator declining, not a fault: it needs no
		// explanation, and warning about it would report a problem where there
		// was a decision. Anything else IS a fault, and the operator is owed
		// it — this question decides whether an image gets built, and a
		// silently defaulted decline would surface much later as "not available
		// and build declined" with nothing saying the question never rendered.
		if !errors.Is(err, huh.ErrUserAborted) {
			cliout.Warn(e.out, "could not ask %q (%s) — treating it as declined", prompt, tui.UserFacing(err))
		}
		return false
	}
	return answered.Bool(keyBuildConfirm)
}

func (e *installEnv) Secret(name string) (string, error) {
	if _, ok := e.sets.secrets[name]; ok {
		return resolveBuildSecret(name, e.sets.secrets)
	}
	if !e.canAsk() {
		return "", fmt.Errorf("build secret %q not supplied and stdin is not interactive (pass --build-secret %s=env:VAR)", name, name)
	}
	answered, err := e.ask(newBuildSecretQuestion(name))
	if err != nil {
		return "", fmt.Errorf("prompt for build secret %q: %w", name, tui.UserFacing(err))
	}
	return answered.Get(keyBuildSecret), nil
}

func (e *installEnv) Path(name string) (string, error) {
	if p, ok := e.sets.contexts[name]; ok {
		return p, nil
	}
	if !e.canAsk() {
		return "", fmt.Errorf("build-context %q not supplied and stdin is not interactive (pass --build-context %s=/path)", name, name)
	}
	answered, err := e.ask(newBuildContextQuestion(name))
	if err != nil {
		return "", fmt.Errorf("prompt for build-context %q: %w", name, tui.UserFacing(err))
	}
	return answered.Get(keyBuildContext), nil
}

// The State keys the build-time answers land under. They are what a caller
// would seed to answer one of these questions ahead of the run, so they are
// part of this file's vocabulary and stay stable.
const (
	keyBuildConfirm = "build"
	keyBuildSecret  = "secret"
	keyBuildContext = "context"
)

// newBuildConfirmQuestion is the yes/no Reconcile asks before building an image
// the cluster does not have.
//
// Pre-set to no. Whatever this resolves to is what SILENCE answers with — huh's
// accessible renderer returns the bound value when its input runs out, with no
// error — and a defaulted yes would start a multi-minute cross-build nobody
// asked for.
func newBuildConfirmQuestion(prompt string) *tui.Question {
	return tui.NewConfirm(tui.ConfirmOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:    "build",
			Label: "Build",
			Key:   keyBuildConfirm,
			Title: prompt,
		},
		Default: false,
	})
}

// newBuildSecretQuestion asks for one of the bundle's build secrets.
//
// It records NO summary line, and that is the point: the run's State lives as
// long as this one question, while a summary is written into the operator's
// scrollback, where a build secret would outlive the terminal it was pasted
// into. The refusal it gives for an empty answer names the question and
// repeats nothing that was typed, for the same reason.
//
// Not masked, either. huh's masked mode needs a reader carrying a terminal file
// descriptor, and its accessible renderer discards the resulting error, so
// off-TTY the field would ask nothing, accept nothing and report nothing —
// handing the build an empty credential.
func newBuildSecretQuestion(name string) *tui.Question {
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:    "secret",
			Label: "Build secret",
			Key:   keyBuildSecret,
			Title: fmt.Sprintf("Build secret %q", name),
		},
	})
}

// newBuildContextQuestion asks for the host path behind one build context.
func newBuildContextQuestion(name string) *tui.Question {
	return tui.NewText(tui.TextOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:    "context",
			Label: "Build context",
			Key:   keyBuildContext,
			Title: fmt.Sprintf("Path for build-context %q", name),
		},
	})
}
