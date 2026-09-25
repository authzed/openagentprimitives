// Package workspacejob constrains Jobs created by a session runner to the
// workspace reconcile template derived from operator-owned session status.
package workspacejob

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

const Path = "/validate-workspace-job"
const runnerPrefix = "system:serviceaccount:"
const runnerSuffix = "-runner-sa"
const reconcileDeadline = int64(600)

type Webhook struct {
	reader  client.Reader
	decoder admission.Decoder
}

func New(reader client.Reader, decoder admission.Decoder) *Webhook {
	return &Webhook{reader: reader, decoder: decoder}
}

func (w *Webhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}
	username := req.UserInfo.Username
	if !strings.HasPrefix(username, runnerPrefix) || !strings.HasSuffix(username, runnerSuffix) {
		return admission.Allowed("requester is not a session runner")
	}
	parts := strings.Split(strings.TrimPrefix(username, runnerPrefix), ":")
	if len(parts) != 2 || parts[0] == "" || !strings.HasSuffix(parts[1], runnerSuffix) {
		return admission.Denied("runner ServiceAccount cannot be attributed to a session")
	}
	sessionName := strings.TrimSuffix(parts[1], runnerSuffix)
	if sessionName == "" || req.Namespace != parts[0] {
		return admission.Denied("workspace Job must be in the runner session namespace")
	}
	var job batchv1.Job
	if err := w.decoder.Decode(req, &job); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if job.Namespace != "" && job.Namespace != parts[0] {
		return admission.Denied("workspace Job namespace differs from runner session")
	}
	var session v1.AgentSession
	if err := w.reader.Get(ctx, types.NamespacedName{Namespace: parts[0], Name: sessionName}, &session); err != nil {
		return admission.Errored(http.StatusForbidden, err)
	}
	resolved := session.Status.ResolvedWorkspaceSource
	if session.UID == "" || resolved == nil || !resolved.OverlayCut {
		return admission.Denied("runner session has no ready workspace source")
	}
	var op string
	switch job.Name {
	case "ws-sync-" + sessionName:
		op = "sync"
	case "ws-apply-" + sessionName:
		op = "apply"
	default:
		return admission.Denied("runner may create only its own workspace reconcile Jobs")
	}
	driver, ok := registry.Get(resolved.Kind)
	if !ok {
		return admission.Denied("workspace source driver is not registered")
	}
	spec := workspacekinds.Spec{Kind: resolved.Kind, Locator: resolved.Locator, Ref: resolved.Revision}
	var cmds []workspacekinds.Command
	var err error
	credEnv, credKey, credSecret := "", "", ""
	if op == "apply" {
		applier, applies := driver.(workspacekinds.Applier)
		if !applies {
			return admission.Denied("workspace source does not support apply")
		}
		cmds, err = applier.ApplyCommands(spec, "/workspace")
		if credentialed, ok := driver.(workspacekinds.CredentialedApplier); ok {
			credEnv, credKey = credentialed.ApplyCredential()
		}
		credSecret = v1.PassthroughCredentialSecretName(sessionName)
	} else {
		cmds, err = driver.SyncCommands(spec, "/workspace")
	}
	if err != nil {
		return admission.Denied("workspace source commands are invalid: " + err.Error())
	}
	image := resolved.ReconcileImage
	if image == "" {
		image = "alpine/git:latest"
	}
	sa := resolved.ReconcileServiceAccount
	if sa == "" {
		sa = "ap-snapshotter"
	}
	expected := workspace.BuildReconcileJob(
		workspace.CPByPodConfig{Namespace: parts[0], Image: image, ServiceAccount: sa},
		job.Name, podspec.WorkspaceClaimName(&session), cmds, credEnv, credSecret, credKey, reconcileDeadline)
	trueValue := true
	expected.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
		Name: sessionName, UID: session.UID, Controller: &trueValue, BlockOwnerDeletion: &trueValue,
	}}
	if !reflect.DeepEqual(job.OwnerReferences, expected.OwnerReferences) {
		return admission.Denied("workspace Job must be owned by the runner session")
	}
	// Kubernetes defaults Jobs before validating admission. Apply the same
	// defaults to both sides so omitted defaultable fields compare equally.
	scheme.Scheme.Default(expected)
	scheme.Scheme.Default(&job)
	if !reflect.DeepEqual(job.Spec, expected.Spec) || !reflect.DeepEqual(job.Labels, expected.Labels) {
		return admission.Denied("workspace Job differs from the session's permitted reconcile template")
	}
	return admission.Allowed("")
}
