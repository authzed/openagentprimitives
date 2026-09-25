package slack

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestCredentialNamesForClass_DanglingRefKeepsResolvableCredentials is the
// regression guard for a silent-error defect on the Slack Home tab: the card
// builder called the STRICT passthrough.Required and, on any error, returned
// nil with no log. One dangling MCPServer ref — a ref whose CR was deleted or
// typed wrong — therefore blanked the ENTIRE required-services line for that
// agent, and the reader had no way to tell "this agent needs nothing" from
// "we could not work out what it needs".
//
// passthrough.RequiredBestEffort exists for exactly this audience; its own doc
// names the Slack Home tab as an intended caller. It logs per-ref failures and
// keeps every credential that did resolve.
func TestCredentialNamesForClass_DanglingRefKeepsResolvableCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	good := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "tracker", Namespace: "demo"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "tracker-token"},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "tracker", Ref: "tracker"},
				{Name: "gone", Ref: "deleted-server"},
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(good, ac).Build()

	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), capLogger)

	got := credentialNamesForClass(ctx, cli, ac)

	assert.Equal(t, []string{"tracker-token"}, got,
		"one dangling MCPServer ref must not blank the credentials that DID resolve")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "the dangling ref must be logged, not silently swallowed")
	assert.Contains(t, strings.Join(logged, "\n"), "deleted-server",
		"log must name the ref that failed to resolve")
}
