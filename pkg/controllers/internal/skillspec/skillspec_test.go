package skillspec_test

// Coverage for the type-agnostic leaf helpers shared by the SkillSource and
// ClusterSkillSource controllers.
//
// Equal is the upsert gate, and it is wrong in both directions:
//   - a false positive (reporting two different specs equal) leaves a changed
//     skill un-updated, so agents keep loading stale instructions;
//   - a false negative (reporting two identical specs different) rewrites the
//     CR on every reconcile, which is the SSA-idempotency violation AGENTS.md
//     calls out — field ownership churns and every watcher re-reconciles.
//
// So each field gets a row proving a difference in it is DETECTED, plus the
// nil-vs-empty cases the four pointer comparators handle.
//
// The Reconcile body and SetPinned are deliberately not retested here: they are
// already exercised through pkg/controllers/skill and pkg/controllers/clusterskill.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillmd"
)

// baseSpec is a fully-populated SkillSpec. Every Equal case starts from a copy
// and mutates exactly one thing, so a row's name names the only difference.
func baseSpec() v1.SkillSpec {
	return v1.SkillSpec{
		CanonicalName: "example.com/demo-org/demo-repo//skills/demo-skill@v1.0.0",
		DisplayName:   "Demo Skill",
		Description:   "does a demo thing",
		Body:          "# Demo\n\nbody text\n",
		Frontmatter: v1.SkillFrontmatter{
			Name:          "demo-skill",
			License:       "Apache-2.0",
			Compatibility: ">=1.0",
			Metadata:      map[string]string{"team": "platform"},
			AllowedTools:  "read_file write_file",
		},
		Source: &v1.SkillProvenance{
			RepoLocator: "example.com/demo-org/demo-repo",
			Subpath:     "skills/demo-skill",
			Ref:         "v1.0.0",
			ResolvedSHA: "abc123",
			SourceName:  "demo-source",
		},
		Bundle: &v1.SkillBundleRef{Digest: "sha256:aaa", CacheKey: "key-1"},
		RepoInstructions: &v1.SkillRepoInstructions{
			SourceFile: "AGENTS.md",
			Content:    "repo instructions",
			Truncated:  false,
		},
	}
}

func TestEqual_IdenticalSpecs(t *testing.T) {
	assert.True(t, skillspec.Equal(baseSpec(), baseSpec()),
		"two identical specs must compare equal, or the controller rewrites the CR every reconcile")
}

// TestEqual_DetectsEveryFieldDifference walks every field Equal is responsible
// for. A field missing from Equal's comparison is invisible here only if the
// row for it is missing too — so each row is a field, not a scenario.
func TestEqual_DetectsEveryFieldDifference(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *v1.SkillSpec)
	}{
		{name: "CanonicalName differs: not equal", mutate: func(s *v1.SkillSpec) { s.CanonicalName = "other" }},
		{name: "DisplayName differs: not equal", mutate: func(s *v1.SkillSpec) { s.DisplayName = "Other" }},
		{name: "Description differs: not equal", mutate: func(s *v1.SkillSpec) { s.Description = "other" }},
		{name: "Body differs: not equal", mutate: func(s *v1.SkillSpec) { s.Body = "# Other\n" }},

		{name: "Frontmatter.Name differs: not equal", mutate: func(s *v1.SkillSpec) { s.Frontmatter.Name = "other" }},
		{name: "Frontmatter.License differs: not equal", mutate: func(s *v1.SkillSpec) { s.Frontmatter.License = "MIT" }},
		{name: "Frontmatter.Compatibility differs: not equal", mutate: func(s *v1.SkillSpec) { s.Frontmatter.Compatibility = ">=2.0" }},
		{name: "Frontmatter.AllowedTools differs: not equal", mutate: func(s *v1.SkillSpec) { s.Frontmatter.AllowedTools = "read_file" }},
		{name: "Frontmatter.Metadata value differs: not equal", mutate: func(s *v1.SkillSpec) {
			s.Frontmatter.Metadata = map[string]string{"team": "other"}
		}},
		{name: "Frontmatter.Metadata gains a key: not equal", mutate: func(s *v1.SkillSpec) {
			s.Frontmatter.Metadata = map[string]string{"team": "platform", "extra": "x"}
		}},
		{name: "Frontmatter.Metadata loses its only key: not equal", mutate: func(s *v1.SkillSpec) {
			s.Frontmatter.Metadata = map[string]string{}
		}},
		{name: "Frontmatter.Metadata becomes nil: not equal", mutate: func(s *v1.SkillSpec) {
			s.Frontmatter.Metadata = nil
		}},
		{name: "Frontmatter.Metadata key renamed at equal length: not equal", mutate: func(s *v1.SkillSpec) {
			s.Frontmatter.Metadata = map[string]string{"crew": "platform"}
		}},

		{name: "Source.RepoLocator differs: not equal", mutate: func(s *v1.SkillSpec) { s.Source.RepoLocator = "other" }},
		{name: "Source.Subpath differs: not equal", mutate: func(s *v1.SkillSpec) { s.Source.Subpath = "other" }},
		{name: "Source.Ref differs: not equal", mutate: func(s *v1.SkillSpec) { s.Source.Ref = "v2.0.0" }},
		{name: "Source.ResolvedSHA differs: not equal", mutate: func(s *v1.SkillSpec) { s.Source.ResolvedSHA = "def456" }},
		{name: "Source.SourceName differs: not equal", mutate: func(s *v1.SkillSpec) { s.Source.SourceName = "other" }},
		{name: "Source becomes nil: not equal", mutate: func(s *v1.SkillSpec) { s.Source = nil }},

		{name: "Bundle.Digest differs: not equal", mutate: func(s *v1.SkillSpec) { s.Bundle.Digest = "sha256:bbb" }},
		{name: "Bundle.CacheKey differs: not equal", mutate: func(s *v1.SkillSpec) { s.Bundle.CacheKey = "key-2" }},
		{name: "Bundle becomes nil: not equal", mutate: func(s *v1.SkillSpec) { s.Bundle = nil }},

		{name: "RepoInstructions.SourceFile differs: not equal", mutate: func(s *v1.SkillSpec) { s.RepoInstructions.SourceFile = "CLAUDE.md" }},
		{name: "RepoInstructions.Content differs: not equal", mutate: func(s *v1.SkillSpec) { s.RepoInstructions.Content = "other" }},
		{name: "RepoInstructions.Truncated differs: not equal", mutate: func(s *v1.SkillSpec) { s.RepoInstructions.Truncated = true }},
		{name: "RepoInstructions becomes nil: not equal", mutate: func(s *v1.SkillSpec) { s.RepoInstructions = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseSpec()
			b := baseSpec()
			tc.mutate(&b)
			require.NotEqual(t, a, b, "the case must actually change something")

			assert.False(t, skillspec.Equal(a, b), "a changed field must be detected, or the CR is never updated")
			assert.False(t, skillspec.Equal(b, a), "Equal must be symmetric")
		})
	}
}

// TestEqual_PointerFieldNilHandling pins the four pointer comparators. Both-nil
// must be equal (a hand-authored skill has no Source, no Bundle, no
// RepoInstructions) and one-nil must not — the case where a nil/non-nil mixup
// would either strand an update or churn the object forever.
func TestEqual_PointerFieldNilHandling(t *testing.T) {
	cases := []struct {
		name  string
		strip func(s *v1.SkillSpec)
	}{
		{name: "Source", strip: func(s *v1.SkillSpec) { s.Source = nil }},
		{name: "Bundle", strip: func(s *v1.SkillSpec) { s.Bundle = nil }},
		{name: "RepoInstructions", strip: func(s *v1.SkillSpec) { s.RepoInstructions = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name+" nil on both sides: equal", func(t *testing.T) {
			a, b := baseSpec(), baseSpec()
			tc.strip(&a)
			tc.strip(&b)
			assert.True(t, skillspec.Equal(a, b),
				"a hand-authored skill legitimately has no "+tc.name+"; both-nil must be a no-op upsert")
		})
		t.Run(tc.name+" nil on one side only: not equal", func(t *testing.T) {
			a, b := baseSpec(), baseSpec()
			tc.strip(&b)
			assert.False(t, skillspec.Equal(a, b))
			assert.False(t, skillspec.Equal(b, a), "Equal must be symmetric across the nil boundary")
		})
	}

	t.Run("all three pointer fields nil on both sides: equal", func(t *testing.T) {
		a, b := baseSpec(), baseSpec()
		for _, s := range []*v1.SkillSpec{&a, &b} {
			s.Source, s.Bundle, s.RepoInstructions = nil, nil, nil
		}
		assert.True(t, skillspec.Equal(a, b))
	})
}

// TestEqual_EmptyMetadataAndNilMetadataAreEquivalent documents the one place a
// "different" representation must NOT trigger a rewrite: both spellings of "no
// metadata" carry the same meaning, and treating them as different would make
// every reconcile rewrite the object.
func TestEqual_EmptyMetadataAndNilMetadataAreEquivalent(t *testing.T) {
	a, b := baseSpec(), baseSpec()
	a.Frontmatter.Metadata = nil
	b.Frontmatter.Metadata = map[string]string{}

	assert.True(t, skillspec.Equal(a, b),
		"nil and empty both mean 'no metadata'; distinguishing them would churn the CR on every pass")
}

func TestToV1Frontmatter(t *testing.T) {
	t.Run("every frontmatter field is carried across", func(t *testing.T) {
		got := skillspec.ToV1Frontmatter(skillmd.Frontmatter{
			Name:          "demo-skill",
			License:       "Apache-2.0",
			Compatibility: ">=1.0",
			Metadata:      map[string]string{"team": "platform"},
			AllowedTools:  "read_file",
		})
		assert.Equal(t, v1.SkillFrontmatter{
			Name:          "demo-skill",
			License:       "Apache-2.0",
			Compatibility: ">=1.0",
			Metadata:      map[string]string{"team": "platform"},
			AllowedTools:  "read_file",
		}, got)
	})

	// Description deliberately lives on SkillSpec, not on the CRD frontmatter.
	// If it ever appears here it has two homes and they will drift.
	t.Run("the zero frontmatter maps to the zero CRD frontmatter", func(t *testing.T) {
		assert.Equal(t, v1.SkillFrontmatter{}, skillspec.ToV1Frontmatter(skillmd.Frontmatter{}))
	})
}

// demoOwnerRef is the canonical controller owner ref these tests adopt.
func demoOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         v1.SchemeGroupVersion.String(),
		Kind:               "SkillSource",
		Name:               "demo-source",
		UID:                types.UID("uid-1"),
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func TestOwnerRefPresent(t *testing.T) {
	want := demoOwnerRef()

	cases := []struct {
		name string
		refs []metav1.OwnerReference
		want bool
	}{
		{name: "empty list: absent", refs: nil, want: false},
		{name: "exact match: present", refs: []metav1.OwnerReference{want}, want: true},
		{
			name: "match among several refs: present",
			refs: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "uid-9"},
				want,
			},
			want: true,
		},
		{
			name: "same identity but a different UID: absent (a recreated owner must be re-adopted)",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.UID = "uid-2"
				return r
			}()},
			want: false,
		},
		{
			name: "same identity but a different Kind: absent",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.Kind = "ClusterSkillSource"
				return r
			}()},
			want: false,
		},
		{
			name: "same identity but a different APIVersion: absent",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.APIVersion = "other/v1"
				return r
			}()},
			want: false,
		},
		{
			name: "same identity but a different Name: absent",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.Name = "other-source"
				return r
			}()},
			want: false,
		},
		{
			// A ref matching on identity but NOT flagged as the controller
			// reports absent, which is what drives AdoptOwnerRef to upgrade it.
			// Reporting it present would leave the weaker ref in place forever.
			name: "identity matches but Controller is false: absent, so it gets upgraded",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.Controller = ptr.To(false)
				return r
			}()},
			want: false,
		},
		{
			name: "identity matches but Controller is unset: absent, so it gets upgraded",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.Controller = nil
				return r
			}()},
			want: false,
		},
		{
			name: "identity matches but BlockOwnerDeletion is unset: absent, so it gets upgraded",
			refs: []metav1.OwnerReference{func() metav1.OwnerReference {
				r := demoOwnerRef()
				r.BlockOwnerDeletion = nil
				return r
			}()},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, skillspec.OwnerRefPresent(tc.refs, want))
		})
	}
}

func TestAdoptOwnerRef(t *testing.T) {
	want := demoOwnerRef()

	t.Run("empty list: the ref is appended", func(t *testing.T) {
		got := skillspec.AdoptOwnerRef(nil, want)
		assert.Equal(t, []metav1.OwnerReference{want}, got)
	})

	t.Run("unrelated refs are preserved and the ref is appended", func(t *testing.T) {
		other := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "uid-9"}
		got := skillspec.AdoptOwnerRef([]metav1.OwnerReference{other}, want)
		assert.Equal(t, []metav1.OwnerReference{other, want}, got)
	})

	// The point of the helper: repeated adoption must not accumulate duplicates,
	// which would grow metadata without bound across reconciles.
	t.Run("a stale ref with the same Kind+Name is replaced, not duplicated", func(t *testing.T) {
		stale := demoOwnerRef()
		stale.UID = "uid-old"
		stale.Controller = ptr.To(false)

		got := skillspec.AdoptOwnerRef([]metav1.OwnerReference{stale}, want)
		require.Len(t, got, 1, "the stale ref must be dropped, not kept alongside the new one")
		assert.Equal(t, want, got[0])
	})

	t.Run("adoption is idempotent: re-adopting yields the same single ref", func(t *testing.T) {
		once := skillspec.AdoptOwnerRef(nil, want)
		twice := skillspec.AdoptOwnerRef(once, want)
		assert.Equal(t, once, twice,
			"a re-reconcile must be a no-op, or the object is rewritten on every pass")
		assert.Len(t, twice, 1)
	})

	t.Run("a same-Kind ref under a different Name is preserved", func(t *testing.T) {
		sibling := demoOwnerRef()
		sibling.Name = "other-source"
		sibling.UID = "uid-8"

		got := skillspec.AdoptOwnerRef([]metav1.OwnerReference{sibling}, want)
		assert.Equal(t, []metav1.OwnerReference{sibling, want}, got,
			"only a ref matching BOTH Kind and Name is the stale one being replaced")
	})

	t.Run("the input slice is not mutated", func(t *testing.T) {
		other := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "uid-9"}
		in := []metav1.OwnerReference{other}
		_ = skillspec.AdoptOwnerRef(in, want)
		assert.Equal(t, []metav1.OwnerReference{other}, in,
			"the caller's slice must be left alone; AdoptOwnerRef returns a new one")
	})

	// AdoptOwnerRef's output must satisfy OwnerRefPresent, or the controller
	// adopts on every reconcile and never converges.
	t.Run("the result satisfies OwnerRefPresent, so the next pass is a no-op", func(t *testing.T) {
		got := skillspec.AdoptOwnerRef(nil, want)
		assert.True(t, skillspec.OwnerRefPresent(got, want))
	})
}
