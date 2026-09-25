package oap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/kubeyaml"
)

// bundleGK identifies an allowed bundle resource by API group + Kind. Group
// is the bare group ("" for the core group), never a group/version — the
// version a bundle declares is irrelevant to whether the Kind is permitted.
type bundleGK struct {
	Group string
	Kind  string
}

// allowedBundleKinds is the CLOSED set of (group, Kind) a .oap bundle's
// manifests stream may carry: the agent graph plus its skills plus ConfigMaps.
//
// SECURITY INVARIANT, enforced by Bundle.Validate, which every install path
// calls — most importantly admind's install endpoint, which runs
// install.Install under the elevated operator ServiceAccount and force-applies
// (ForceOwnership) every bundled CR. Without this allowlist an install_agent
// holder could smuggle a privileged Pod, or a ServiceAccount + cluster-admin
// (Cluster)RoleBinding, into a bundle alongside a throwaway AgentClass and have
// the operator apply it: confused-deputy privilege escalation via one
// authenticated POST. (The `oap` CLI applies under the caller's own kubeconfig,
// so it is immune — but the invariant lives here so inspect / lint reject a
// malformed bundle too.)
//
// Skill and SkillSource ARE allowed: a skill is part of the agent's
// definition and installs with it, whether hand-authored inline (Skill) or
// fetched from git at install time (SkillSource). Channel is DELIBERATELY
// absent — it is per-install deployment config (which workspace, which
// tokens) selected in the install UI, never baked into a portable, shareable
// agent container. Secret is DELIBERATELY absent — install materializes
// Secrets from answered secret questions (install.SecretSpec), so allowing
// one here would only open an avenue to plant arbitrary Secret content under
// the operator's identity.
var allowedBundleKinds = map[bundleGK]bool{
	{spiceboxv1alpha1.GroupName, "AgentClass"}:       true,
	{spiceboxv1alpha1.GroupName, "AgentIdentity"}:    true,
	{spiceboxv1alpha1.GroupName, "AgentUI"}:          true,
	{spiceboxv1alpha1.GroupName, "MCPServer"}:        true,
	{spiceboxv1alpha1.GroupName, "SidecarToolbox"}:   true,
	{spiceboxv1alpha1.GroupName, "Skill"}:            true,
	{spiceboxv1alpha1.GroupName, "SkillSource"}:      true,
	{spiceboxv1alpha1.GroupName, "SpiceboxClass"}:    true,
	{spiceboxv1alpha1.GroupName, "SpiceboxToolkit"}:  true,
	{spiceboxv1alpha1.GroupName, "SpiceboxToolspec"}: true,
	{"", "ConfigMap"}: true,
}

// AllowedBundleKindNames returns the agentprimitives.authzed.com Kind names a
// .oap bundle may carry, sorted. Core kinds (ConfigMap) are excluded — a fixed
// pair install.managedKinds already names.
//
// It exists so install's managedKinds coverage test asserts against the live
// allowlist rather than a hand-copied list that drifts out of sync with it —
// the failure that let AgentUI become bundleable without Uninstall reaping it.
func AllowedBundleKindNames() []string {
	names := make([]string, 0, len(allowedBundleKinds))
	for gk := range allowedBundleKinds {
		if gk.Group != spiceboxv1alpha1.GroupName {
			continue
		}
		names = append(names, gk.Kind)
	}
	sort.Strings(names)
	return names
}

// Bundle is the in-memory form of a .oap: the manifest, the multi-document CR
// YAML (valid CRs with sentinel defaults), named binary assets, and optional
// long-form README documentation.
type Bundle struct {
	Manifest     *Manifest
	Manifests    []byte            // multi-doc YAML stream
	Assets       map[string][]byte // path → bytes, e.g. "assets/logo.png"
	Readme       []byte            // raw README.md markdown; nil when absent
	Dependencies []*Dependency     `json:"-"`
}

// CRs decodes the manifests stream into unstructured objects. Documents that
// carry no resource — empty, comment-only, or kind-less — are skipped by the
// splitter, so every element here has a Kind for checkAllowedKinds to rule on.
func (b *Bundle) CRs() ([]*unstructured.Unstructured, error) {
	return kubeyaml.Split(b.Manifests)
}

// Validate checks the manifest, questions, that the CR stream decodes, and that
// every non-secret binding target resolves to a CR present in the bundle.
func (b *Bundle) Validate() error {
	if b.Manifest == nil {
		return fmt.Errorf("bundle has no manifest")
	}
	if err := b.Manifest.Validate(); err != nil {
		return err
	}
	if err := b.Manifest.ValidateQuestions(); err != nil {
		return err
	}
	crs, err := b.CRs()
	if err != nil {
		return fmt.Errorf("decode bundled manifests: %w", err)
	}
	// Fail closed on a disallowed Kind BEFORE any binding/logo check: it is a
	// security reject, not a shape nit (see allowedBundleKinds).
	if err := checkAllowedKinds(crs); err != nil {
		return err
	}
	// A .oap describes exactly one agent: the sole AgentClass is the canonical
	// source every derived field (DeriveInherited) inherits from. Zero or
	// multiple is ambiguous and rejected.
	if _, err := singleAgentClass(crs); err != nil {
		return err
	}
	idx := indexByKindName(crs)
	for _, q := range b.Manifest.Questions {
		if q.Type == QSecret {
			continue
		}
		for _, bd := range q.Binding {
			tgt, err := ParseTarget(bd.Target)
			if err != nil {
				return fmt.Errorf("question %q: %w", q.Name, err)
			}
			if idx[tgt.Kind+"/"+tgt.Name] == nil {
				return fmt.Errorf("question %q: binding target %s/%s is not present in the bundle manifests", q.Name, tgt.Kind, tgt.Name)
			}
		}
	}
	if logo := b.Manifest.Agent.Logo; logo != "" {
		if _, ok := b.Assets[logo]; !ok {
			return fmt.Errorf("agent.logo %q is not present in assets", logo)
		}
	}
	return nil
}

// checkAllowedKinds rejects the whole bundle if any decoded CR's (group, Kind)
// is outside allowedBundleKinds. Matching is on group AND Kind, so a Kind that
// merely shares a name with an allowed one but lives in another group is still
// refused. An unparseable or absent apiVersion fails closed — its group cannot
// be established, so it cannot be on the allowlist.
func checkAllowedKinds(crs []*unstructured.Unstructured) error {
	for _, cr := range crs {
		gv, err := schema.ParseGroupVersion(cr.GetAPIVersion())
		if err != nil {
			return fmt.Errorf("oap bundle CR %q has an unparseable apiVersion %q: %w", cr.GetKind(), cr.GetAPIVersion(), err)
		}
		if !allowedBundleKinds[bundleGK{Group: gv.Group, Kind: cr.GetKind()}] {
			display := cr.GetKind()
			if gv.Group != "" {
				display = gv.Group + "/" + cr.GetKind()
			}
			return fmt.Errorf("oap bundle contains a disallowed resource kind %q (a .oap may carry only the agent graph, its skills, and ConfigMaps)", display)
		}
	}
	return nil
}

// FromFolder builds a Bundle from a source-project directory:
//
//	<dir>/oap.yaml            – the manifest + questions
//	<dir>/manifests/*.yaml    – CR YAML (concatenated, document order = lexical)
//	<dir>/assets/**           – binary assets, keyed by path relative to <dir>
//	<dir>/README.md           – optional long-form docs (Bundle.Readme)
func FromFolder(dir string) (*Bundle, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle root: %w", err)
	}
	budget := &graphBudget{}
	bundle, err := fromFolder(root, root, nil, budget)
	if err != nil {
		return nil, err
	}
	if err := ValidateDependencyGraph(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func fromFolder(root, dir string, path DependencyPath, budget *graphBudget) (*Bundle, error) {
	if err := budget.enter(path); err != nil {
		return nil, qualifyDependencyError(budget.rootName, path, err)
	}
	bundle, err := loadFolder(dir, budget)
	if err != nil {
		if len(path) == 0 {
			return nil, err
		}
		return nil, qualifyDependencyError(budget.rootName, path, err)
	}
	if len(path) == 0 {
		budget.rootName = bundle.Manifest.Agent.Name
	} else {
		path = cloneDependencyPath(path)
		path[len(path)-1] = bundle.Manifest.Agent.Name
	}
	if err := bundle.Validate(); err != nil {
		if len(path) == 0 {
			return nil, err
		}
		return nil, qualifyDependencyError(budget.rootName, path, err)
	}
	for i, req := range bundle.Manifest.Requires.Agents {
		if req.Path == "" {
			return nil, qualifyDependencyError(budget.rootName, path,
				fmt.Errorf("requires.agents[%d]: folder source dependency requires path", i))
		}
		tentativePath := appendDependencyPath(path, filepath.Base(req.Path))
		childDir, err := secureDependencyDir(root, dir, req.Path)
		if err != nil {
			return nil, qualifyDependencyError(budget.rootName, tentativePath, err)
		}
		child, err := fromFolder(root, childDir, tentativePath, budget)
		if err != nil {
			return nil, err
		}
		childPath := appendDependencyPath(path, child.Manifest.Agent.Name)
		bundle.Dependencies = append(bundle.Dependencies, &Dependency{
			Descriptor: req,
			Path:       childPath,
			Bundle:     child,
		})
	}
	return bundle, nil
}

func secureDependencyDir(root, parent, relative string) (string, error) {
	current := parent
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect dependency path %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("dependency path %q contains symlink component %q", relative, component)
		}
	}
	rel, err := filepath.Rel(root, current)
	if err != nil {
		return "", fmt.Errorf("resolve dependency path %q: %w", relative, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("dependency path %q escapes bundle root", relative)
	}
	return current, nil
}

func loadFolder(dir string, budget *graphBudget) (*Bundle, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "oap.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read oap.yaml: %w", err)
	}
	if err := budget.addExtracted(int64(len(manifestBytes))); err != nil {
		return nil, err
	}
	m, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}

	crFiles, err := filepath.Glob(filepath.Join(dir, "manifests", "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob manifests: %w", err)
	}
	sort.Strings(crFiles)
	var stream bytes.Buffer
	for _, f := range crFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		if err := budget.addExtracted(int64(len(data))); err != nil {
			return nil, err
		}
		if stream.Len() > 0 {
			stream.WriteString("\n---\n")
		}
		stream.Write(bytes.TrimRight(data, "\n"))
	}

	assets := map[string][]byte{}
	assetRoot := filepath.Join(dir, "assets")
	if _, err := os.Stat(assetRoot); err == nil {
		err = filepath.Walk(assetRoot, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := budget.addExtracted(int64(len(data))); err != nil {
				return err
			}
			assets[filepath.ToSlash(rel)] = data
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk assets: %w", err)
		}
	}

	var readme []byte
	if data, err := os.ReadFile(filepath.Join(dir, "README.md")); err == nil {
		readme = data
		if err := budget.addExtracted(int64(len(data))); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read README.md: %w", err)
	}

	bundle := &Bundle{Manifest: m, Manifests: stream.Bytes(), Assets: assets, Readme: readme}

	// Inherit the derived fields from the bundled AgentClass graph instead of
	// repeating them in oap.yaml. An authored copy of any derived field is a
	// fail-closed error naming the manifest source — one source of truth.
	crs, err := bundle.CRs()
	if err != nil {
		return nil, fmt.Errorf("decode bundled manifests: %w", err)
	}
	agent, requires, err := DeriveInherited(crs, logr.Discard())
	if err != nil {
		return nil, err
	}
	if err := rejectDerivedFields(m, agent.Name); err != nil {
		return nil, err
	}
	m.Agent.Name = agent.Name
	m.Agent.DisplayName = agent.DisplayName
	m.Agent.Description = agent.Description
	m.Requires.Skills = requires.Skills
	m.Requires.Secrets = requires.Secrets
	linkDerivedSecretsToAuthoredQuestions(m)
	return bundle, nil
}

// linkDerivedSecretsToAuthoredQuestions points each derived RequiredSecret at an
// authored type=secret question whose createSecret materializes it (same Secret
// name). Derived secrets carry no question pointer, so without this install
// would synthesize its own collector (RequiredSecretQuestions) for a Secret the
// bundle already asks for through an authored question — prompting for one
// credential twice. Linking makes the authored question, with its own prompt and
// description, the sole collector; the cluster-export path sets this pointer
// itself as it generates the questions.
func linkDerivedSecretsToAuthoredQuestions(m *Manifest) {
	for i := range m.Requires.Secrets {
		rs := &m.Requires.Secrets[i]
		if rs.Question != "" {
			continue
		}
		for _, q := range m.Questions {
			if q.Type == QSecret && q.Secret != nil && q.Secret.CreateSecret != nil &&
				q.Secret.CreateSecret.Name == rs.Name {
				rs.Question = q.Name
				break
			}
		}
	}
}

// rejectDerivedFields fails closed when the authored oap.yaml sets a field that
// FromFolder derives from the bundled AgentClass graph. className names the
// canonical source in the error so the fix ("remove it") is obvious.
func rejectDerivedFields(m *Manifest, className string) error {
	var set []string
	if m.Agent.Name != "" {
		set = append(set, "agent.name")
	}
	if m.Agent.DisplayName != "" {
		set = append(set, "agent.displayName")
	}
	if m.Agent.Description != "" {
		set = append(set, "agent.description")
	}
	if len(m.Requires.Skills) > 0 {
		set = append(set, "requires.skills")
	}
	if len(m.Requires.Secrets) > 0 {
		set = append(set, "requires.secrets")
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf("oap.yaml sets %s, but these are derived from the bundled AgentClass %q and its dependency graph; remove them from oap.yaml (they are inherited)",
		strings.Join(set, ", "), className)
}

// assetPaths returns the asset keys in deterministic order (used by packing).
func (b *Bundle) assetPaths() []string {
	keys := make([]string, 0, len(b.Assets))
	for k := range b.Assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
