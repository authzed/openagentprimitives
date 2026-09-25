package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestPodRunnerFactoryObservedNameIsThePodName(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
	}
	f := &PodRunnerFactory{}
	assert.Equal(t, "demo-session-runner", f.ObservedName(sess),
		"the pod factory must report the pod it creates so status reflects it")
}
