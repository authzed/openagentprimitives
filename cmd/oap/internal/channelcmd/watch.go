// What `oap channel create` does after it has applied a Channel: watch that
// Channel until it settles, and say what happened.
//
// The command used to end with a block of next-steps prose whose first
// instruction was "watch `oap channel show <name>` for Connected=True". That is
// work the CLI is holding all the pieces for — the name, the namespace, a
// client — and handing back to the user, who then has to learn which condition
// means what. Doing it here means the run ends with an answer rather than an
// errand.
package channelcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

const (
	// channelWatchInterval is how often the Channel's status is re-read. The
	// same 1s the session-readiness wait uses (waitForSessionReady): the whole
	// point of the watch is to answer promptly, and a short-lived CLI polling
	// one object for a minute is nothing next to the QPS budget kube.New sets.
	channelWatchInterval = time.Second

	// channelWatchTimeout is how long a run waits by default. Long enough for
	// the operator to validate and channelsd to attach a socket on a cold
	// cluster; short enough that a user whose workspace admin has not flipped a
	// toggle is not left staring at it.
	channelWatchTimeout = 60 * time.Second
)

// channelState is how far along a Channel is, as one look at its conditions
// can tell.
type channelState int

const (
	// channelPending is "nothing has gone wrong and it is not up yet" — the
	// state a Channel is in for the second or two between the apply and
	// channelsd attaching. It is NOT an outcome; only the deadline turns it
	// into one.
	channelPending channelState = iota
	// channelConnected is Connected=True: channelsd has a live listener.
	channelConnected
	// channelBlocked is a False condition that names something a human has to
	// change. Reported immediately — waiting out the deadline first would only
	// delay the same message.
	channelBlocked
)

// channelBlockingConditions are the conditions whose False means the Channel
// cannot serve, in the order they are consulted — which is the order of cause.
//
// Valid comes first because Connected=False/SocketDetached is DOWNSTREAM of it:
// channelsd tears a listener down when the Channel stops being Valid, so a
// Channel with both set would otherwise be reported by its symptom. Connected
// is consulted last for the same reason and still matters on its own — a bad
// token fails the listener's Start with Valid=True (see
// ReasonChannelListenerStartFailed).
//
// ScopesValid is deliberately NOT here; see channelVerdict.scopeGap.
var channelBlockingConditions = []string{
	spiceboxv1alpha1.ChannelConditionValid,
	spiceboxv1alpha1.ChannelConditionInformationLeakageReady,
	spiceboxv1alpha1.ChannelConditionConnected,
}

// channelVerdict is what one look at a Channel's conditions concluded.
type channelVerdict struct {
	state channelState

	// blockedBy is the False condition that decided a blocked verdict. It is
	// the condition itself rather than a re-worded copy so the message the
	// controller wrote reaches the user verbatim — that message is where the
	// missing AgentClass, the rejected spec field or the listener's own error
	// text lives.
	blockedBy *metav1.Condition

	// scopeGap is a ScopesValid=False observed alongside Connected=True: the
	// socket is up and some capability's Slack scope is not granted.
	//
	// It rides along instead of blocking because ScopesValid is owned by
	// channelsd's LISTENER, not the operator — it is written after the listener
	// attaches, and only by a kind that reports its granted scopes at all. A
	// bento or fake Channel never gets the condition, so gating "connected" on
	// it would hang every non-Slack create for the full timeout waiting for a
	// write that is never coming. Its absence is not a finding; its False is.
	scopeGap *metav1.Condition
}

// classifyChannel reads a Channel's conditions and says which of the three
// outcomes it is in. A pure function of the object so every outcome is
// reachable in a test without a cluster or a clock.
func classifyChannel(ch *spiceboxv1alpha1.Channel) channelVerdict {
	for _, condType := range channelBlockingConditions {
		if c := conditions.Find(ch.Status.Conditions, condType); c != nil && c.Status == metav1.ConditionFalse {
			return channelVerdict{state: channelBlocked, blockedBy: c}
		}
	}
	if !conditions.IsTrue(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionConnected) {
		return channelVerdict{state: channelPending}
	}
	v := channelVerdict{state: channelConnected}
	if c := conditions.Find(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionScopesValid); c != nil && c.Status == metav1.ConditionFalse {
		v.scopeGap = c
	}
	return v
}

// watchChannel polls the named Channel until it connects, blocks, or the
// deadline passes.
//
// A NotFound is treated as "not there yet" rather than an error: the apply that
// preceded this went through the dynamic client, and a Channel that genuinely
// never appears is reported by the timeout, in the same words as any other
// failure to come up. Every OTHER read error ends the watch and is returned —
// a run that cannot read the object must not sit in a loop pretending to
// watch it.
func watchChannel(ctx context.Context, c client.Client, namespace, name string, interval, timeout time.Duration) (channelVerdict, error) {
	var v channelVerdict
	err := wait.Until(ctx, interval, timeout, func(ctx context.Context) (bool, error) {
		var ch spiceboxv1alpha1.Channel
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ch); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("read Channel %q in namespace %q: %w", name, namespace, err)
		}
		v = classifyChannel(&ch)
		return v.state != channelPending, nil
	})
	if err != nil {
		// A timeout is not a failure of the watch, it is one of its three
		// outcomes: the caller renders it as "not connected yet". Anything else
		// (a read error, Ctrl-C) is returned for the caller to surface.
		if errors.Is(err, wait.ErrTimeout) {
			return channelVerdict{state: channelPending}, nil
		}
		return channelVerdict{state: channelPending}, err
	}
	return v, nil
}

// channelFollowUp is the one command every non-green outcome points at. `oap
// channel show` rather than `oap channel test`: show is the user-facing view
// (spec, the full condition table, recent sessions) and is what the wizard's
// own notes named, while test is registered as a diagnostic.
func channelFollowUp(name string) string {
	return fmt.Sprintf("Run `oap channel show %s` for the full status.", name)
}

// renderChannelVerdict writes the outcome of a watch.
//
// watchErr is whatever ended the watch other than the deadline. It is rendered
// rather than returned because the Channel and its Secret were created — this
// command's job — and only the report on them is incomplete.
func renderChannelVerdict(out io.Writer, th *tui.Theme, name string, v channelVerdict, watchErr error, timeout time.Duration) {
	followUp := th.Render(th.Subtle, channelFollowUp(name))

	if watchErr != nil {
		fmt.Fprintf(out, "%s\n%s\n",
			th.Render(th.Warn, fmt.Sprintf("Could not read the Channel's status: %v", watchErr)),
			followUp)
		return
	}

	switch v.state {
	case channelConnected:
		fmt.Fprintf(out, "%s\n", th.Render(th.Success, fmt.Sprintf("Connected. Channel %q is live.", name)))
		if c := v.scopeGap; c != nil {
			fmt.Fprintf(out, "%s\n%s\n",
				th.Render(th.Warn, fmt.Sprintf("Some permissions are missing — %s", conditionSentence(c))),
				followUp)
		}
	case channelBlocked:
		fmt.Fprintf(out, "%s\n%s\n",
			th.Render(th.Err, fmt.Sprintf("Not connected. %s", conditionSentence(v.blockedBy))),
			followUp)
	default:
		fmt.Fprintf(out, "%s\n%s\n",
			th.Render(th.Warn, fmt.Sprintf("Not connected yet: Channel %q has not reported Connected=True within %s.",
				name, timeout.Round(time.Second))),
			followUp)
	}
}

// conditionSentence renders one condition as "Type=Status (Reason): message",
// dropping the message when the writer left it empty. The controller's own
// wording is what carries the specifics, so it is passed through rather than
// paraphrased.
func conditionSentence(c *metav1.Condition) string {
	s := fmt.Sprintf("%s=%s (%s)", c.Type, c.Status, c.Reason)
	if c.Message != "" {
		s += ": " + c.Message
	}
	return s
}
