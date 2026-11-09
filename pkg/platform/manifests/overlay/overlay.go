// Package overlay layers user-supplied kustomize Components over the manifests
// oap installs, so a change an operator needs on their cluster (a raised memory
// limit, an extra env var) lives in files they own and survives every re-install
// and upgrade instead of being reset to the bundled value.
//
// An overlay is a directory holding a kustomization.yaml of `kind: Component`.
// A Component, unlike a classic overlay, does not name its base: oap supplies
// it. That is what lets one overlay directory serve both install paths — `oap
// install` layers it over the documents it is about to apply, and `oap render`
// lists it under the rendered kustomization's `components:`.
//
// Load builds every overlay against the full set of first-party manifests oap
// ships and refuses, before anything touches a cluster:
//
//   - a patch that matches no resource (kustomize's own check — a typo'd
//     Deployment name fails instead of being silently ignored);
//   - an overlay that adds, removes or renames a resource — overlays modify what
//     oap installs, they do not change which resources it installs;
//   - two overlays that set the same field to different values. Each overlay is
//     built on its own and the fields it changes are compared; agreeing values
//     are not a conflict.
//
// The result is a per-resource patch, derived once, that Apply lays over a
// document at the moment install writes it. Deriving a patch rather than
// substituting the built document is what keeps install's own per-document
// rewrites (image refs, pull secrets, mirrors) intact: a strategic-merge patch
// carries only the fields the overlays changed.
package overlay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// Set is a validated set of overlays, reduced to one patch per resource they
// change. A nil *Set is valid and applies nothing, so callers without overlays
// need no branch.
type Set struct {
	dirs    []string
	patches map[resID]resourcePatch
}

// resID identifies a resource across the base and the built output. Version is
// deliberately absent: install may apply the same resource at a different
// apiVersion than the bundle names, and it is still the same object.
type resID struct {
	Group, Kind, Namespace, Name string
}

func (id resID) String() string {
	k := id.Kind
	if id.Group != "" {
		k += "." + id.Group
	}
	if id.Namespace != "" {
		return k + " " + id.Namespace + "/" + id.Name
	}
	return k + " " + id.Name
}

func idOf(u *unstructured.Unstructured) resID {
	gvk := u.GroupVersionKind()
	return resID{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()}
}

// resourcePatch is the change the overlays make to one resource: a
// strategic-merge patch for kinds the client-go scheme knows (so lists such as
// containers merge by key), an RFC 7386 JSON merge patch for everything else.
type resourcePatch struct {
	strategic bool
	gvk       schema.GroupVersionKind
	data      []byte
}

// Base returns the documents an overlay is validated against: every first-party
// manifest `oap install` can apply. It is the same set whether or not a given
// optional component is enabled on this install, so whether an overlay is valid
// never depends on install flags.
func Base() ([][]byte, error) {
	out := [][]byte{manifests.Install}
	for _, load := range []func() ([][]byte, error){
		manifests.ChannelsD, manifests.NATS, manifests.Postgres, manifests.Neo4j, manifests.Graphiti,
	} {
		docs, err := load()
		if err != nil {
			return nil, err
		}
		out = append(out, docs...)
	}
	return out, nil
}

// Load reads each overlay directory, validates the set against base, and
// derives the per-resource patches. Directories apply in the order given. An
// empty dirs returns a nil Set.
func Load(base [][]byte, dirs []string) (*Set, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	fsys := filesys.MakeFsInMemory()
	baseDocs, err := splitAll(base)
	if err != nil {
		return nil, fmt.Errorf("overlay base: %w", err)
	}
	if err := writeBase(fsys, baseDocs); err != nil {
		return nil, err
	}
	for i, dir := range dirs {
		if err := copyOverlay(fsys, dir, overlayRoot(i)); err != nil {
			return nil, err
		}
	}

	baseByID := index(baseDocs)

	// Each overlay alone: catch a bad patch with the right directory named, and
	// collect the fields it changes for the conflict check.
	changes := make([]map[resID]map[string]any, len(dirs))
	for i, dir := range dirs {
		built, err := build(fsys, []int{i})
		if err != nil {
			return nil, fmt.Errorf("overlay %s: %w", dir, err)
		}
		if err := sameResources(baseByID, built); err != nil {
			return nil, fmt.Errorf("overlay %s: %w", dir, err)
		}
		changes[i] = map[resID]map[string]any{}
		for id, doc := range built {
			if c := changedLeaves(baseByID[id].Object, doc.Object); len(c) > 0 {
				changes[i][id] = c
			}
		}
	}
	if err := conflicts(dirs, changes); err != nil {
		return nil, err
	}

	all := make([]int, len(dirs))
	for i := range all {
		all[i] = i
	}
	built, err := build(fsys, all)
	if err != nil {
		return nil, fmt.Errorf("overlays: %w", err)
	}
	s := &Set{dirs: slices.Clone(dirs), patches: map[resID]resourcePatch{}}
	for id, doc := range built {
		orig := baseByID[id]
		if reflect.DeepEqual(orig.Object, doc.Object) {
			continue
		}
		p, err := derivePatch(orig, doc)
		if err != nil {
			return nil, fmt.Errorf("overlays: derive patch for %s: %w", id, err)
		}
		s.patches[id] = p
	}
	return s, nil
}

// Dirs returns the overlay directories in application order.
func (s *Set) Dirs() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.dirs)
}

// Patched returns the resources the overlays change, sorted, for narration.
func (s *Set) Patched() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.patches))
	for id := range s.patches {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

// Apply lays the overlays' patch for d's resource over d in place. A resource
// no overlay touches, and any call on a nil Set, leaves d unchanged.
func (s *Set) Apply(d *unstructured.Unstructured) error {
	if s == nil {
		return nil
	}
	p, ok := s.patches[idOf(d)]
	if !ok {
		return nil
	}
	orig, err := json.Marshal(d.Object)
	if err != nil {
		return err
	}
	var patched []byte
	if p.strategic {
		obj, err := scheme.Scheme.New(p.gvk)
		if err != nil {
			return err
		}
		patched, err = strategicpatch.StrategicMergePatch(orig, p.data, obj)
		if err != nil {
			return fmt.Errorf("apply overlay to %s: %w", idOf(d), err)
		}
	} else {
		patched, err = jsonpatch.MergePatch(orig, p.data)
		if err != nil {
			return fmt.Errorf("apply overlay to %s: %w", idOf(d), err)
		}
	}
	obj := map[string]any{}
	if err := json.Unmarshal(patched, &obj); err != nil {
		return err
	}
	d.Object = obj
	return nil
}

func derivePatch(orig, mod *unstructured.Unstructured) (resourcePatch, error) {
	a, err := json.Marshal(orig.Object)
	if err != nil {
		return resourcePatch{}, err
	}
	b, err := json.Marshal(mod.Object)
	if err != nil {
		return resourcePatch{}, err
	}
	gvk := orig.GroupVersionKind()
	if obj, err := scheme.Scheme.New(gvk); err == nil {
		data, err := strategicpatch.CreateTwoWayMergePatch(a, b, obj)
		if err != nil {
			return resourcePatch{}, err
		}
		return resourcePatch{strategic: true, gvk: gvk, data: data}, nil
	}
	data, err := jsonpatch.CreateMergePatch(a, b)
	if err != nil {
		return resourcePatch{}, err
	}
	return resourcePatch{gvk: gvk, data: data}, nil
}

const (
	fsBase     = "/oap/base"
	fsOverlays = "/oap/overlays"
	fsBuild    = "/oap/build"
)

func overlayRoot(i int) string { return path.Join(fsOverlays, fmt.Sprint(i)) }

func writeBase(fsys filesys.FileSystem, docs []*unstructured.Unstructured) error {
	var buf bytes.Buffer
	for _, d := range docs {
		b, err := yaml.Marshal(d.Object)
		if err != nil {
			return err
		}
		buf.WriteString("---\n")
		buf.Write(b)
	}
	if err := fsys.MkdirAll(fsBase); err != nil {
		return err
	}
	if err := fsys.WriteFile(path.Join(fsBase, "base.yaml"), buf.Bytes()); err != nil {
		return err
	}
	return fsys.WriteFile(path.Join(fsBase, "kustomization.yaml"), []byte("resources:\n- base.yaml\n"))
}

// copyOverlay copies dir into the in-memory filesystem at dst, refusing anything
// that is not a kustomize Component. Copying, rather than building from disk,
// keeps the base off the user's filesystem and means a build reads exactly the
// files Load validated.
func copyOverlay(fsys filesys.FileSystem, dir, dst string) error {
	if err := CheckComponent(dir); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		target := path.Join(dst, filepath.ToSlash(rel))
		if e.IsDir() {
			return fsys.MkdirAll(target)
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("overlay %s: %s is not a regular file", dir, rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return fsys.WriteFile(target, b)
	})
}

// kustomizationFiles are the names kustomize accepts for a kustomization, in its
// own lookup order.
var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// CheckComponent reports whether dir holds a kustomization of `kind:
// Component`. A classic overlay names its own base by path, which cannot point
// at the documents install holds in memory, so it is refused with a message
// saying what to change.
func CheckComponent(dir string) error {
	for _, name := range kustomizationFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("overlay %s: %w", dir, err)
		}
		var k struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			return fmt.Errorf("overlay %s: parse %s: %w", dir, name, err)
		}
		if k.Kind != "Component" {
			return fmt.Errorf("overlay %s: %s must be a kustomize Component (apiVersion: kustomize.config.k8s.io/v1alpha1, kind: Component), not %q — oap supplies the base, so the overlay must not name one", dir, name, k.Kind)
		}
		return nil
	}
	return fmt.Errorf("overlay %s: no kustomization.yaml found", dir)
}

// build runs kustomize over the base plus the overlays at idx, in order.
func build(fsys filesys.FileSystem, idx []int) (map[resID]*unstructured.Unstructured, error) {
	dir := path.Join(fsBuild, strings.Trim(strings.ReplaceAll(fmt.Sprint(idx), " ", "-"), "[]"))
	var k strings.Builder
	// kustomize refuses absolute roots, so every reference is relative to dir
	// (one level under fsBuild, itself a sibling of fsBase and fsOverlays).
	k.WriteString("resources:\n- ../../base\ncomponents:\n")
	for _, i := range idx {
		k.WriteString("- ../../overlays/" + fmt.Sprint(i) + "\n")
	}
	if err := fsys.MkdirAll(dir); err != nil {
		return nil, err
	}
	if err := fsys.WriteFile(path.Join(dir, "kustomization.yaml"), []byte(k.String())); err != nil {
		return nil, err
	}
	res, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fsys, dir)
	if err != nil {
		return nil, err
	}
	out, err := res.AsYaml()
	if err != nil {
		return nil, err
	}
	docs, err := manifests.Split(out)
	if err != nil {
		return nil, err
	}
	return index(docs), nil
}

func splitAll(base [][]byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, b := range base {
		docs, err := manifests.Split(b)
		if err != nil {
			return nil, err
		}
		out = append(out, docs...)
	}
	return out, nil
}

func index(docs []*unstructured.Unstructured) map[resID]*unstructured.Unstructured {
	m := make(map[resID]*unstructured.Unstructured, len(docs))
	for _, d := range docs {
		m[idOf(d)] = d
	}
	return m
}

// sameResources refuses an overlay that changes which resources exist. Install
// applies resources from several bundles at different moments; an added
// resource would belong to none of them, and a removed or renamed one would be
// applied anyway under its bundled identity.
func sameResources(base, built map[resID]*unstructured.Unstructured) error {
	var added, removed []string
	for id := range built {
		if _, ok := base[id]; !ok {
			added = append(added, id.String())
		}
	}
	for id := range base {
		if _, ok := built[id]; !ok {
			removed = append(removed, id.String())
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	sort.Strings(added)
	sort.Strings(removed)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "adds "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removes "+strings.Join(removed, ", "))
	}
	return fmt.Errorf("%s; overlays may only modify the resources oap installs (a rename counts as both)", strings.Join(parts, " and "))
}

// removed marks a field an overlay deleted, so a deletion conflicts with a set.
type removedMarker struct{}

// changedLeaves returns every leaf path whose value differs between a and b,
// mapped to b's value. A list of objects that all carry a string "name" is
// keyed by name, matching how a strategic-merge patch addresses containers,
// env vars and ports, so two overlays touching different containers never
// collide on a shared index.
func changedLeaves(a, b any) map[string]any {
	out := map[string]any{}
	walkDiff("", a, b, out)
	return out
}

func walkDiff(p string, a, b any, out map[string]any) {
	if reflect.DeepEqual(a, b) {
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		for k := range union(am, bm) {
			av, ain := am[k]
			bv, bin := bm[k]
			switch {
			case !bin:
				out[p+"/"+k] = removedMarker{}
			case !ain:
				walkDiff(p+"/"+k, nil, bv, out)
			default:
				walkDiff(p+"/"+k, av, bv, out)
			}
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok {
		if ak, ok := byName(al); ok {
			if bk, ok := byName(bl); ok {
				for k := range union(ak, bk) {
					bv, bin := bk[k]
					if !bin {
						out[p+"/["+k+"]"] = removedMarker{}
						continue
					}
					walkDiff(p+"/["+k+"]", ak[k], bv, out)
				}
				return
			}
		}
	}
	if bm, ok := b.(map[string]any); ok && a == nil {
		for k, v := range bm {
			walkDiff(p+"/"+k, nil, v, out)
		}
		return
	}
	out[p] = b
}

func byName(l []any) (map[string]any, bool) {
	m := make(map[string]any, len(l))
	for _, item := range l {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := obj["name"].(string)
		if !ok {
			return nil, false
		}
		m[name] = obj
	}
	return m, true
}

func union[V any](a, b map[string]V) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// conflicts reports every field two overlays set to different values. All
// conflicts are reported at once so a user fixing them is not sent round the
// loop once per field.
func conflicts(dirs []string, changes []map[resID]map[string]any) error {
	var found []string
	for i := range changes {
		for j := i + 1; j < len(changes); j++ {
			for id, ci := range changes[i] {
				cj, ok := changes[j][id]
				if !ok {
					continue
				}
				for field, vi := range ci {
					vj, ok := cj[field]
					if !ok || reflect.DeepEqual(vi, vj) {
						continue
					}
					found = append(found, fmt.Sprintf("  %s %s: %s sets %s, %s sets %s", id, field, dirs[i], describe(vi), dirs[j], describe(vj)))
				}
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("overlays conflict:\n%s", strings.Join(found, "\n"))
}

func describe(v any) string {
	if _, ok := v.(removedMarker); ok {
		return "(removed)"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
