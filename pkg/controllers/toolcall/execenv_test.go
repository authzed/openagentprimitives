package toolcall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	toolspecregistry "github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// TestExecEnv pins the precedence of the environment one tool exec receives.
//
// The interesting layer is the toolkit's envDefaults. The runner stamps those
// onto spec.env so the ToolCall records what the toolkit contributed, but
// spec.env normally outranks every operator-set layer beneath it. A toolkit
// default is not an operator decision, so it is demoted here: it fills a key
// nobody else set, and yields to SpiceboxSession.spec.defaultEnv and to
// SpiceboxClass.spec.envDefaults (delivered as the sandbox container's own
// env, which nothing here exports and everything here would otherwise shadow).
func TestExecEnv(t *testing.T) {
	const key = "A_MAX_RETRIES"
	tk := &toolkit.Toolkit{EnvDefaults: map[string]string{key: "1"}}

	cases := []struct {
		name string
		// specEnv is ToolCall.spec.env as the runner (or a hand-written CR) left it.
		specEnv map[string]string
		// sessionEnv is SpiceboxSession.spec.defaultEnv.
		sessionEnv map[string]string
		// classEnv is the resolved SpiceboxClass.spec.envDefaults snapshot.
		classEnv map[string]string
		agentEnv map[string]string
		toolkit  *toolkit.Toolkit
		want     map[string]string
	}{
		{
			name:    "nothing set anywhere: no env at all",
			toolkit: tk,
			want:    nil,
		},
		{
			name:       "no toolkit: the existing three layers are unchanged",
			specEnv:    map[string]string{"A": "spec", "B": "spec"},
			sessionEnv: map[string]string{"B": "session", "C": "session"},
			agentEnv:   map[string]string{"A": "agent"},
			toolkit:    nil,
			want:       map[string]string{"A": "agent", "B": "spec", "C": "session"},
		},
		{
			name:    "toolkit default alone: it reaches the exec env",
			specEnv: map[string]string{key: "1"},
			toolkit: tk,
			want:    map[string]string{key: "1"},
		},
		{
			name:       "session defaultEnv names the same key: the operator's value wins",
			specEnv:    map[string]string{key: "1"},
			sessionEnv: map[string]string{key: "9"},
			toolkit:    tk,
			want:       map[string]string{key: "9"},
		},
		{
			name:     "class envDefaults names the same key: key drops out so the container's value shows through",
			specEnv:  map[string]string{key: "1"},
			classEnv: map[string]string{key: "9"},
			toolkit:  tk,
			want:     nil,
		},
		{
			name:       "session and class both name it: the session's value wins, the key still reaches exec",
			specEnv:    map[string]string{key: "1"},
			sessionEnv: map[string]string{key: "9"},
			classEnv:   map[string]string{key: "5"},
			toolkit:    tk,
			want:       map[string]string{key: "9"},
		},
		{
			name:       "spec.env carries a value that is NOT the toolkit default: explicit per-call env wins",
			specEnv:    map[string]string{key: "7"},
			sessionEnv: map[string]string{key: "9"},
			classEnv:   map[string]string{key: "5"},
			toolkit:    tk,
			want:       map[string]string{key: "7"},
		},
		{
			name:     "credential-injected key: never demoted, whatever the class says",
			specEnv:  map[string]string{key: "1"},
			classEnv: map[string]string{key: "5"},
			agentEnv: map[string]string{key: "from-broker"},
			toolkit:  tk,
			want:     map[string]string{key: "from-broker"},
		},
		{
			name:     "class names an unrelated key: the toolkit default is untouched",
			specEnv:  map[string]string{key: "1"},
			classEnv: map[string]string{"SOMETHING_ELSE": "x"},
			toolkit:  tk,
			want:     map[string]string{key: "1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := &spiceboxv1alpha1.ToolCall{
				Spec: spiceboxv1alpha1.ToolCallSpec{Env: tc.specEnv},
			}
			sess := &spiceboxv1alpha1.SpiceboxSession{
				Spec: spiceboxv1alpha1.SpiceboxSessionSpec{DefaultEnv: tc.sessionEnv},
			}
			if tc.classEnv != nil {
				sess.Status.ResolvedClass = &spiceboxv1alpha1.SpiceboxClassSpec{EnvDefaults: tc.classEnv}
			}
			assert.Equal(t, tc.want, execEnv(tc.agentEnv, call, sess, tc.toolkit), "exec env")
		})
	}
}

// TestExecEnv_NilSession guards the call sites: both build the exec env from a
// resolvedCall, and a nil session there must not panic the operator.
func TestExecEnv_NilSession(t *testing.T) {
	call := &spiceboxv1alpha1.ToolCall{
		Spec: spiceboxv1alpha1.ToolCallSpec{Env: map[string]string{"A_MAX_RETRIES": "1"}},
	}
	tk := &toolkit.Toolkit{EnvDefaults: map[string]string{"A_MAX_RETRIES": "1"}}
	assert.Equal(t, map[string]string{"A_MAX_RETRIES": "1"}, execEnv(nil, call, nil, tk),
		"a nil session leaves the ToolCall's own env alone")
}

// TestValidateToolspec_RecordsAcceptingToolkit closes the seam between the two
// halves of the toolkit-envDefaults path: execEnv can only demote a toolkit
// default if something hands it the accepting toolkit. Nothing else reads
// resolvedCall.toolkit, so a refactor that stopped setting it would leave every
// execEnv unit test green while the operator silently let a toolkit default
// outrank the operator's own SpiceboxClass.spec.envDefaults.
func TestValidateToolspec_RecordsAcceptingToolkit(t *testing.T) {
	const toolName = "claude"

	reg, err := toolspecregistry.NewWithBuiltins(nil)
	require.NoError(t, err, "seed the built-in toolkit registry")
	builtin, err := reg.Resolve(context.Background(), toolName, claudeRevision(t))
	require.NoError(t, err, "resolve the built-in claude toolkit")
	require.NotEmpty(t, builtin.EnvDefaults,
		"fixture assumption: the claude toolkit declares envDefaults")

	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "claude-spec"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "claude",
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolName, Revision: builtin.ToolkitRevision},
			AllowSubcommands: []string{""},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "Valid",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	sch := gatherTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(ts).Build()

	r := &Reconciler{Client: c, ToolkitRegistry: reg}
	sess := sessionForTool(toolName, ts.Name)
	call := toolCallFor(toolName)
	call.Spec.Args = []string{"--print", "hello"}

	resolved := &resolvedCall{session: sess}
	acceptedBy, failures, err := r.validateToolspec(context.Background(), call, sess, resolved)
	require.NoError(t, err, "validateToolspec")
	require.Equal(t, ts.Name, acceptedBy, "the spec must accept this call (failures: %v)", failures)

	require.NotNil(t, resolved.toolkit, "the accepting toolkit must be recorded for execEnv")
	assert.Equal(t, builtin.EnvDefaults, resolved.toolkit.EnvDefaults,
		"recorded toolkit must be the one that accepted, envDefaults included")
}

// claudeRevision reads the built-in claude toolkit's revision so the fixture
// above does not hardcode a date that every toolkit edit would break.
func claudeRevision(t *testing.T) string {
	t.Helper()
	for _, tk := range toolkits.All() {
		if tk.Name == "claude" {
			return tk.ToolkitRevision
		}
	}
	t.Fatal("built-in claude toolkit not found")
	return ""
}
