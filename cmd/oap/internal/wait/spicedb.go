package wait

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// spicedbClusterGVR is the authzed spicedb-operator's cluster CR.
var spicedbClusterGVR = schema.GroupVersionResource{
	Group:    "authzed.com",
	Version:  "v1alpha1",
	Resource: "spicedbclusters",
}

// DiagnoseSpiceDBCluster explains a stalled SpiceDB rollout using both the
// operator-created Deployment and the SpiceDBCluster CR the operator writes to.
//
// The CR is not redundant with the Deployment: the spicedb-operator publishes
// the failing pod's fatal error verbatim into
// status.conditions[type=RolloutError].message, and it does so for a CR whose
// Deployment may not exist yet (the operator creates it asynchronously). During
// an install stall that condition is frequently the only place the actual cause
// is written down — `oap` previously never read it, and an outage whose fix was
// one line took hours to find as a result.
func DiagnoseSpiceDBCluster(ctx context.Context, typed kubernetes.Interface, dyn dynamic.Interface, namespace, deployment, cluster string) (Diagnosis, error) {
	conds := spiceDBClusterConditions(ctx, dyn, namespace, cluster)

	diag, err := DiagnoseDeployment(ctx, typed, namespace, deployment)
	if err != nil {
		// The Deployment is unreadable (commonly: the operator has not created it
		// yet). Do NOT propagate the error when the CR told us something — the
		// caller drops the entire diagnosis on error, which is precisely how the
		// operator's own explanation went unseen. Surface the read failure as a
		// visible line instead of discarding it.
		if len(conds) == 0 {
			return diag, err
		}
		diag.Headline = fmt.Sprintf("deployment/%s not readable yet (%v); reporting the SpiceDBCluster's own status instead", deployment, err)
	}
	diag.Conditions = append(diag.Conditions, conds...)
	return diag, nil
}

// spiceDBClusterConditions reads the CR's status conditions. Best-effort: a
// missing CR or a shape we don't recognize yields no notes rather than an error,
// because this only ever augments a diagnosis the caller is already printing.
func spiceDBClusterConditions(ctx context.Context, dyn dynamic.Interface, namespace, name string) []ConditionNote {
	if dyn == nil {
		return nil
	}
	u, err := dyn.Resource(spicedbClusterGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || u == nil {
		return nil
	}
	raw, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	var notes []ConditionNote
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		note := ConditionNote{
			Type:    stringField(m, "type"),
			Status:  stringField(m, "status"),
			Reason:  stringField(m, "reason"),
			Message: truncateMessage(spiceDBConditionMessage(stringField(m, "message"))),
		}
		if note.Type == "" {
			continue
		}
		notes = append(notes, note)
	}
	return notes
}

// spiceDBConditionMessage unwraps the JSON envelope the operator writes into a
// RolloutError message ({"component":…,"error":…}) down to the error itself, so
// the line an operator reads is the fault rather than a serialized struct. Any
// other shape passes through untouched.
func spiceDBConditionMessage(msg string) string {
	var env struct {
		Component string `json:"component"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(msg), &env); err != nil || env.Error == "" {
		return msg
	}
	if env.Component == "" {
		return env.Error
	}
	return env.Component + ": " + env.Error
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
