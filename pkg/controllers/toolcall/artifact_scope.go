package toolcall

import (
	"fmt"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// validateInputArtifactRefs refuses a ToolCall whose spec.inputArtifacts name
// an artifact belonging to a different session.
//
// The artifact store's authorization model is "a token may read refs under its
// own <ns>/<session> key prefix" — pkg/x/debug enforces exactly that on the
// direct read path, scoping a per-session token to the ref's own scope. The
// hydrate path had no equivalent: it fetched whatever ref the ToolCall named
// with the OPERATOR's unrestricted store and materialized it into the sandbox.
//
// That made it the way around the model, and a cheaper one than it looks. It
// needs no credentials and no grant, so the use_token check never runs: name
// your OWN bundle in spec.session, point an input artifact at a sibling's
// stdout, and capture the hydrated path back out under a prefix your own token
// may read.
//
// Refs whose scope cannot be established are refused rather than passed
// through — a foreign store base, a malformed key, a nil store. An
// unparseable ref proves nothing about who owns it, and reading "unknown" as
// "fine" is the shape of the original defect.
func validateInputArtifactRefs(store artifactstore.Store, namespace, session string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if store == nil {
		return fmt.Errorf("no artifact store configured, so the scope of %d input artifact ref(s) cannot be established", len(refs))
	}
	want := namespace + "/" + session + "/"
	for _, ref := range refs {
		key, err := store.Key(artifactstore.Ref(ref))
		if err != nil {
			return fmt.Errorf("input artifact ref %q does not resolve in this artifact store, so it cannot be shown to belong to %s/%s: %w",
				ref, namespace, session, err)
		}
		if !strings.HasPrefix(key, want) {
			return fmt.Errorf("input artifact %q belongs to another session (key %q is not under %q): a session may only hydrate its own artifacts",
				ref, key, want)
		}
	}
	return nil
}

// inputArtifactRefs lifts the refs off the spec for validation.
func inputArtifactRefs(artifacts []spiceboxv1alpha1.InputArtifact) []string {
	out := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, a.ArtifactRef)
	}
	return out
}
