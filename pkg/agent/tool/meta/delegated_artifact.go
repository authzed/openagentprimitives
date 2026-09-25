package meta

import (
	"fmt"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// delegatedHandleSep joins a delegation's name to an artifact handle the child
// under it returned, forming the one handle a parent passes to
// respond_to_user's `attached` for an artifact it did not itself render.
//
// A composite handle rather than a bare render name, because the parent's
// authority to attach a child's artifact is not a property of the render: it
// comes from the DELEGATION, whose controller-owned status records both that
// this session is the parent and which handles the child returned. Naming the
// delegation in the handle is what lets respond_to_user find that record with
// a single Get — the per-session runner Role grants `get` on subagentrequests
// and deliberately no `list`, so a handle that named only the render would
// leave the tool with a claim it has no way to check.
//
// "/" is the separator because no handle a session mints for itself can carry
// one BEFORE a "#": render CR names and artifact ids are object names, which
// have no slash. A revision TAG may contain one (artifact_prepare rejects only
// spaces and "#"), so "artifact-abc#v1/final" is a legal ordinary handle —
// which is why cutDelegatedArtifactHandle refuses to read a "#" on the left of
// the split.
const delegatedHandleSep = "/"

// delegatedArtifactHandle mints the handle for artifactID as returned under
// the delegation named delegation.
func delegatedArtifactHandle(delegation, artifactID string) string {
	return delegation + delegatedHandleSep + artifactID
}

// cutDelegatedArtifactHandle splits a delegated handle back into the
// delegation and the artifact handle the child returned under it. ok is false
// for anything that is not one — an ordinary render name, an artifact id, a
// tagged revision — so a caller falls through to its own resolution unchanged.
//
// Both halves must be non-empty: "/ar-x" names no delegation and "subreq-x/"
// names no artifact, and treating either as a delegated handle would turn a
// typo into a lookup for the empty string.
//
// A "#" on the left disqualifies the split, and that is not a nicety: a tag
// may contain "/", so "artifact-abc#v1/final" would otherwise be read as
// delegation "artifact-abc#v1" and refused as a delegation that does not
// exist — an ordinary tagged attachment failing with an error about
// delegation. The "#" always precedes the tag, so its presence on the left is
// exactly the signal that the slash belongs to a tag.
func cutDelegatedArtifactHandle(h string) (delegation, artifactID string, ok bool) {
	delegation, artifactID, ok = strings.Cut(h, delegatedHandleSep)
	if !ok || delegation == "" || artifactID == "" || strings.Contains(delegation, "#") {
		return "", "", false
	}
	return delegation, artifactID, true
}

// describeReturnedArtifacts renders the artifact handles a child returned as
// the trailing block of the delegate / reply_to_subagent tool result, or ""
// when it returned none.
//
// It is appended INSIDE the untrusted result rather than returned as a
// separate trusted note, matching the framing sentence waitForNext's
// AwaitingParent arm already carries: every description here is the child's
// own words, and widening what content inspection sees is harmless where
// narrowing it is the laundering path the delegation surface exists to close.
//
// The text is imperative about what has NOT happened. A child returning an
// artifact is delivery TO THIS AGENT and to nobody else; the observed failure
// this whole path addresses is an agent that believed something reached a
// person because a render existed.
func describeReturnedArtifacts(delegation string, artifacts []v1.ResultArtifact) string {
	if len(artifacts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nThe agent returned %d artifact(s) to you. Nothing has been shown to anyone: these are handles, "+
		"and an artifact reaches a person only when you name one in respond_to_user's `attached` array. "+
		"Use each handle exactly as written below — it is only valid from this session.\n", len(artifacts))
	for _, a := range artifacts {
		fmt.Fprintf(&b, "- %q", delegatedArtifactHandle(delegation, a.ID))
		if a.Description != "" {
			fmt.Fprintf(&b, " — %s", a.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}
