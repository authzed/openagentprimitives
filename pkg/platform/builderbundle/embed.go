// Package builderbundle assembles the builder .oap: the locked-down
// agent-builder AgentClass, its workshop SidecarToolbox, the workshop
// AgentUI, its page compiled from src/ui/page.tsx, and the eight
// builder-phase Skills, all embedded at build time from src/. No temp dir, no
// committed binary — Bundle() reads the embedded FS, folds each SKILL.md into
// a Skill CR, and returns a *oap.Bundle that has already passed Validate().
package builderbundle

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillmd"
)

//go:embed all:src
var srcFS embed.FS

// Bundle assembles the embedded builder .oap: it reads src/oap.yaml plus the
// hand-written manifests under src/manifests, folds each src/skills/*/SKILL.md
// into a Skill CR, and returns a validated *oap.Bundle.
func Bundle() (*oap.Bundle, error) {
	manifestYAML, err := fs.ReadFile(srcFS, "src/oap.yaml")
	if err != nil {
		return nil, fmt.Errorf("read oap.yaml: %w", err)
	}
	m, err := oap.ParseManifest(manifestYAML)
	if err != nil {
		return nil, err
	}

	// The workshop page, compiled by `mage ui:compile` from src/ui/page.tsx.
	// The absence of either file means the compile step never ran for this
	// checkout; a sidecar that no longer matches the page means it ran before
	// the last edit, and splicing the old view would ship a page nobody wrote.
	// Both are refused here, before the splice.
	page, err := fs.ReadFile(srcFS, pageSourcePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pageSourcePath, err)
	}
	sha, err := fs.ReadFile(srcFS, compiledViewShaPath)
	if err != nil {
		return nil, fmt.Errorf("read %s (run mage ui:compile): %w", compiledViewShaPath, err)
	}
	view, err := fs.ReadFile(srcFS, compiledViewPath)
	if err != nil {
		return nil, fmt.Errorf("read %s (run mage ui:compile): %w", compiledViewPath, err)
	}
	if err := checkCompiledViewFresh(page, sha); err != nil {
		return nil, err
	}

	// Concatenate the hand-written CR manifests (deterministic order),
	// splicing the compiled view into the AgentUI's spec.view as we go.
	var docs [][]byte
	entries, err := fs.ReadDir(srcFS, "src/manifests")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := fs.ReadFile(srcFS, path.Join("src/manifests", name))
		if err != nil {
			return nil, err
		}
		spliced, err := spliceCompiledView(b, view)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", name, err)
		}
		docs = append(docs, spliced)
	}

	// Fold each SKILL.md into a Skill CR.
	skillDirs, err := fs.ReadDir(srcFS, "src/skills")
	if err != nil {
		return nil, err
	}
	skillNames := make([]string, 0, len(skillDirs))
	for _, d := range skillDirs {
		skillNames = append(skillNames, d.Name())
	}
	sort.Strings(skillNames)
	for _, dir := range skillNames {
		raw, err := fs.ReadFile(srcFS, path.Join("src/skills", dir, "SKILL.md"))
		if err != nil {
			return nil, err
		}
		doc, err := skillmd.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", dir, err)
		}
		cr, err := skillCR(doc)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", dir, err)
		}
		crYAML, err := yaml.Marshal(cr)
		if err != nil {
			return nil, err
		}
		docs = append(docs, crYAML)
	}

	b := &oap.Bundle{Manifest: m, Manifests: joinYAMLDocs(docs)}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("assembled builder bundle invalid: %w", err)
	}
	return b, nil
}

// skillCR builds the Skill CR for one parsed SKILL.md, mirroring
// cmd/oap/internal/skillcmd/create.go's SkillSpec construction: every
// builder-phase skill is a cluster-scoped, hand-authored, local-authority
// skill, so its canonical name is "local//<frontmatter name>" and its CR name
// is the canonical SafeSlug (deterministic — a hash of the canonical name —
// so re-assembling the bundle byte-for-byte re-derives the same CR name).
func skillCR(doc skillmd.Doc) (*spiceboxv1alpha1.Skill, error) {
	canonicalName := "local//" + doc.Frontmatter.Name
	n, err := canonical.Parse(canonicalName)
	if err != nil {
		return nil, fmt.Errorf("canonical name %q: %w", canonicalName, err)
	}
	cr := &spiceboxv1alpha1.Skill{
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: canonicalName,
			DisplayName:   doc.Frontmatter.Name,
			Description:   doc.Frontmatter.Description,
			Body:          doc.Body,
			Frontmatter: spiceboxv1alpha1.SkillFrontmatter{
				Name:          doc.Frontmatter.Name,
				License:       doc.Frontmatter.License,
				Compatibility: doc.Frontmatter.Compatibility,
				Metadata:      doc.Frontmatter.Metadata,
				AllowedTools:  doc.Frontmatter.AllowedTools,
			},
		},
	}
	cr.TypeMeta = metav1.TypeMeta{
		APIVersion: spiceboxv1alpha1.GroupName + "/v1alpha1",
		Kind:       "Skill",
	}
	cr.Name = n.SafeSlug()
	return cr, nil
}

// joinYAMLDocs concatenates CR documents into one multi-doc YAML stream using
// the same "\n---\n" separator oap.Bundle.FromFolder writes and kubeyaml.Split
// (via utilyaml.NewYAMLOrJSONDecoder) reads back.
func joinYAMLDocs(docs [][]byte) []byte {
	var out []byte
	for _, d := range docs {
		if len(out) > 0 {
			out = append(out, []byte("\n---\n")...)
		}
		out = append(out, trimTrailingNewlines(d)...)
	}
	return out
}

func trimTrailingNewlines(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}
