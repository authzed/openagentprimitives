package manifests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/kubeyaml"
)

// Split parses a multi-document YAML stream into individual unstructured
// objects. Empty / comment-only documents are skipped.
//
// The implementation lives in pkg/platform/kubeyaml because three copies of this loop
// had drifted — pkg/platform/oap's had lost the kind-less skip and rejected such a
// document as `disallowed resource kind ""`. kubeyaml is a leaf package on
// purpose: pkg/platform/manifests embeds the ~906KB install bundle, and internal/cmd/operator
// and internal/cmd/webd pull the splitter without pulling that.
func Split(in []byte) ([]*unstructured.Unstructured, error) {
	return kubeyaml.Split(in)
}
