package wizardrun

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// ApplyReceipt tracks only Channel/Secret objects this invocation atomically
// created. It deliberately excludes preexisting resources and capability
// patches, whose rollback policy remains the graph installer's policy.
type ApplyReceipt struct {
	mu      sync.Mutex
	once    sync.Once
	err     error
	created []trackedObject
}

type trackedObject struct {
	apiVersion      string
	kind            string
	namespace       string
	name            string
	uid             types.UID
	resourceVersion string
}

// Rollback deletes invocation-created objects once, in reverse order, using
// both observed UID and resourceVersion so an externally changed or replaced
// object is preserved rather than mistaken for this invocation's write.
func (r *ApplyReceipt) Rollback(ctx context.Context, c client.Client) error {
	if r == nil {
		return nil
	}
	if c == nil {
		return fmt.Errorf("wizardrun: no Kubernetes client wired for channel setup rollback")
	}
	r.once.Do(func() {
		r.mu.Lock()
		created := append([]trackedObject(nil), r.created...)
		r.mu.Unlock()
		var errs []error
		for i := len(created) - 1; i >= 0; i-- {
			tracked := created[i]
			if tracked.uid == "" || tracked.resourceVersion == "" {
				errs = append(errs, fmt.Errorf("rollback channel setup: preserve %s %s/%s because its created identity was incomplete", tracked.kind, tracked.namespace, tracked.name))
				continue
			}
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion(tracked.apiVersion)
			obj.SetKind(tracked.kind)
			obj.SetNamespace(tracked.namespace)
			obj.SetName(tracked.name)
			preconditions := metav1.Preconditions{UID: &tracked.uid, ResourceVersion: &tracked.resourceVersion}
			if err := c.Delete(ctx, obj, client.Preconditions(preconditions)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("rollback channel setup: delete %s %s/%s: %w", tracked.kind, tracked.namespace, tracked.name, err))
			}
		}
		r.err = errors.Join(errs...)
	})
	return r.err
}

func (r *ApplyReceipt) record(obj client.Object) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = append(r.created, trackedObject{
		apiVersion: obj.GetObjectKind().GroupVersionKind().GroupVersion().String(),
		kind:       obj.GetObjectKind().GroupVersionKind().Kind, namespace: obj.GetNamespace(), name: obj.GetName(),
		uid: obj.GetUID(), resourceVersion: obj.GetResourceVersion(),
	})
	return len(r.created) - 1
}

func (r *ApplyReceipt) refresh(index int, obj client.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.created) || r.created[index].uid != obj.GetUID() {
		return
	}
	r.created[index].resourceVersion = obj.GetResourceVersion()
}

// ApplyTracked is the in-cluster transactional variant of Apply. It claims
// absent resources with Create before exact-identity SSA and returns a receipt
// even when a later write fails, allowing the graph workflow to clean partial
// channel writes together with its other created resources.
func ApplyTracked(ctx context.Context, c client.Client, installedBy string, wizOut channelkinds.WizardOutput) (*ApplyReceipt, error) {
	return ApplyTrackedGuarded(ctx, c, installedBy, wizOut, nil)
}

// ObjectObservation is a metadata-only snapshot captured before a graph can
// mutate the cluster. Guard carries the exact identity into ApplyTrackedGuarded;
// labels and annotations let the graph installer make its ownership decision
// without exposing Secret data.
type ObjectObservation struct {
	Guard       ObjectGuard
	Exists      bool
	Labels      map[string]string
	Annotations map[string]string
}

// ObjectGuard pins one object's plan-time existence and API identity. Its
// fields stay private so callers cannot accidentally manufacture a weaker
// guard than ObserveObject returned.
type ObjectGuard struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
	snapshot   trackedObjectSnapshot
}

// ObserveObject returns metadata and an exact identity guard without reading
// or retaining an object's payload.
func ObserveObject(ctx context.Context, c client.Client, desired *unstructured.Unstructured) (ObjectObservation, error) {
	if c == nil {
		return ObjectObservation{}, fmt.Errorf("wizardrun: no Kubernetes client wired for object preflight")
	}
	if desired == nil || desired.GetKind() == "" || desired.GetName() == "" {
		return ObjectObservation{}, fmt.Errorf("wizardrun: object preflight requires kind and name")
	}
	snapshot, err := snapshotTrackedObject(ctx, c, desired)
	if err != nil {
		return ObjectObservation{}, err
	}
	return ObjectObservation{
		Guard: ObjectGuard{
			apiVersion: desired.GetAPIVersion(), kind: desired.GetKind(), namespace: desired.GetNamespace(), name: desired.GetName(),
			snapshot: snapshot,
		},
		Exists: snapshot.exists, Labels: snapshot.labels, Annotations: snapshot.annotations,
	}, nil
}

// ApplyTrackedGuarded is ApplyTracked with plan-time exact-identity guards.
// Every supplied guard must name an emitted manifest, and the live object must
// still have exactly the existence/UID/resourceVersion observed at planning.
func ApplyTrackedGuarded(ctx context.Context, c client.Client, installedBy string, wizOut channelkinds.WizardOutput, guards []ObjectGuard) (*ApplyReceipt, error) {
	receipt := &ApplyReceipt{}
	if c == nil {
		return receipt, fmt.Errorf("wizardrun: no Kubernetes client wired, so there is nothing to land this channel with")
	}
	type trackedApply struct {
		desired     *unstructured.Unstructured
		manager     string
		allowCreate bool
		snapshot    trackedObjectSnapshot
	}
	toApply := make([]any, 0, 2)
	if wizOut.SecretManifest != nil {
		toApply = append(toApply, wizOut.SecretManifest)
	}
	if wizOut.ChannelManifest != nil {
		toApply = append(toApply, wizOut.ChannelManifest)
	}
	objects := make([]trackedApply, 0, 3)
	for _, manifest := range toApply {
		raw, err := yaml.Marshal(manifest)
		if err != nil {
			return receipt, fmt.Errorf("marshal manifest: %w", err)
		}
		docs, err := manifests.Split(raw)
		if err != nil {
			return receipt, err
		}
		for _, obj := range docs {
			stampInstalledBy(obj, installedBy)
			objects = append(objects, trackedApply{desired: obj, manager: FieldManager, allowCreate: true})
		}
	}
	if patch := wizOut.CapabilityPatch; patch != nil {
		objects = append(objects, trackedApply{desired: patch.DeepCopy(), manager: CapabilityFieldManager})
	}
	guardByKey := make(map[string]ObjectGuard, len(guards))
	for _, guard := range guards {
		key := guard.key()
		if _, duplicate := guardByKey[key]; duplicate {
			return receipt, fmt.Errorf("wizardrun: duplicate object guard for %s", key)
		}
		guardByKey[key] = guard
	}
	usedGuards := make(map[string]bool, len(guards))

	// Snapshot every guard before the first write. Apply must carry these
	// exact observations forward; reclassifying a resource that appeared,
	// disappeared, or changed after this point would turn the guard into an
	// upsert race.
	for i := range objects {
		observed, err := snapshotTrackedObject(ctx, c, objects[i].desired)
		if err != nil {
			return receipt, err
		}
		key := objectGuardKey(objects[i].desired.GetAPIVersion(), objects[i].desired.GetKind(), objects[i].desired.GetNamespace(), objects[i].desired.GetName())
		if guard, ok := guardByKey[key]; ok {
			if err := guard.verify(observed); err != nil {
				return receipt, err
			}
			usedGuards[key] = true
		}
		objects[i].snapshot = observed
		if !observed.exists && !objects[i].allowCreate {
			return receipt, fmt.Errorf("%s %s/%s does not exist", objects[i].desired.GetKind(), objects[i].desired.GetNamespace(), objects[i].desired.GetName())
		}
		switch objects[i].desired.GetKind() {
		case "Channel":
			if observed.exists && !wizOut.ReplaceExisting {
				return receipt, fmt.Errorf("channel %q already exists in namespace %q; choose a different name or delete it first", objects[i].desired.GetName(), objects[i].desired.GetNamespace())
			}
		case "Secret":
			if observed.exists && observed.annotations[InstalledByAnnotation] == "" {
				return receipt, fmt.Errorf("secret %q already exists in namespace %q and was not created by this project; channel setup will not overwrite its data — choose a different channel name, or delete the Secret first", objects[i].desired.GetName(), objects[i].desired.GetNamespace())
			}
		}
	}
	for key := range guardByKey {
		if !usedGuards[key] {
			return receipt, fmt.Errorf("wizardrun: guarded object %s was not emitted by channel setup", key)
		}
	}

	for i := range objects {
		obj := objects[i]
		if err := applyTrackedObject(ctx, c, receipt, obj.desired, obj.manager, obj.snapshot); err != nil {
			if obj.manager == CapabilityFieldManager {
				return receipt, fmt.Errorf("apply capabilities to AgentClass %q in namespace %q: %w", obj.desired.GetName(), obj.desired.GetNamespace(), err)
			}
			return receipt, fmt.Errorf("apply %s/%s: %w", obj.desired.GetKind(), obj.desired.GetName(), err)
		}
	}
	return receipt, nil
}

type trackedObjectSnapshot struct {
	exists          bool
	uid             types.UID
	resourceVersion string
	annotations     map[string]string
	labels          map[string]string
}

func snapshotTrackedObject(ctx context.Context, c client.Client, desired *unstructured.Unstructured) (trackedObjectSnapshot, error) {
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(desired.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), observed)
	if apierrors.IsNotFound(err) {
		return trackedObjectSnapshot{}, nil
	}
	if err != nil {
		return trackedObjectSnapshot{}, fmt.Errorf("check for existing %s %q in namespace %q: %w", desired.GetKind(), desired.GetName(), desired.GetNamespace(), err)
	}
	return trackedObjectSnapshot{
		exists: true, uid: observed.GetUID(), resourceVersion: observed.GetResourceVersion(),
		annotations: observed.GetAnnotations(), labels: observed.GetLabels(),
	}, nil
}

func (g ObjectGuard) key() string {
	return objectGuardKey(g.apiVersion, g.kind, g.namespace, g.name)
}

func objectGuardKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "|" + kind + "|" + namespace + "|" + name
}

func (g ObjectGuard) verify(observed trackedObjectSnapshot) error {
	if observed.exists != g.snapshot.exists {
		return fmt.Errorf("guarded %s %s/%s changed existence after graph preflight; refusing to overwrite it", g.kind, g.namespace, g.name)
	}
	if observed.exists && (observed.uid != g.snapshot.uid || observed.resourceVersion != g.snapshot.resourceVersion) {
		return fmt.Errorf("guarded %s %s/%s changed after graph preflight; refusing to overwrite it", g.kind, g.namespace, g.name)
	}
	return nil
}

func stampInstalledBy(obj *unstructured.Unstructured, installedBy string) {
	if installedBy == "" {
		return
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[InstalledByAnnotation] = installedBy
	obj.SetAnnotations(annotations)
}

func applyTrackedObject(ctx context.Context, c client.Client, receipt *ApplyReceipt, desired *unstructured.Unstructured, manager string, snapshot trackedObjectSnapshot) error {
	key := client.ObjectKeyFromObject(desired)
	createdIndex := -1
	var observed *unstructured.Unstructured
	if !snapshot.exists {
		created := desired.DeepCopy()
		if err := c.Create(ctx, created, client.FieldOwner(manager)); err != nil {
			return fmt.Errorf("claim absent %s/%s with create: %w", desired.GetKind(), desired.GetName(), err)
		}
		createdIndex = receipt.record(created)
		observed = created
	} else {
		observed = &unstructured.Unstructured{}
		observed.SetGroupVersionKind(desired.GroupVersionKind())
		if err := c.Get(ctx, key, observed); err != nil {
			return fmt.Errorf("re-read guarded %s/%s: %w", desired.GetKind(), desired.GetName(), err)
		}
		if observed.GetUID() != snapshot.uid || observed.GetResourceVersion() != snapshot.resourceVersion {
			return fmt.Errorf("guarded %s %s/%s changed after preflight; refusing to overwrite it", desired.GetKind(), desired.GetNamespace(), desired.GetName())
		}
	}

	desired.SetUID(observed.GetUID())
	desired.SetResourceVersion(observed.GetResourceVersion())
	if err := c.Patch(ctx, desired, client.Apply, client.FieldOwner(manager), client.ForceOwnership); err != nil {
		// A failed write is ambiguous: the server may have applied it before the
		// client observed the error. Keep the Create response identity. A later
		// rollback may conservatively preserve the object, but must never adopt
		// a subsequent same-UID update through another read.
		return fmt.Errorf("ssa-apply %s/%s: %w", desired.GetKind(), desired.GetName(), err)
	}
	if createdIndex >= 0 {
		// controller-runtime decodes the Patch response into desired. It is the
		// only response whose resourceVersion belongs to this invocation; a GET
		// here could observe and adopt an external update before rollback.
		if desired.GetUID() == "" || desired.GetUID() != observed.GetUID() || desired.GetResourceVersion() == "" {
			return fmt.Errorf("ssa-apply %s/%s returned an incomplete or changed identity", desired.GetKind(), desired.GetName())
		}
		receipt.refresh(createdIndex, desired)
	}
	return nil
}
