package lifecycle

// Fold rebuilds current state by replaying events from zero. Effects are
// discarded — folding is for state reconstruction, not side effects.
func Fold(events []Event) State {
	var s State
	s = s.OrDefault()
	for _, e := range events {
		s, _ = Transition(s, e)
	}
	return s
}

// FoldWithReissue folds, then returns a single ReissuePending effect for any
// still-open decisions so a restarting sequencer re-arms their waits/timeouts.
// This prevents pending decisions from being lost on restart: the sequencer is
// guaranteed to see every outstanding decision it must wait on, even if the prior
// runner died before it could cancel or deliver them.
func FoldWithReissue(events []Event) (State, []Effect) {
	s := Fold(events)
	if len(s.Pending) == 0 {
		return s, nil
	}
	pend := append([]PendingDecision{}, s.Pending...)
	return s, []Effect{ReissuePending{Pending: pend}}
}
