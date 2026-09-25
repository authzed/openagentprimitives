package subjectresolve

import (
	"context"
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// triggerAuthorRef is the literal reference form this resolver claims.
const triggerAuthorRef = "trigger-author"

// noTriggerOwnerReason is returned whenever this session has no recorded
// trigger owner to resolve — whether because it wasn't opened by a trigger,
// the annotation is missing, or no session context was wired in at all.
// Deliberately the SAME string in every one of those cases: an agent
// relaying it should not be able to distinguish "no session" from "a
// trigger session with no recorded owner" — both mean the reference cannot
// be honored right now.
const noTriggerOwnerReason = "this session was not opened by a trigger with a recorded owner"

// triggerAuthorResolver resolves "trigger-author" to the platform user
// linked to whoever opened this session's trigger (e.g. a pull request's
// author), by reading the AnnotationTriggerOwnerSubject value channelsd
// wrote from a verified delivery and recursing into the generic resource
// path (resolveTypeID) on its "<type>:<id>" half.
type triggerAuthorResolver struct{}

func (triggerAuthorResolver) Usage() (string, string) {
	return triggerAuthorRef, "the user linked to whoever opened this session's trigger (e.g. the pull request author)"
}

func (triggerAuthorResolver) TryResolve(ctx context.Context, ref string, env Env) (Resolution, bool, error) {
	if ref != triggerAuthorRef {
		return Resolution{}, false, nil
	}
	if env.SessionAnnotations == nil {
		return Resolution{Reason: noTriggerOwnerReason}, true, nil
	}
	annotations, err := env.SessionAnnotations(ctx)
	if err != nil {
		return Resolution{}, true, err
	}
	raw := annotations[spiceboxv1alpha1.AnnotationTriggerOwnerSubject]
	if raw == "" {
		return Resolution{Reason: noTriggerOwnerReason}, true, nil
	}

	// raw is "<type>:<id>#<relation>" (e.g. "github_user:4172237#user"); the
	// relation half is meaningful to the SpiceDB ownership tuple it was
	// written for, not to a user lookup — strip it and resolve the resource
	// half through the same path a "<type>:<id>" reference would take.
	typeID, _, _ := strings.Cut(raw, "#")
	res, matched, err := resolveTypeID(ctx, typeID, env)
	if err != nil {
		return Resolution{}, true, err
	}
	if !matched {
		return Resolution{Reason: fmt.Sprintf("this session's recorded trigger owner (%q) is not a resolvable resource reference", raw)}, true, nil
	}
	return res, true, nil
}
