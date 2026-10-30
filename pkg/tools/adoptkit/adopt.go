// Package adoptkit marks CR-referenced Secrets/ConfigMaps as adopted by the
// operator — a metadata-only server-side-apply of the adoption label + a
// per-owner ownership annotation. It NEVER reads or writes .data. This is the
// Slice-1 minimal adopt pattern; the full controller-idioms adopt.AdoptionHandler
// arrives per-controller in the handler-chain migration slices.
package adoptkit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

const ownerAnnotationPrefix = "agentprimitives.authzed.com/owner-"

// maxAnnotationNameLen is the Kubernetes limit for the "name" part of an
// annotation key (the segment after the last "/"). Names longer than 63
// characters are rejected by the API server with a validation error.
const maxAnnotationNameLen = 63

// OwnerAnnotationKey returns the annotation key for a given owner CR. The full
// key is "agentprimitives.authzed.com/owner-{kind}-{ownerName}".
//
// Exported so a reader that needs to ask WHO adopted an object builds the same
// key this package stamps, instead of re-deriving the format from the string
// literal above and drifting from the truncation rule below.
//
// Kubernetes limits the "name" segment of an annotation key (the part after
// the last "/") to 63 characters. The name segment here is
// "owner-{kind}-{ownerName}". When this exceeds 63 chars, ownerName is
// truncated and a short SHA-256 suffix is appended for uniqueness.
func OwnerAnnotationKey(ownerKind string, owner types.NamespacedName) string {
	// ownerAnnotationPrefix = "agentprimitives.authzed.com/owner-"
	// The name segment (after the "/") = "owner-" + ownerKind + "-" + owner.Name.
	// ownerAnnotationPrefix already supplies "...authzed.com/owner-", so appending
	// ownerKind+"-"+owner.Name gives the correct full key.
	suffix := ownerKind + "-" + owner.Name
	nameSegment := "owner-" + suffix // the part after the "/"
	if len(nameSegment) <= maxAnnotationNameLen {
		return ownerAnnotationPrefix + suffix
	}
	// Name segment is too long. Truncate ownerName and append a hash suffix.
	sum := sha256.Sum256([]byte(owner.Namespace + "/" + ownerKind + "/" + owner.Name))
	hashSuffix := fmt.Sprintf("-%x", sum[:4]) // "-" + 8 hex chars = 9 chars
	// The name segment must be ≤ maxAnnotationNameLen.
	// nameSegment = "owner-" + ownerKind + "-" + owner.Name[:keep] + hashSuffix
	// where keep = maxAnnotationNameLen - len("owner-") - len(ownerKind) - len("-") - len(hashSuffix)
	keep := maxAnnotationNameLen - len("owner-") - len(ownerKind) - 1 - len(hashSuffix)
	return ownerAnnotationPrefix + ownerKind + "-" + owner.Name[:keep] + hashSuffix
}

// HasOwnerOfKind reports whether annotations carry an adoption owner annotation
// for ANY owner of ownerKind. Adoption annotations accumulate — one object
// adopted by several CRs carries one key per owner — so this answers "is this
// object also claimed by a <kind>?" rather than "who owns it".
//
// The truncated-and-hashed long-name form keeps the same
// "...owner-{kind}-" prefix, so both forms are matched. Kind names are
// CamelCase with no "-", so the trailing "-" makes "AgentIdentity" match
// neither "AgentIdentityBinding" nor any other kind sharing a prefix.
func HasOwnerOfKind(annotations map[string]string, ownerKind string) bool {
	if ownerKind == "" {
		return false
	}
	prefix := ownerAnnotationPrefix + ownerKind + "-"
	for k := range annotations {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func fieldManager(ownerKind string, owner types.NamespacedName) string {
	return "agentprimitives-adopt-" + ownerKind + "-" + owner.Name
}

// Adopt applies the label + owner annotation onto obj via SSA. obj must carry
// TypeMeta plus namespace/name (name alone for cluster-scoped objects) and ONLY
// the metadata to own. Exported so callers outside adoptkit — adopting
// arbitrary cluster deps as *unstructured.Unstructured — can reuse the same
// metadata-only SSA pattern instead of reimplementing it.
//
// It adopts only EXISTING objects. SSA Apply upserts, so it would CREATE an
// empty object for a missing reference; existence is confirmed first and a
// NotFound flows back to the caller as their "referenced object is missing"
// signal.
//
// That existence read uses reader — the LIVE APIReader — not the cached client:
// the label-filtered cache holds only already-adopted objects, so a cached Get
// would wrongly return NotFound for a not-yet-adopted one. The SSA Patch uses
// writer.
//
// An object that already carries the adoption label AND this owner's annotation
// is left alone: the apply would change nothing, and it is not free. Every apply
// is merged against every field manager on the object, so on a Secret shared by
// many owners one no-op apply took the API server seconds, paid on every
// reconcile of every owner.
func Adopt(ctx context.Context, reader client.Reader, writer client.Client, obj client.Object, owner types.NamespacedName, ownerKind string) error {
	_, err := adopt(ctx, reader, writer, obj, owner, ownerKind, false)
	return err
}

// adopt is Adopt with one more way to skip the write: when ifUnadopted is set,
// an object that already carries the adoption label, whoever stamped it, is
// left alone too. It reports whether it wrote.
func adopt(ctx context.Context, reader client.Reader, writer client.Client, obj client.Object, owner types.NamespacedName, ownerKind string, ifUnadopted bool) (bool, error) {
	probe := obj.DeepCopyObject().(client.Object)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(obj), probe); err != nil {
		return false, err // NotFound (or other) — do NOT create via the apply below
	}
	key, val := OwnerAnnotationKey(ownerKind, owner), owner.Namespace+"/"+owner.Name
	if _, adopted := probe.GetLabels()[adoptguard.AdoptedLabel]; adopted {
		if ifUnadopted || probe.GetAnnotations()[key] == val {
			return false, nil
		}
	}
	adoptguard.WithAdoptedLabel(obj)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = val
	obj.SetAnnotations(ann)
	return true, writer.Patch(ctx, obj, client.Apply,
		client.FieldOwner(fieldManager(ownerKind, owner)), client.ForceOwnership)
}

// AdoptSecret adopts a referenced Secret for an owner CR (metadata-only SSA).
// reader is the LIVE reader for the existence check; writer applies the patch.
func AdoptSecret(ctx context.Context, reader client.Reader, writer client.Client, objRef, owner types.NamespacedName, ownerKind string) error {
	return Adopt(ctx, reader, writer, secretRef(objRef), owner, ownerKind)
}

// AdoptSecretIfUnadopted adopts a referenced Secret only when nothing has
// adopted it yet, and reports whether it did.
//
// It is for an owner that only needs the Secret READABLE, not recorded as
// its own. The adoptguard reader asks for the adoption label and nothing else,
// so a Secret its long-lived owner (an AgentIdentity, a UserIdentity) already
// adopted is readable as it stands. A short-lived owner that adopts anyway adds
// one annotation and one field manager per instance to a Secret shared by all
// of them, which nothing ever reads and which makes every later write to that
// Secret slower. When it does adopt here, as the first owner, it should
// release that ownership when it is done with the Secret (PruneOwners).
func AdoptSecretIfUnadopted(ctx context.Context, reader client.Reader, writer client.Client, objRef, owner types.NamespacedName, ownerKind string) (bool, error) {
	return adopt(ctx, reader, writer, secretRef(objRef), owner, ownerKind, true)
}

func secretRef(nn types.NamespacedName) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: nn.Namespace, Name: nn.Name},
	}
}

// AdoptConfigMap adopts a referenced ConfigMap for an owner CR (metadata-only SSA).
// reader is the LIVE reader for the existence check; writer applies the patch.
func AdoptConfigMap(ctx context.Context, reader client.Reader, writer client.Client, objRef, owner types.NamespacedName, ownerKind string) error {
	return Adopt(ctx, reader, writer, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: objRef.Namespace, Name: objRef.Name},
	}, owner, ownerKind)
}
