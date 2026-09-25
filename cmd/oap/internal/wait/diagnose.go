package wait

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Diagnosis is a best-effort, human-readable summary of why a workload has not
// become ready: the blocking pod's phase/reason, its recent Warning events, and
// the bound-state of the PVCs it references. Any field may be empty when that
// data could not be gathered — callers render whatever is present.
type Diagnosis struct {
	Workload string
	Pod      string
	PodPhase string
	Reason   string
	Events   []EventNote
	PVCs     []PVCNote

	// Headline is a one-line, human-readable summary printed before the
	// structured detail below. It is used by non-pod diagnoses (e.g. a Gateway
	// whose cloud load balancer never programmed) to lead with the actionable
	// sentence — "the LB program is wedged" — rather than a wall of conditions.
	Headline string
	// Conditions surfaces the blocking status conditions of a non-pod resource
	// (a Gateway, today). Each renders as one line so an operator sees the
	// controller's own reason/message without re-running kubectl describe.
	Conditions []ConditionNote

	// Terminated is the fatal exit of the blocking pod's container, when it has
	// already died once. For a CrashLoopBackOff pod this is the ONLY field that
	// says why: the pod events (BackOff, Unhealthy, CONTAINER_EXITED) are
	// downstream consequences of a process that already exited, and chasing them
	// leads to probe tuning instead of the real fault.
	Terminated *TerminatedNote
}

// TerminatedNote is a container's last fatal exit: its status, the termination
// message Kubernetes captured, and a tail of its previous logs.
//
// This exists because a SpiceDB rollout once wedged `oap install` for hours while
// the crashlooping pod's own first log line stated the root cause verbatim and
// named the fix — `oap` gathered pod events, showed probe failures, and never
// looked at the log or at lastState.terminated.message (which the Deployments
// already populate via terminationMessagePolicy: FallbackToLogsOnError).
type TerminatedNote struct {
	Container string
	ExitCode  int32
	Reason    string
	// Message is status.containerStatuses[].lastState.terminated.message.
	Message string
	// Logs is a short tail of the exited container's output, preferring lines
	// that look like errors. Best-effort: empty when logs could not be read.
	Logs []string
}

// ConditionNote is one status condition worth surfacing in a Diagnosis.
type ConditionNote struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// EventNote is one Warning event on the blocking pod.
type EventNote struct {
	Reason  string
	Message string
	Count   int32
}

// PVCNote is the bound-state of a PVC the blocking pod references.
type PVCNote struct {
	Name  string
	Phase string
}

const (
	maxDiagEvents = 3
	// maxDiagLogScan is how many trailing log lines to fetch; maxDiagLogLines is
	// how many of those (error lines preferred) actually get printed. Scanning
	// wider than we print is what lets an error a few lines above the tail still
	// surface.
	maxDiagLogScan     = 40
	maxDiagLogLines    = 5
	maxDiagDetailChars = 300
)

// DiagnoseDeployment gathers a Diagnosis for a Deployment that has not become
// ready. Best-effort: it returns a partial Diagnosis with a nil error whenever
// it can reach the API; a non-nil error is reserved for a hard failure to read
// the Deployment itself.
func DiagnoseDeployment(ctx context.Context, typed kubernetes.Interface, namespace, name string) (Diagnosis, error) {
	d, err := typed.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Diagnosis{Workload: "deployment/" + name}, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	return diagnoseWorkload(ctx, typed, namespace, "deployment/"+name,
		labels.Set(d.Spec.Selector.MatchLabels).AsSelector().String())
}

// DiagnoseStatefulSet is the StatefulSet analogue of DiagnoseDeployment.
func DiagnoseStatefulSet(ctx context.Context, typed kubernetes.Interface, namespace, name string) (Diagnosis, error) {
	s, err := typed.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Diagnosis{Workload: "statefulset/" + name}, fmt.Errorf("get statefulset %s/%s: %w", namespace, name, err)
	}
	return diagnoseWorkload(ctx, typed, namespace, "statefulset/"+name,
		labels.Set(s.Spec.Selector.MatchLabels).AsSelector().String())
}

func diagnoseWorkload(ctx context.Context, typed kubernetes.Interface, namespace, workload, selector string) (Diagnosis, error) {
	diag := Diagnosis{Workload: workload}
	pods, err := typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return diag, fmt.Errorf("list pods for %s: %w", workload, err)
	}
	pod := firstNotReadyPod(pods.Items)
	if pod == nil {
		return diag, nil // race: nothing not-ready to report; caller still prints the timeout
	}
	diag.Pod = pod.Name
	diag.PodPhase = string(pod.Status.Phase)
	diag.Reason = waitingReason(pod)
	diag.Terminated = terminatedNote(ctx, typed, namespace, pod)
	diag.Headline = headline(diag.Reason, diag.Terminated)
	diag.PVCs = pvcNotes(ctx, typed, namespace, pod)
	diag.Events = warningEvents(ctx, typed, namespace, pod.Name)
	return diag, nil
}

func firstNotReadyPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		if !podReady(&pods[i]) {
			return &pods[i]
		}
	}
	return nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func waitingReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

// headline picks the lead line for a pod diagnosis.
//
// A container that exited fatally takes precedence over the reassuring
// backing-off line: "will retry" is true of a dependency that is not up yet, but
// actively misleading when the process died of a permanent misconfiguration that
// no amount of retrying fixes. Leading with the container's own error is what
// turns an unexplained multi-hour install hang into a one-line fix.
func headline(reason string, term *TerminatedNote) string {
	if fatal := term.fatalLine(); fatal != "" {
		return fatal
	}
	if reason == "CrashLoopBackOff" {
		return "waiting on a dependency — pod is backing off and will retry (common during a cold-start install)"
	}
	return ""
}

// fatalLine renders the terminated container's own explanation as one line, or
// "" when there is no terminated container or nothing useful to say about it. A
// clean exit(0) is not a fault worth leading with.
func (t *TerminatedNote) fatalLine() string {
	if t == nil || t.ExitCode == 0 {
		return ""
	}
	detail := t.Message
	if detail == "" && len(t.Logs) > 0 {
		// The earliest error in the captured window, not the latest: a failing
		// startup usually logs its root cause first and then cascades.
		detail = t.Logs[0]
	}
	if detail == "" {
		return ""
	}
	return fmt.Sprintf("container %s exited %d (%s) — this is a fatal startup error, not a retryable wait: %s",
		t.Container, t.ExitCode, t.Reason, truncate(detail, maxDiagDetailChars))
}

// truncate cuts to at most n runes (not bytes, so a multi-byte character is
// never split into mojibake) and marks the cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// terminatedNote reports the blocking pod's last fatal container exit.
//
// Init containers are checked first: one that keeps failing blocks every app
// container from starting at all, so it is the cause and they are the symptom.
// Within a container it prefers a dead-and-waiting state (lastState.terminated,
// the CrashLoopBackOff shape) over one terminated right now, and pulls that
// container's logs when Kubernetes captured no termination message.
// Best-effort throughout: a nil return simply means "nothing to add".
func terminatedNote(ctx context.Context, typed kubernetes.Interface, ns string, p *corev1.Pod) *TerminatedNote {
	for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, cs := range statuses {
			term := cs.LastTerminationState.Terminated
			previous := true
			if term == nil {
				term, previous = cs.State.Terminated, false
			}
			if term == nil || term.ExitCode == 0 {
				continue
			}
			note := &TerminatedNote{
				Container: cs.Name,
				ExitCode:  term.ExitCode,
				Reason:    term.Reason,
				Message:   strings.TrimSpace(term.Message),
			}
			if note.Message == "" {
				note.Logs = containerLogTail(ctx, typed, ns, p.Name, cs.Name, previous)
			}
			return note
		}
	}
	return nil
}

// containerLogTail reads the tail of a container's log and returns the lines
// most likely to explain the exit: error-looking lines first, else the last few
// lines verbatim. Errors are swallowed deliberately — this is supplementary
// detail on a path that is already reporting a failure, and a log read that
// fails must not mask the diagnosis the caller is about to print.
func containerLogTail(ctx context.Context, typed kubernetes.Interface, ns, pod, container string, previous bool) []string {
	tail := int64(maxDiagLogScan)
	body, err := typed.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tail,
	}).DoRaw(ctx)
	if err != nil || len(body) == 0 {
		return nil
	}
	var all, errs []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		all = append(all, line)
		if looksLikeError(line) {
			errs = append(errs, line)
		}
	}
	if len(errs) > 0 {
		return lastN(errs, maxDiagLogLines)
	}
	return lastN(all, maxDiagLogLines)
}

// looksLikeError matches the level markers of the log formats this project's
// components and its dependencies emit (zerolog/zap JSON, logfmt, plain text)
// rather than any single library's shape.
func looksLikeError(line string) bool {
	l := strings.ToLower(line)
	for _, marker := range []string{`"level":"error"`, `"level":"fatal"`, "level=error", "level=fatal", "error:", "fatal:"} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func pvcNotes(ctx context.Context, typed kubernetes.Interface, ns string, p *corev1.Pod) []PVCNote {
	var notes []PVCNote
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		phase := "Unknown"
		if pvc, err := typed.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			phase = string(pvc.Status.Phase)
		}
		notes = append(notes, PVCNote{Name: name, Phase: phase})
	}
	return notes
}

// warningEvents returns the pod's most-recent Warning events, newest first,
// capped at maxDiagEvents. It filters by InvolvedObject.Name in code as well as
// via the API field selector, so it stays correct when a client (e.g. the test
// fake) ignores field selectors.
func warningEvents(ctx context.Context, typed kubernetes.Interface, ns, podName string) []EventNote {
	evs, err := typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + podName,
	})
	if err != nil {
		return nil
	}
	items := evs.Items
	sort.Slice(items, func(i, j int) bool {
		return items[i].LastTimestamp.After(items[j].LastTimestamp.Time)
	})
	var notes []EventNote
	for i := range items {
		e := items[i]
		if e.Type != corev1.EventTypeWarning || e.InvolvedObject.Name != podName {
			continue
		}
		notes = append(notes, EventNote{Reason: e.Reason, Message: e.Message, Count: e.Count})
		if len(notes) >= maxDiagEvents {
			break
		}
	}
	return notes
}
