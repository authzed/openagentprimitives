package lifecycle

import "testing"

func TestEffectsSealed(t *testing.T) {
	_ = []Effect{
		AppendLog{}, ProjectStatus{}, StopLoop{}, ArmTimer{}, CancelTimer{},
		Notify{}, Unpark{}, ReissuePending{}, MarkPlanStopped{},
	}
}
