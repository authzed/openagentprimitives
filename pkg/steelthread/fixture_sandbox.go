package steelthread

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// rewriteSandboxDocs emits the three CRs a captured SANDBOX tool is synthesized
// from: the SpiceboxToolkit, the SpiceboxToolspec, and the SpiceboxClass.
//
// Numbered 02b/02c/02d for the same reason 02a-sidecartoolbox.yaml is: the
// harness applies a directory's *.yaml in sorted order, and 03-agent.yaml
// declares spec.toolBundles pointing at these. Emitted in reference order among
// themselves — a toolspec names a toolkit, a class names its toolspecs — so a
// reader meets each object before the thing that refers to it.
//
// Each is sorted by name, for RewriteFixture's byte-identical-on-rerun promise:
// a caller that gathered these by following refs has whatever order the class
// happened to list them in.
//
// # What is rewritten
//
// spec.secretOutput is DROPPED from every toolspec, and it is the one rewrite
// here. A producer's secret output is not tool output at all: the runner diverts
// the value out-of-band and replaces the model-facing result with a
// "<secret-output …>" handle line, so what the transcript records is the handle,
// not something the tool printed. At replay there is nothing to divert — the
// value came from a file the real tool wrote in a real pod, and a canned stdout
// cannot produce one — so a toolspec still declaring the output composes an
// error ("declared file … was not produced") in place of the result.
//
// The gate that output opened is dropped on the other side too: see
// rewriteSidecarToolboxes on why spec.secretInputs goes. The two are the same
// decision made at both ends of one mechanism — at replay the sidecar is the
// fake MCP stub, so nothing needs the credential, and so nothing needs to
// produce it.
//
// Everything else rides through unchanged. In particular allowSubcommands,
// constraints and writesRelationships are exactly what a captured session's
// authorization behavior came from, and the SpiceboxClass's image names an
// image no fixture ever pulls (the harness creates no sandbox pod; the driver
// stamps the SpiceboxSession's status directly), so substituting one would be
// inventing a fact.
func rewriteSandboxDocs(in FixtureInput) ([]FixtureFile, error) {
	var files []FixtureFile

	if len(in.Toolkits) > 0 {
		docs, err := rewriteToolkits(in.Toolkits)
		if err != nil {
			return nil, err
		}
		doc, err := marshalDocs(docs)
		if err != nil {
			return nil, err
		}
		files = append(files, FixtureFile{Name: "02b-spiceboxtoolkit.yaml", YAML: doc})
	}

	if len(in.Toolspecs) > 0 {
		docs, err := rewriteToolspecs(in.Toolspecs)
		if err != nil {
			return nil, err
		}
		doc, err := marshalDocs(docs)
		if err != nil {
			return nil, err
		}
		files = append(files, FixtureFile{Name: "02c-spiceboxtoolspec.yaml", YAML: doc})
	}

	if len(in.SandboxClasses) > 0 {
		docs, err := rewriteSandboxClasses(in.SandboxClasses)
		if err != nil {
			return nil, err
		}
		doc, err := marshalDocs(docs)
		if err != nil {
			return nil, err
		}
		files = append(files, FixtureFile{Name: "02d-spiceboxclass.yaml", YAML: doc})
	}

	return files, nil
}

func rewriteToolkits(live []*spiceboxv1alpha1.SpiceboxToolkit) ([]any, error) {
	sorted, err := sortedByName(live, func(t *spiceboxv1alpha1.SpiceboxToolkit) string { return t.Name },
		"SpiceboxToolkit")
	if err != nil {
		return nil, err
	}
	docs := make([]any, 0, len(sorted))
	for _, tk := range sorted {
		docs = append(docs, &spiceboxv1alpha1.SpiceboxToolkit{
			TypeMeta:   typeMeta("SpiceboxToolkit"),
			ObjectMeta: fixtureMeta(tk.Name),
			Spec:       *tk.Spec.DeepCopy(),
		})
	}
	return docs, nil
}

func rewriteToolspecs(live []*spiceboxv1alpha1.SpiceboxToolspec) ([]any, error) {
	sorted, err := sortedByName(live, func(t *spiceboxv1alpha1.SpiceboxToolspec) string { return t.Name },
		"SpiceboxToolspec")
	if err != nil {
		return nil, err
	}
	docs := make([]any, 0, len(sorted))
	for _, ts := range sorted {
		spec := ts.Spec.DeepCopy()
		spec.SecretOutput = nil // see the doc comment above
		docs = append(docs, &spiceboxv1alpha1.SpiceboxToolspec{
			TypeMeta:   typeMeta("SpiceboxToolspec"),
			ObjectMeta: fixtureMeta(ts.Name),
			Spec:       *spec,
		})
	}
	return docs, nil
}

func rewriteSandboxClasses(live []*spiceboxv1alpha1.SpiceboxClass) ([]any, error) {
	sorted, err := sortedByName(live, func(c *spiceboxv1alpha1.SpiceboxClass) string { return c.Name },
		"SpiceboxClass")
	if err != nil {
		return nil, err
	}
	docs := make([]any, 0, len(sorted))
	for _, cls := range sorted {
		docs = append(docs, &spiceboxv1alpha1.SpiceboxClass{
			TypeMeta:   typeMeta("SpiceboxClass"),
			ObjectMeta: fixtureMeta(cls.Name),
			Spec:       *cls.Spec.DeepCopy(),
		})
	}
	return docs, nil
}

// sortedByName drops nils and orders by metadata.name, refusing an entry that
// has none.
//
// An unnamed CR would marshal to a document the harness applies as a nameless
// object, and the apply fails many steps later with nothing pointing back at
// the capture that emitted it — the same failure rewriteSidecarToolboxes
// refuses an empty Ref for.
func sortedByName[T any](live []*T, name func(*T) string, kind string) ([]*T, error) {
	out := make([]*T, 0, len(live))
	for _, v := range live {
		if v == nil {
			continue
		}
		if name(v) == "" {
			return nil, fmt.Errorf("steelthread: RewriteFixture: a gathered %s has no metadata.name, "+
				"so the AgentClass's toolBundles cannot resolve to an emitted CR", kind)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
	return out, nil
}

// sandboxTools maps each toolBundle's LLM-facing PREFIX to the class-tool names
// its toolspecs reach — the sandbox counterpart of an MCP server's prefix and
// server-side tool list, in the same shape declaredTools returns.
//
// The resolution mirrors sandbox.Synthesize exactly, and the indirection is the
// part that is easy to get wrong: a toolspec is matched to a class tool by the
// toolspec's TOOLKIT NAME, not by the toolspec's own name. So
// `toolBundles[{name: sre-discovery, class: sre-sandbox, toolspecs: [sre-discovery]}]`
// plus a toolspec whose toolkit is `sre` plus a class tool named `sre` yields
// the LLM-facing `sre-discovery_sre`. Deriving it any other way produces a name
// that looks plausible and matches nothing in the transcript.
//
// A bundle whose class or toolspec was not gathered contributes an empty tool
// list rather than being skipped: the PREFIX still has to exist, or every call
// to that bundle's tools falls through to "not a declared tool" and the capture
// refuses a session it could otherwise take.
func sandboxTools(in FixtureInput) map[string][]string {
	if in.Class == nil {
		return nil
	}
	classByName := map[string]*spiceboxv1alpha1.SpiceboxClass{}
	for _, c := range in.SandboxClasses {
		if c != nil {
			classByName[c.Name] = c
		}
	}
	specByName := map[string]*spiceboxv1alpha1.SpiceboxToolspec{}
	for _, s := range in.Toolspecs {
		if s != nil {
			specByName[s.Name] = s
		}
	}

	out := map[string][]string{}
	for _, b := range in.Class.Spec.ToolBundles {
		if b.Name == "" {
			continue
		}
		var tools []string
		cls := classByName[b.Class]
		for _, tsName := range b.Toolspecs {
			ts := specByName[tsName]
			if ts == nil || cls == nil {
				continue
			}
			for _, ct := range cls.Spec.Tools {
				if ct.Name == ts.Spec.Toolkit.Name {
					tools = append(tools, ct.Name)
					break
				}
			}
		}
		slices.Sort(tools)
		out[b.Name] = slices.Compact(tools)
	}
	return out
}

// sandboxOutputKey is the ToolOutputs key a captured sandbox result is filed
// under: the LLM-facing name, "<bundle>_<class tool>".
//
// NOT the bare class-tool name, which is what an MCP entry's server-side key
// would suggest. One SpiceboxClass is one container image, and several
// toolBundles routinely narrow the SAME binary differently — the session this
// was built for has `sre-discovery` and `sre-fetch` both reaching class tool
// `sre` — so the bare name collides and the two recorded results would
// overwrite each other in a flat map.
//
// The driver reads both spellings, preferring this one, so a hand-authored
// bundle whose class tool is reached by exactly one toolBundle can keep saying
// just "gh".
func sandboxOutputKey(prefix, classTool string) string {
	return prefix + "_" + classTool
}

// splitSandboxName is splitMCPName for the sandbox prefixes: it reports whether
// an LLM-facing name belongs to one of the class's toolBundles, and returns the
// class-tool suffix when it does.
func splitSandboxName(llmFacing string, prefixes []string) (bool, string) {
	for _, p := range prefixes {
		if after, ok := strings.CutPrefix(llmFacing, p+"_"); ok {
			return true, after
		}
	}
	return false, ""
}
