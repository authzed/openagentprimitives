// Package materialize answers whether a Skill/ClusterSkill's non-local
// canonical name is actually backed by the SkillSource/ClusterSkillSource that
// produces it. This is the unforgeable signal validate.CheckProvenance must
// gate on instead of the user-writable spec.Source field: a tenant with
// skills:create can set spec.Source to anything, so a boolean derived from
// "Source != nil" lets them claim a trusted git-authority canonical name
// (e.g. "github.com/trusted-org/skills//evil") for attacker-controlled
// spec.body — which an org's name-pattern allowlist (AllowedSkills) would then
// trust.
//
// The signal used here instead cannot be forged by a same-namespace tenant:
//
//   - A controller owner-ref (Controller: true) of the right Kind
//     (SkillSource for a Skill, ClusterSkillSource for a ClusterSkill) —
//     k8s owner-refs are same-namespace only, so a namespaced Skill cannot
//     owner-ref a SkillSource in another namespace, and a tenant cannot set
//     Controller:true on a ref to an object it doesn't already control.
//   - The referenced SkillSource/ClusterSkillSource must actually exist (a
//     dangling owner-ref to a deleted/never-created source is not enough).
//   - Its RepoURL, normalized, must equal the canonical name's authority — so
//     a tenant can't owner-ref their OWN SkillSource and still claim a
//     different org's authority.
//
// Only when all three hold is the name treated as materialized. A canonical
// name that fails to parse, or one using the reserved "local" authority, is
// reported as (false, nil) here: validate.Skill and
// validate.CheckProvenance's own canonical.Parse call already surface a parse
// error, and a local name never needs (or gets) a git-authority owner, so
// there is nothing this package needs to add.
package materialize

import (
	"context"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	skillSourceKind        = "SkillSource"
	clusterSkillSourceKind = "ClusterSkillSource"
)

// Skill reports whether a namespaced Skill's canonicalName is a materialized
// git-authority name, per the package doc's three-part test. namespace and
// ownerRefs are the Skill's own Namespace/OwnerReferences. A Get error on the
// candidate SkillSource other than NotFound is returned as err — the caller
// (a security gate) must decide how to treat an inconclusive lookup rather
// than have it silently folded into either verdict.
func Skill(ctx context.Context, r client.Reader, namespace string, ownerRefs []metav1.OwnerReference, canonicalName string) (bool, error) {
	n, ownerName, ok := parseAndFindOwner(canonicalName, ownerRefs, skillSourceKind)
	if !ok {
		return false, nil
	}
	var src v1.SkillSource
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ownerName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return canonical.Normalize(src.Spec.RepoURL) == n.Authority, nil
}

// ClusterSkill is the cluster-scoped counterpart of Skill: it checks for a
// controller owner-ref to a ClusterSkillSource. Neither the ClusterSkill nor
// its ClusterSkillSource owner carries a namespace.
func ClusterSkill(ctx context.Context, r client.Reader, ownerRefs []metav1.OwnerReference, canonicalName string) (bool, error) {
	n, ownerName, ok := parseAndFindOwner(canonicalName, ownerRefs, clusterSkillSourceKind)
	if !ok {
		return false, nil
	}
	var src v1.ClusterSkillSource
	if err := r.Get(ctx, client.ObjectKey{Name: ownerName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return canonical.Normalize(src.Spec.RepoURL) == n.Authority, nil
}

// parseAndFindOwner parses canonicalName and looks for a controller owner-ref
// of Kind wantKind (matching the apis/v1alpha1 group/version) among ownerRefs.
// ok is false whenever the check should stop and report "not materialized"
// without a Get: an unparseable or local// name (no git-authority owner
// needed or possible), or no matching controller owner-ref present.
func parseAndFindOwner(canonicalName string, ownerRefs []metav1.OwnerReference, wantKind string) (n canonical.Name, ownerName string, ok bool) {
	n, err := canonical.Parse(canonicalName)
	if err != nil || n.IsLocal {
		return n, "", false
	}
	for _, ref := range ownerRefs {
		if ref.Kind == wantKind &&
			ref.APIVersion == v1.SchemeGroupVersion.String() &&
			ptr.Deref(ref.Controller, false) {
			return n, ref.Name, true
		}
	}
	return n, "", false
}
