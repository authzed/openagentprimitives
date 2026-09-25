package installcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

var crGroupVersionResources = []schema.GroupVersionResource{
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentsessions"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentclasses"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentidentities"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxsessions"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "toolcalls"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxtoolchains"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxtoolkits"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxtoolspecs"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spiceboxclasses"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "mcpservers"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "channels"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "artifactrenders"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentsessiongrants"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "spicedbbootstraps"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "useridentities"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "sessionuseridentities"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "sidecartoolboxes"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "clusteragentsettings"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentsettings"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "skills"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "skillsources"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "clusterskills"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "clusterskillsources"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "clusteridentityproviders"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "workspacesources"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "credentialupdaterequests"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentuis"},
	// PublicEndpoint carries a finalizer only the operator can clear (it exists
	// to guarantee the tunnel is closed). Deleting its CRD without deleting the
	// CRs first — which is what omitting it from this list does — wedges the
	// teardown on an object nothing is left running to release.
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "publicendpoints"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "sessionholds"},
	// Workshop (the agent-builder's locked-down build space, spec §1)
	// carries FinalizerWorkshop, cleared only by the operator's Workshop
	// controller (pkg/controllers/workshop) reversing every provisioned
	// layer — the tuple, the bearer registration, the toolwriter
	// ClusterRoleBinding, the namespace. The same PublicEndpoint concern
	// above applies: omitting it here would wedge teardown on a finalizer
	// nothing is left running to clear.
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "workshops"},
	// WorkshopProbe (the agent-builder's tool-discovery probe, spec §2.5)
	// carries no finalizer of its own — its pod/NetworkPolicy/ConfigMap are
	// owner-ref GC'd, and any still-running probe pod is deleted
	// synchronously within the same Probe() call that created it — but it is
	// still listed here so `oap clean` removes every trace of it rather than
	// leaving orphaned CRs behind once its CRD is gone.
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "workshopprobes"},
	// SubagentRequest is the delegation request; its child AgentSession is
	// owner-ref'd to it (see buildChild in
	// pkg/controllers/subagentrequest/controller.go). Listed AFTER
	// agentsessions above so the child sessions are already gone by the time
	// this kind is processed, rather than racing this loop's explicit delete
	// against Kubernetes' own owner-reference garbage collection.
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "subagentrequests"},
	{Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "relationshipsources"},
}

// cleanDefaultTimeout is `oap clean`'s own budget for a full teardown. Named
// because the interrupted-install path also has to use it: `oap install`'s
// --timeout is far shorter, and a teardown cut off half-way is worse than one
// that takes longer.
const cleanDefaultTimeout = 5 * time.Minute

func NewCleanCmd(g *apcmd.Globals) *cobra.Command {
	yes := false
	keepNs := false
	keepCRDs := false
	orphanFinalizers := false
	keepComponents := false
	deleteArtifacts := false
	dryRun := ""
	timeout := cleanDefaultTimeout

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove everything oap installed (CRs → operator → CRDs → namespace)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClean(cmd.Context(), cmd.OutOrStdout(), g, cleanOptions{
				Yes:              yes,
				KeepNamespace:    keepNs,
				KeepCRDs:         keepCRDs,
				OrphanFinalizers: orphanFinalizers,
				KeepComponents:   keepComponents,
				DeleteArtifacts:  deleteArtifacts,
				DryRun:           dryRun,
				Timeout:          timeout,
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the interactive confirmation prompt")
	cmd.Flags().BoolVar(&keepNs, "keep-namespace", false, "Leave the agentprimitives-system namespace")
	cmd.Flags().BoolVar(&keepCRDs, "keep-crds", false, "Leave CRDs (and CRs) intact")
	cmd.Flags().BoolVar(&keepComponents, "keep-components", false, "Leave oap-installed cluster components (cert-manager, Envoy Gateway) in place")
	cmd.Flags().BoolVar(&orphanFinalizers, "orphan-finalizers", false, "Force-remove finalizers if CRs are stuck")
	cmd.Flags().BoolVar(&deleteArtifacts, "delete-artifacts", false,
		"Also empty and delete the cloud artifact store bucket (kept by default; only buckets labeled agentprimitives-owned=true are ever deleted)")
	apcmd.DryRunModeFlag(cmd, &dryRun)
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Overall timeout")
	return cmd
}

// cleanOptions carries the teardown switches. It exists because the interrupt
// path in `oap install` calls runClean with no cobra flag vars to name the
// arguments: six consecutive bools there were unreadable at the call site and a
// transposition compiled silently.
type cleanOptions struct {
	// Yes skips the interactive confirmation. NOTE: it also suppresses the
	// scope banner, which is the only place the full destructive scope is
	// printed — callers that set it are responsible for telling the user what is
	// about to be deleted.
	Yes              bool
	KeepNamespace    bool
	KeepCRDs         bool
	OrphanFinalizers bool
	KeepComponents   bool
	DeleteArtifacts  bool
	DryRun           string
	Timeout          time.Duration
}

func runClean(ctx context.Context, out io.Writer, g *apcmd.Globals, opts cleanOptions) error {
	// Ahead of the confirmation prompt: a mode this command will refuse must
	// not first ask the user to confirm a full teardown.
	if err := apcmd.ValidateDryRunMode(opts.DryRun); err != nil {
		return err
	}
	// Asked of stdin, not of the output stream: this gate is about whether
	// somebody is there to type "y" before every CR in the cluster is deleted.
	// The same question `oap identity put-token` and `oap init` ask, asked the
	// same way, because a looser answer here (a char device that is not a
	// terminal, say) would let an unattended run reach the prompt.
	stdinIsTTY := term.IsTerminal(int(os.Stdin.Fd()))

	if !opts.Yes {
		if !stdinIsTTY {
			return fmt.Errorf("oap clean: requires --yes when stdin is not interactive")
		}
		fmt.Fprintln(out, "This will delete all CRs in the agentprimitives.authzed.com group across")
		fmt.Fprintln(out, "ALL namespaces, then remove the spicebox-operator deployment, services,")
		fmt.Fprintln(out, "RBAC, CRDs, the workspace-storage provisioner, and the")
		fmt.Fprintln(out, "agentprimitives-system and ap-workspace-storage namespaces themselves.")
		fmt.Fprintln(out)
		cliout.Prompt(out, "Continue? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(strings.ToLower(line))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(out, "aborted.")
			return nil
		}
	}

	if opts.DryRun == apcmd.DryRunClient {
		fmt.Fprintln(out, "would delete CRs:")
		for _, gvr := range crGroupVersionResources {
			fmt.Fprintf(out, "  - %s.%s\n", gvr.Resource, gvr.Group)
		}
		if !opts.KeepCRDs {
			fmt.Fprintln(out, "would delete CRDs:")
			for _, gvr := range crGroupVersionResources {
				fmt.Fprintf(out, "  - %s.%s\n", gvr.Resource, gvr.Group)
			}
		}
		fmt.Fprintln(out, "would delete workspace-storage provisioner:")
		fmt.Fprintln(out, "  - storageclass ap-workspace-rwx")
		fmt.Fprintln(out, "  - clusterrole/clusterrolebinding ap-workspace-provisioner")
		fmt.Fprintln(out, "  - deployment/serviceaccount/configmap in ap-workspace-storage")
		if !opts.KeepNamespace {
			fmt.Fprintln(out, "would delete namespace agentprimitives-system")
			fmt.Fprintln(out, "would delete namespace ap-workspace-storage")
		}
		if !opts.KeepComponents {
			fmt.Fprintln(out, "would remove oap-installed components (cert-manager, Envoy Gateway), if oap installed them")
		}
		if opts.DeleteArtifacts {
			fmt.Fprintln(out, "would DELETE the artifact store bucket (--delete-artifacts; label-guarded)")
		} else {
			fmt.Fprintln(out, "would keep the artifact store bucket (pass --delete-artifacts to delete it)")
		}
		return nil
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Capture the artifact store URL BEFORE the operator Deployment (its
	// only record) is deleted below.
	artifactURL := operatorArtifactStoreURL(ctx, b)

	// 1. delete all CRs in each kind across namespaces; wait until empty.
	for _, gvr := range crGroupVersionResources {
		if err := deleteAllCRs(ctx, out, b, gvr, opts.OrphanFinalizers); err != nil {
			fmt.Fprintf(out, "warning: %s: %v\n", gvr.Resource, err)
		}
	}

	// 2. delete operator workload (deployment + services + RBAC + SAs).
	gvrDeploy := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrSts := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	gvrSvc := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	gvrSA := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}
	gvrSecret := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	gvrPVC := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	gvrCR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	gvrCRB := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	ns := "agentprimitives-system"

	deletes := []struct {
		gvr  schema.GroupVersionResource
		name string
		ns   string
	}{
		// Operator
		{gvrDeploy, "spicebox-operator", ns},
		{gvrSvc, "spicebox-operator", ns},
		{gvrSvc, "spicebox-gateway", ns},
		{gvrSA, "spicebox-operator", ns},
		{gvrCR, "spicebox-operator", ""},
		{gvrCRB, "spicebox-operator", ""},
		// Postgres
		{gvrDeploy, "spicebox-postgres", ns},
		{gvrSvc, "spicebox-postgres", ns},
		{gvrSecret, "spicebox-postgres-token", ns},
		{gvrPVC, "spicebox-postgres-data", ns},
		// Neo4j
		{gvrSts, "spicebox-neo4j", ns},
		{gvrSvc, "spicebox-neo4j", ns},
		{gvrSecret, "spicebox-neo4j-token", ns},
		{gvrPVC, "data-spicebox-neo4j-0", ns},
		// Graphiti
		{gvrDeploy, "spicebox-graphiti", ns},
		{gvrSvc, "spicebox-graphiti", ns},
		{gvrSecret, "spicebox-graphiti-config", ns},
	}
	for _, d := range deletes {
		err := b.Dynamic.Resource(d.gvr).Namespace(d.ns).Delete(ctx, d.name, metav1.DeleteOptions{})
		switch {
		case err == nil:
			cliout.OK(out, "deleted %s %s", d.gvr.Resource, d.name)
		case errors.IsNotFound(err):
			cliout.Info(out, "  %s %s already absent", d.gvr.Resource, d.name)
		default:
			cliout.Warn(out, "delete %s/%s: %v", d.gvr.Resource, d.name, err)
		}
	}

	// 2b. Tear down the bundled workspace-storage provisioner tier (a separate
	// ap-workspace-storage namespace + cluster-scoped StorageClass/RBAC) oap
	// install lays down for RWX agent workspaces.
	removeWorkspaceStorage(ctx, out, b, opts.KeepNamespace)

	// 2b-ii. Tear down the oap-created stateful (RWO) StorageClass, if any.
	removeStatefulStorage(ctx, out, b)

	// 2b-iii. Tear down (or, by default, report and keep) the durable artifact
	// store bucket the operator was using. Uses the URL captured above, since
	// the operator Deployment that recorded it is already gone by now.
	removeArtifactStore(ctx, out, b, artifactURL, opts.DeleteArtifacts)

	// 2c. Remove cluster components oap installed on the user's behalf
	// (cert-manager, Envoy Gateway), gated by the installed-by annotation so a
	// pre-existing one is never touched. --keep-components opts out.
	if !opts.KeepComponents {
		removeAPInstalledComponents(ctx, out, b)
	}

	// 3. delete CRDs.
	if !opts.KeepCRDs {
		for _, gvr := range crGroupVersionResources {
			crdName := gvr.Resource + "." + gvr.Group
			var crd apiextv1.CustomResourceDefinition
			if err := b.Controller.Get(ctx, client.ObjectKey{Name: crdName}, &crd); err == nil {
				if err := b.Controller.Delete(ctx, &crd); err != nil && !errors.IsNotFound(err) {
					fmt.Fprintf(out, "warning: delete CRD %s: %v\n", crdName, err)
				} else {
					fmt.Fprintf(out, "deleted CRD %s\n", crdName)
				}
			}
		}
	}

	// 4. delete the operator namespace and WAIT for it to actually terminate.
	// A namespace stuck Terminating (e.g. on a GKE neg-finalizer) must be
	// surfaced — never reported as success.
	if !opts.KeepNamespace {
		if err := b.Typed.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			fmt.Fprintf(out, "warning: delete namespace %s: %v\n", ns, err)
		} else {
			fmt.Fprintf(out, "deleting namespace %s...\n", ns)
			nsWaitCtx, cancelNS := context.WithTimeout(ctx, 60*time.Second)
			if werr := waitForNamespaceGone(nsWaitCtx, b, ns); werr != nil {
				// Stuck: surface always; remediate only with --orphan-finalizers.
				unwedgeNamespaceIfStuck(ctx, out, b, ns, opts.OrphanFinalizers)
				if opts.OrphanFinalizers {
					reWaitCtx, cancelRe := context.WithTimeout(ctx, 30*time.Second)
					if rerr := waitForNamespaceGone(reWaitCtx, b, ns); rerr != nil {
						cliout.Warn(out, "namespace %s still terminating after unwedge attempt", ns)
					} else {
						cliout.OK(out, "namespace %s terminated", ns)
					}
					cancelRe()
				}
			} else {
				cliout.OK(out, "deleted namespace %s", ns)
			}
			cancelNS()
		}
	}

	fmt.Fprintln(out, "oap clean done.")
	return nil
}

func deleteAllCRs(ctx context.Context, out io.Writer, b *kube.Bundle, gvr schema.GroupVersionResource, orphan bool) error {
	list, err := b.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, item := range list.Items {
		if err := b.Dynamic.Resource(gvr).Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			fmt.Fprintf(out, "warning: delete %s/%s/%s: %v\n", gvr.Resource, item.GetNamespace(), item.GetName(), err)
		} else {
			fmt.Fprintf(out, "deleted %s %s/%s\n", gvr.Resource, item.GetNamespace(), item.GetName())
		}
	}

	// Wait up to 30s for finalizers to drain.
	drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	drainErr := waitUntilEmpty(drainCtx, b, gvr)
	if drainErr == nil {
		return nil
	}

	// Drain timed out: list what's stuck.
	stuck, lerr := b.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		cliout.Warn(out, "%s drain timed out and listing stuck items failed: %v", gvr.Resource, lerr)
		return nil
	}
	if stuck == nil || len(stuck.Items) == 0 {
		return nil
	}
	if !orphan {
		cliout.Warn(out, "%s drain timed out; %d item(s) stuck — pass --orphan-finalizers to force-remove finalizers",
			gvr.Resource, len(stuck.Items))
		return nil
	}

	// Force-remove finalizers and re-delete.
	for _, item := range stuck.Items {
		patch := []byte(`{"metadata":{"finalizers":[]}}`)
		_, perr := b.Dynamic.Resource(gvr).Namespace(item.GetNamespace()).Patch(ctx, item.GetName(),
			types.MergePatchType, patch, metav1.PatchOptions{})
		if perr != nil {
			cliout.Warn(out, "orphan-patch %s/%s/%s: %v", gvr.Resource, item.GetNamespace(), item.GetName(), perr)
			continue
		}
		// Re-delete; finalizer GC should now complete.
		if derr := b.Dynamic.Resource(gvr).Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{}); derr != nil && !errors.IsNotFound(derr) {
			cliout.Warn(out, "orphaned %s/%s/%s but re-delete failed: %v", gvr.Resource, item.GetNamespace(), item.GetName(), derr)
			continue
		}
		cliout.OK(out, "orphaned + deleted %s %s/%s", gvr.Resource, item.GetNamespace(), item.GetName())
	}
	return nil
}

// removeAPInstalledComponents deletes the cluster components oap installs on the
// user's behalf (cert-manager, Envoy Gateway) — but ONLY the ones oap installed.
// Each component's namespace carries the installed-by annotation (stamped by
// kube.Apply) iff oap applied it, so a pre-existing component is left untouched.
// Best-effort: warnings are logged, never fatal to the rest of clean.
func removeAPInstalledComponents(ctx context.Context, out io.Writer, b *kube.Bundle) {
	components := []struct {
		name, namespace string
		manifests       func() ([][]byte, error)
	}{
		{"cert-manager", "cert-manager", manifests.CertManager},
		{"Envoy Gateway", "envoy-gateway-system", manifests.EnvoyGateway},
	}
	for _, comp := range components {
		ns, err := b.Typed.CoreV1().Namespaces().Get(ctx, comp.namespace, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			continue // component not present
		}
		if err != nil {
			fmt.Fprintf(out, "warning: check %s: %v\n", comp.name, err)
			continue
		}
		if ns.Annotations[kube.InstalledByAnnotation] != kube.InstalledByValue {
			fmt.Fprintf(out, "%s present but not installed by oap — leaving it\n", comp.name)
			continue
		}
		fmt.Fprintf(out, "removing %s (installed by oap)...\n", comp.name)
		groups, gerr := comp.manifests()
		if gerr != nil {
			fmt.Fprintf(out, "warning: load %s manifests: %v\n", comp.name, gerr)
			continue
		}
		for _, g := range groups {
			docs, derr := manifests.Split(g)
			if derr != nil {
				fmt.Fprintf(out, "warning: split %s manifest: %v\n", comp.name, derr)
				continue
			}
			for _, d := range docs {
				if err := kube.Delete(ctx, b.Dynamic, d); err != nil {
					fmt.Fprintf(out, "warning: delete %s %s: %v\n", d.GetKind(), d.GetName(), err)
				}
			}
		}
		fmt.Fprintf(out, "removed %s\n", comp.name)
	}
}

// removeWorkspaceStorage tears down the bundled workspace-storage provisioner
// tier `oap install` lays down for RWX agent workspaces: the cluster-scoped
// StorageClass + RBAC, the namespaced workload in ap-workspace-storage, the
// detection-marker ConfigMap in agentprimitives-system, and — unless keepNs —
// the oap-workspace-storage namespace itself. A cloud-native RWX-class install
// lays none of this down, so every delete tolerates NotFound. Best-effort:
// warnings are logged, never fatal to the rest of clean.
func removeWorkspaceStorage(ctx context.Context, out io.Writer, b *kube.Bundle, keepNs bool) {
	const workspaceNS = "ap-workspace-storage"
	gvrDeploy := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrSA := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}
	gvrCM := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	gvrCR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	gvrCRB := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	gvrSC := schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}

	// Named objects are deleted explicitly (not via the namespace cascade) so
	// that --keep-namespace still removes the workload, and so the cluster-scoped
	// StorageClass/RBAC — which would survive a namespace delete — are caught.
	targets := []struct {
		gvr  schema.GroupVersionResource
		name string
		ns   string
	}{
		{gvrSC, "ap-workspace-rwx", ""},
		{gvrCR, "ap-workspace-provisioner", ""},
		{gvrCRB, "ap-workspace-provisioner", ""},
		{gvrDeploy, "ap-workspace-provisioner", workspaceNS},
		{gvrSA, "ap-workspace-provisioner", workspaceNS},
		{gvrCM, "ap-workspace-provisioner-config", workspaceNS},
		// Detection marker oap install writes into the operator namespace.
		{gvrCM, "ap-workspace-config", "agentprimitives-system"},
	}
	for _, t := range targets {
		err := b.Dynamic.Resource(t.gvr).Namespace(t.ns).Delete(ctx, t.name, metav1.DeleteOptions{})
		switch {
		case err == nil:
			cliout.OK(out, "deleted %s %s", t.gvr.Resource, t.name)
		case errors.IsNotFound(err):
			cliout.Info(out, "  %s %s already absent", t.gvr.Resource, t.name)
		default:
			cliout.Warn(out, "delete %s/%s: %v", t.gvr.Resource, t.name, err)
		}
	}

	if keepNs {
		return
	}
	if err := b.Typed.CoreV1().Namespaces().Delete(ctx, workspaceNS, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		cliout.Warn(out, "delete namespace %s: %v", workspaceNS, err)
	} else {
		cliout.OK(out, "deleted namespace %s", workspaceNS)
	}
}

// removeStatefulStorage deletes the StorageClass oap install created to steer the
// bundled Postgres/Neo4j PVCs onto Hyperdisk (GKE Hyperdisk-only node pools).
// It is cluster-scoped (would survive a namespace delete) and is identified by
// the install-tier label, so a pre-existing cluster class is never touched. A
// clean that laid none down finds nothing. Best-effort: warnings, never fatal.
func removeStatefulStorage(ctx context.Context, out io.Writer, b *kube.Bundle) {
	gvrSC := schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	list, err := b.Dynamic.Resource(gvrSC).List(ctx, metav1.ListOptions{
		LabelSelector: cloud.InstallTierLabelKey + "=" + cloud.InstallTierStatefulStorage,
	})
	if err != nil {
		cliout.Warn(out, "list stateful StorageClasses: %v", err)
		return
	}
	for i := range list.Items {
		item := &list.Items[i]
		// Defensive check: skip any item that slipped through without the label
		// (dynfake and some intermediaries may not honour label-selector filtering).
		if item.GetLabels()[cloud.InstallTierLabelKey] != cloud.InstallTierStatefulStorage {
			continue
		}
		name := item.GetName()
		err := b.Dynamic.Resource(gvrSC).Delete(ctx, name, metav1.DeleteOptions{})
		switch {
		case err == nil:
			cliout.OK(out, "deleted storageclass %s", name)
		case errors.IsNotFound(err):
			cliout.Info(out, "  storageclass %s already absent", name)
		default:
			cliout.Warn(out, "delete storageclass %s: %v", name, err)
		}
	}
}

// operatorArtifactStoreURL reads ARTIFACT_STORE_URL off the live operator
// Deployment — the single source of truth for which store this install used.
// Empty on any error: clean must proceed even on a half-torn cluster, and the
// teardown step reports "no artifact store recorded" loudly instead.
func operatorArtifactStoreURL(ctx context.Context, b *kube.Bundle) string {
	dep, err := b.Typed.AppsV1().Deployments("agentprimitives-system").Get(ctx, "spicebox-operator", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "operator" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "ARTIFACT_STORE_URL" {
				return e.Value
			}
		}
	}
	return ""
}

// removeArtifactStore runs the cloud seam's Teardown for object-store URLs.
// file:// (the --local PVC dies with the namespace) and mem:// need nothing.
func removeArtifactStore(ctx context.Context, out io.Writer, b *kube.Bundle, url string, deleteArtifacts bool) {
	if url == "" {
		if deleteArtifacts {
			cliout.Warn(out, "--delete-artifacts: no artifact store URL recorded on the operator; nothing to delete")
		}
		return
	}
	if strings.HasPrefix(url, "file://") || strings.HasPrefix(url, "mem://") {
		return // PVC/in-process: torn down with the namespace
	}
	strat, err := cloud.Detect(ctx, b.Typed)
	if err != nil {
		cliout.Warn(out, "artifact store %s: cloud detect failed (%v); left untouched — delete manually if desired", url, err)
		return
	}
	res, err := strat.ArtifactStorage().Teardown(ctx, cloud.ArtifactTeardownParams{
		Clients:  cloudClients(b),
		Reporter: newCloudReporter(out),
		URL:      url,
		Delete:   deleteArtifacts,
	})
	switch {
	case err != nil:
		cliout.Warn(out, "artifact store teardown: %v", err)
	case res.Deleted:
		cliout.OK(out, "deleted artifact store %s", url)
	case res.KeptMessage != "":
		cliout.Info(out, "  %s", res.KeptMessage)
	}
}

// unwedgeStrategyForTest, when non-nil, overrides cloud.Detect in
// unwedgeNamespaceIfStuck so the clean wait/unwedge path is testable without a
// live cluster. Production leaves it nil.
var unwedgeStrategyForTest cloud.Strategy

// waitForNamespaceGone polls until the namespace no longer exists or ctx is done.
// A namespace stuck Terminating returns the last observed error (or ctx.Err()) on
// timeout, so the caller can surface the real reason rather than a bare deadline.
func waitForNamespaceGone(ctx context.Context, b *kube.Bundle, ns string) error {
	var lastErr error
	for {
		_, err := b.Typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			lastErr = err // persistent (e.g. forbidden) errors surface on timeout
		}
		select {
		case <-ctx.Done():
			if lastErr != nil && ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("namespace %s still present: %w", ns, lastErr)
			}
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// unwedgeNamespaceIfStuck detects the cloud and runs its namespace-unwedge
// routine, printing the structured report. remediate=true performs the
// destructive cleanup (gated by --orphan-finalizers at the call site);
// remediate=false only inspects and surfaces the manual fix commands.
func unwedgeNamespaceIfStuck(ctx context.Context, out io.Writer, b *kube.Bundle, ns string, remediate bool) {
	strat := unwedgeStrategyForTest
	if strat == nil {
		var derr error
		strat, derr = cloud.Detect(ctx, b.Typed)
		if derr != nil {
			cliout.Warn(out, "namespace %s still terminating and cloud detection failed: %v", ns, derr)
			return
		}
	}
	report, err := strat.UnwedgeTerminatingNamespace(ctx, cloudClients(b), newCloudReporter(out), ns, remediate)
	if err != nil {
		cliout.Warn(out, "unwedge %s: %v", ns, err)
		return
	}
	if len(report.StuckFinalizers) == 0 {
		cliout.Warn(out, "namespace %s is still terminating, but no known cloud finalizer was found. Inspect: kubectl get ns %s -o jsonpath='{.status.conditions}'", ns, ns)
		return
	}
	cliout.Warn(out, "namespace %s wedged on finalizers: %s", ns, strings.Join(report.StuckFinalizers, ", "))
	for _, d := range report.CloudResourcesDeleted {
		cliout.OK(out, "  deleted orphaned cloud resource %s", d)
	}
	for _, c := range report.FinalizersCleared {
		cliout.OK(out, "  cleared finalizer on %s", c)
	}
	for _, blk := range report.Blocked {
		cliout.Warn(out, "  blocked: %s", blk)
	}
	if !remediate || len(report.Blocked) > 0 {
		cliout.Info(out, "to finish unwedging, run:")
		for _, c := range report.ManualCommands {
			cliout.Info(out, "  %s", c)
		}
		if !remediate {
			cliout.Info(out, "or re-run: oap clean --orphan-finalizers")
		}
	}
}

func waitUntilEmpty(ctx context.Context, b *kube.Bundle, gvr schema.GroupVersionResource) error {
	for {
		list, err := b.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if len(list.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
