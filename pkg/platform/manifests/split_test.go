package manifests_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

const multiDoc = `apiVersion: v1
kind: Namespace
metadata:
  name: ns1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: ns1
---
# comment-only doc, should be skipped
---
apiVersion: v1
kind: Service
metadata:
  name: svc1
  namespace: ns1
`

func TestSplitMultiDoc(t *testing.T) {
	docs, err := manifests.Split([]byte(multiDoc))
	require.NoError(t, err, "Split must succeed")
	require.Len(t, docs, 3, "comment-only doc should be skipped")

	wantKinds := []string{"Namespace", "ConfigMap", "Service"}
	for i, d := range docs {
		assert.Equal(t, wantKinds[i], d.GetKind(), "docs[%d].Kind", i)
	}
}
