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
func Adopt(ctx context.Context, reader client.Reader, writer client.Client, obj client.Object, owner types.NamespacedName, ownerKind string) error {
	probe := obj.DeepCopyObject().(client.Object)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(obj), probe); err != nil {
		return err // NotFound (or other) — do NOT create via the apply below
	}
	adoptguard.WithAdoptedLabel(obj)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[OwnerAnnotationKey(ownerKind, owner)] = owner.Namespace + "/" + owner.Name
	obj.SetAnnotations(ann)
	return writer.Patch(ctx, obj, client.Apply,
		client.FieldOwner(fieldManager(ownerKind, owner)), client.ForceOwnership)
}

// AdoptSecret adopts a referenced Secret for an owner CR (metadata-only SSA).
// reader is the LIVE reader for the existence check; writer applies the patch.
func AdoptSecret(ctx context.Context, reader client.Reader, writer client.Client, objRef, owner types.NamespacedName, ownerKind string) error {
	return Adopt(ctx, reader, writer, &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: objRef.Namespace, Name: objRef.Name},
	}, owner, ownerKind)
}

// AdoptConfigMap adopts a referenced ConfigMap for an owner CR (metadata-only SSA).
// reader is the LIVE reader for the existence check; writer applies the patch.
func AdoptConfigMap(ctx context.Context, reader client.Reader, writer client.Client, objRef, owner types.NamespacedName, ownerKind string) error {
	return Adopt(ctx, reader, writer, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: objRef.Namespace, Name: objRef.Name},
	}, owner, ownerKind)
}
