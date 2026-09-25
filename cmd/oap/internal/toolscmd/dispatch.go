package toolscmd

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"
)

// dispatchKind picks a Kind for the given YAML/JSON document by:
//
//  1. Attempting a CR decode via registry.DecodeAny — succeeds when the doc
//     has apiVersion+kind set to a registered type.
//  2. Falling back to FlatMatcher: each kind that implements the optional
//     interface gets a chance to claim the doc by structural sniff.
//
// Used by validate/lint/probe — verbs that operate on either CR or
// "flat" authoring-format files.
func dispatchKind(data []byte) (contract.Kind, error) {
	if _, k, err := registry.DecodeAny(data); err == nil {
		return k, nil
	}
	var matched []contract.Kind
	for _, k := range registry.All() {
		fm, ok := k.(contract.FlatMatcher)
		if !ok {
			continue
		}
		if fm.MatchFlat(data) {
			matched = append(matched, k)
		}
	}
	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("could not determine tool kind from file (set apiVersion+kind, or check the spec format)")
	case 1:
		return matched[0], nil
	default:
		names := make([]string, 0, len(matched))
		for _, k := range matched {
			names = append(names, k.Name())
		}
		return nil, fmt.Errorf("file structure is ambiguous between kinds: %s", strings.Join(names, ", "))
	}
}
