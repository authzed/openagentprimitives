//go:build integration

package installcmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestSpiceDBDatabaseJob_ReinstallAgainstAPIServer applies the createdb Job the
// way the install pipeline does, against a real API server, over each state a
// re-run can find. A fake client cannot stand in here: only the API server
// enforces that a Job's spec.template is immutable, which is the whole failure.
//
// mage test:integration does not list this package; run it by name:
//
//	go test -tags=integration -run TestSpiceDBDatabaseJob_ReinstallAgainstAPIServer ./cmd/oap/internal/installcmd/
func TestSpiceDBDatabaseJob_ReinstallAgainstAPIServer(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	const ns = "agentprimitives-system"
	typed, err := kubernetes.NewForConfig(env.Cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(env.Cfg)
	require.NoError(t, err)
	_, err = typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	current, doc, err := buildSpiceDBDatabaseJob(installCreateDBDeadlineSeconds, nil)
	require.NoError(t, err)
	apply := func(doc []byte) error {
		return applyManifestGroups(ctx, dyn, [][]byte{doc}, "", nil, nil, nil)
	}
	installed := map[string]string{kube.InstalledByAnnotation: kube.InstalledByValue}
	jobNames := func(t *testing.T) []string {
		t.Helper()
		jobs, err := typed.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		var names []string
		for _, j := range jobs.Items {
			names = append(names, j.Name)
		}
		return names
	}

	cases := []struct {
		name     string
		existing []*batchv1.Job // on the cluster before this install runs
		reapply  bool           // run the apply a second time and require a no-op
	}{
		{name: "fresh install: the Job is created"},
		{name: "re-install with an identical template: the second apply changes nothing", reapply: true},
		{
			name:     "re-install over the fixed-name Job of an older install: applies, then prunes it",
			existing: []*batchv1.Job{installedJob(createDBJobBaseName, installed)},
		},
		{
			name:     "re-install over a hashed Job of an older template: applies, then prunes it",
			existing: []*batchv1.Job{installedJob(createDBJobBaseName+"-0123456789", installed)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			background := metav1.DeletePropagationBackground
			require.NoError(t, typed.BatchV1().Jobs(ns).DeleteCollection(ctx,
				metav1.DeleteOptions{PropagationPolicy: &background}, metav1.ListOptions{}))
			for _, j := range tc.existing {
				_, err := typed.BatchV1().Jobs(ns).Create(ctx, j, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			require.NoError(t, apply(doc), "the apply must not be rejected by a Job an earlier install left behind")
			got, err := typed.BatchV1().Jobs(ns).Get(ctx, current, metav1.GetOptions{})
			require.NoError(t, err)

			if tc.reapply {
				require.NoError(t, apply(doc))
				again, err := typed.BatchV1().Jobs(ns).Get(ctx, current, metav1.GetOptions{})
				require.NoError(t, err)
				assert.Equal(t, got.ResourceVersion, again.ResourceVersion, "a byte-identical re-apply is a no-op")
			}

			require.NoError(t, pruneSupersededCreateDBJobs(ctx, typed, ns, current))
			assert.Equal(t, []string{current}, jobNames(t), "only the current Job is left")
		})
	}

	// The failure the hashed name exists to prevent, pinned so the reason stays
	// true: the same apply under the old fixed name, over a Job with another
	// template, is refused by the API server.
	t.Run("control: the current template under the fixed name of an existing Job is rejected as immutable", func(t *testing.T) {
		_, err := typed.BatchV1().Jobs(ns).Create(ctx, installedJob(createDBJobBaseName, installed), metav1.CreateOptions{})
		require.NoError(t, err)
		var job map[string]any
		require.NoError(t, yaml.Unmarshal(doc, &job))
		job["metadata"].(map[string]any)["name"] = createDBJobBaseName
		fixed, err := yaml.Marshal(job)
		require.NoError(t, err)

		err = apply(fixed)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "field is immutable")
	})
}
