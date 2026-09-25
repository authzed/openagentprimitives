package kube_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

var cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

func bareConfigMap(name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetName(name)
	o.SetNamespace("default")
	return o
}

func TestApplyStampsInstalledBy(t *testing.T) {
	dc := dynfake.NewSimpleDynamicClient(scheme.Scheme)
	require.NoError(t, kube.Apply(context.Background(), dc, bareConfigMap("c"), "test"))
	got, err := dc.Resource(cmGVR).Namespace("default").Get(context.Background(), "c", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, kube.InstalledByValue, got.GetAnnotations()[kube.InstalledByAnnotation],
		"Apply must stamp the installed-by annotation so clean can identify oap-installed resources")
}

func TestDeleteRemovesObjectAndToleratesMissing(t *testing.T) {
	dc := dynfake.NewSimpleDynamicClient(scheme.Scheme, bareConfigMap("c"))
	require.NoError(t, kube.Delete(context.Background(), dc, bareConfigMap("c")))
	_, err := dc.Resource(cmGVR).Namespace("default").Get(context.Background(), "c", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "object should be gone after Delete")
	require.NoError(t, kube.Delete(context.Background(), dc, bareConfigMap("c")), "deleting a missing object is not an error")
}
