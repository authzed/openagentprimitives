package completion

import (
	"context"
	"fmt"
	"strings"
)

// KeyCheckFailed is the reserved Unmet key standing for "the declared
// requirements could not be checked at all".
//
// It exists so a configuration fault travels the SAME path as a genuine gap:
// one refusal shape, one bypass, one record. A caller that had to branch on
// Evaluate's error would end up offering an escape from the gap and none from
// the fault — which is the wedge the bypass exists to prevent. Register refuses
// it as a requirement key so a real kind can never collide with the synthetic
// one.
const KeyCheckFailed = "requirement-check-failed"

// CheckFailedUnmet renders an Evaluate error as the synthetic Unmet entry
// callers fold in alongside the real ones.
func CheckFailedUnmet(err error) Unmet {
	return Unmet{
		Key:   KeyCheckFailed,
		Title: "the agent's completion checks could not run",
		Missing: err.Error() +
			". This is a configuration fault in how this agent was set up, not something you did.",
	}
}

// Evaluate resolves each declared key through Get and returns the requirements
// this session has NOT satisfied, in declaration order.
//
// Only the keys the AgentClass declared are consulted. A registered kind no
// class opted into never runs — registration makes a requirement AVAILABLE, it
// does not turn it on, which is what keeps a new kind from silently tightening
// every existing class.
//
// Fail-closed on both failure modes. An unknown key means the operator declared
// a guarantee this build cannot check, and an erroring Check means a
// requirement could not answer; either way the error is returned rather than
// treated as satisfaction, and the caller refuses completion. That refusal is
// bypassable like any other, so a misconfiguration degrades a round instead of
// wedging the session.
func Evaluate(ctx context.Context, keys []string, in Input) ([]Unmet, error) {
	var unmet []Unmet
	var problems []string
	for _, key := range keys {
		req, ok := Get(key)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%q is not a requirement this build knows about (registered: %s)",
				key, strings.Join(Keys(), ", ")))
			continue
		}
		f, err := req.Check(ctx, in)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%q could not be checked: %v", key, err))
			continue
		}
		if f.Met {
			continue
		}
		unmet = append(unmet, Unmet{Key: key, Title: req.Title(), Missing: f.Missing})
	}
	if len(problems) > 0 {
		return unmet, fmt.Errorf("completion requirements could not be evaluated: %s",
			strings.Join(problems, "; "))
	}
	return unmet, nil
}

// UnmetList renders unmet as the model-facing block naming each requirement and
// what is missing from it.
//
// It is the LIST only, with no lead-in and no call to action, because the two
// places that refuse a completion are refusing different things and must say so
// differently: agent_work_complete is answering a call the model just made,
// while the runner's settle gate is answering a round that tried to end without
// calling it at all. What must not differ is how a requirement is named — a
// model that saw "[artifact-delivered] …" once has to recognise it the next
// time, whichever gate produced it.
func UnmetList(unmet []Unmet) string {
	var b strings.Builder
	for _, u := range unmet {
		b.WriteString(fmt.Sprintf("\n- [%s] %s\n  %s\n", u.Key, u.Title, u.Missing))
	}
	return b.String()
}
