package toolscli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"
)

// Match is one (kind, object) result from Resolve.
type Match struct {
	Kind contract.Kind
	Obj  client.Object
}

// AmbiguousError is returned when a name maps to multiple kinds. The CLI
// surfaces this so the user can disambiguate.
type AmbiguousError struct {
	Name  string
	Kinds []string
}

func (e *AmbiguousError) Error() string {
	sort.Strings(e.Kinds)
	return fmt.Sprintf("name %q is ambiguous: matches kinds %s", e.Name, strings.Join(e.Kinds, ", "))
}

// ErrNotFound is returned when no registered kind has an object with the
// requested name in the namespace.
var ErrNotFound = errors.New("name not found in any registered kind")

// Resolve walks every registered kind asking for client.Get on (ns, name).
// Returns a single match. With 0 matches, returns ErrNotFound. With 2+,
// returns an *AmbiguousError listing all kinds that owned the name.
//
// For cluster-scoped kinds, ns is ignored by the API server. For namespaced
// kinds, an empty ns means the caller wants the default namespace already
// resolved upstream — Resolve does not infer ns.
func Resolve(ctx context.Context, c client.Client, ns, name string) (Match, error) {
	all := registry.All()
	if len(all) == 0 {
		return Match{}, fmt.Errorf("no tool kinds registered")
	}

	var matches []Match
	for _, k := range all {
		obj := k.NewObject()
		// Cluster-scoped kinds ignore the namespace; namespaced kinds use
		// the supplied ns.
		lookupNS := ns
		if cs, ok := k.(contract.ClusterScoped); ok && cs.ClusterScoped() {
			lookupNS = ""
		}
		key := client.ObjectKey{Namespace: lookupNS, Name: name}
		if err := c.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			// NoKindMatchError / similar: scheme might not know the kind.
			// Skip and let other kinds try.
			if isNoKindMatch(err) {
				continue
			}
			return Match{}, fmt.Errorf("get %s/%s: %w", k.Name(), name, err)
		}
		matches = append(matches, Match{Kind: k, Obj: obj})
	}

	switch len(matches) {
	case 0:
		return Match{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Kind.Name())
		}
		return Match{}, &AmbiguousError{Name: name, Kinds: names}
	}
}

// isNoKindMatch returns true if err is the controller-runtime "no kind match"
// error a fake client raises for kinds it doesn't know.
func isNoKindMatch(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no kind") || strings.Contains(s, "no matches for kind")
}
