package wait

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayAddress reports the Gateway's first load-balancer address and whether
// one is present yet (present=false → the controller has not provisioned the LB
// address). It is the single-shot check; callers that want to wait wrap it in a
// poll (ForGatewayAddress) or in cliout.Await for a spinner-backed wait.
func GatewayAddress(ctx context.Context, c client.Client, namespace, name string) (addr string, present bool, err error) {
	var gw gatewayv1.Gateway
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &gw); err != nil {
		return "", false, err
	}
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			return a.Value, true, nil
		}
	}
	return "", false, nil
}

// gceWedgeMarkers are substrings the GKE managed-gateway controller writes into
// a Gateway's Programmed / GatewayHealthy condition message when its underlying
// GCE load-balancer program is stuck — a duplicate/zombie compute operation that
// hangs at progress 0, leaving the backend service "not ready" so the URL map,
// target proxy, and forwarding rule are never created and no address is ever
// assigned. This is NOT the benign "still provisioning" path (which resolves on
// its own in minutes); it is a wedge that will not clear until the stuck cloud
// operation is removed, so it is worth calling out explicitly rather than
// letting the address wait silently burn its whole budget.
var gceWedgeMarkers = []string{
	"gceSync",
	"deadline_exceeded",
	"context deadline exceeded",
	"RESOURCE_NOT_READY",
	"is not ready",
}

// maxDiagMessage caps a condition message so a multi-line GCE error (the
// controller repeats "context deadline exceeded" several times) renders as one
// readable line instead of flooding the install output.
const maxDiagMessage = 200

func isGCEWedge(cond metav1.Condition) bool {
	if cond.Status != metav1.ConditionFalse {
		return false
	}
	// Invalid (Programmed) / Error (GatewayHealthy) are the reasons GKE uses for
	// a hard programming failure; the benign in-progress states use other reasons.
	if cond.Reason != "Invalid" && cond.Reason != "Error" {
		return false
	}
	for _, m := range gceWedgeMarkers {
		if strings.Contains(cond.Message, m) {
			return true
		}
	}
	return false
}

// truncateMessage collapses a multi-line condition message to its first line
// and caps it at maxDiagMessage display columns.
//
// The cap is spent in display columns, not bytes: a controller's message is
// arbitrary text (a quoted resource name, a provider error), and a byte offset
// can land inside a multi-byte rune — which prints replacement characters in
// the middle of the one line the operator has to diagnose from.
func truncateMessage(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return ansi.Truncate(strings.TrimSpace(msg), maxDiagMessage, "…")
}

// DiagnoseGatewayAddress explains why a Gateway still has no load-balancer
// address. It returns an empty Diagnosis (the caller keeps waiting quietly) when
// the address is already present or there is nothing blocking to report.
// Otherwise it surfaces the Gateway's non-True status conditions and, when those
// carry the GKE GCE-sync wedge signature (a stuck duplicate compute operation
// keeping the backend service "not ready"), sets a Headline flagging that the
// load-balancer program is wedged and the address will not arrive until the
// stuck cloud operation is cleared. Best-effort: a non-nil error is reserved for
// a hard failure to read the Gateway itself.
func DiagnoseGatewayAddress(ctx context.Context, c client.Client, namespace, name string) (Diagnosis, error) {
	var gw gatewayv1.Gateway
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &gw); err != nil {
		return Diagnosis{Workload: "gateway/" + name}, fmt.Errorf("get gateway %s/%s: %w", namespace, name, err)
	}
	// Address already present — nothing to diagnose (poll race: the address
	// landed between the poll and the diagnose call).
	for _, a := range gw.Status.Addresses {
		if a.Value != "" {
			return Diagnosis{}, nil
		}
	}

	diag := Diagnosis{Workload: "gateway/" + name}
	wedged := false
	for _, cond := range gw.Status.Conditions {
		if cond.Status == metav1.ConditionTrue {
			continue // the satisfied conditions don't explain the missing address
		}
		// Scheduled and Ready are deprecated/noise on GKE — Programmed and
		// GatewayHealthy carry the real reason. Skip the deprecated pair.
		if cond.Type == "Scheduled" || cond.Type == "Ready" {
			continue
		}
		diag.Conditions = append(diag.Conditions, ConditionNote{
			Type:    cond.Type,
			Status:  string(cond.Status),
			Reason:  cond.Reason,
			Message: truncateMessage(cond.Message),
		})
		if isGCEWedge(cond) {
			wedged = true
		}
	}
	if wedged {
		diag.Headline = "GKE load-balancer programming appears wedged: a managed-gateway GCE operation is stuck " +
			"(gceSync deadline / backend service not ready), so no address will be assigned until it clears. " +
			"Inspect with: gcloud compute operations list --global --filter=\"status=RUNNING\" — a stuck duplicate " +
			"insert on the backend service blocks the URL map and forwarding rule."
	}
	return diag, nil
}

// ForGatewayAddress polls a Gateway until status.addresses has a value (the
// cloud load balancer the controller provisioned), returning the first address.
// Times out per `deadline`. On managed clouds this is typically seconds-to-a-
// couple-minutes; on kind without cloud-provider-kind it never resolves.
func ForGatewayAddress(ctx context.Context, c client.Client, namespace, name string, interval, deadline time.Duration) (string, error) {
	var addr string
	err := Until(ctx, interval, deadline, func(ctx context.Context) (bool, error) {
		a, present, err := GatewayAddress(ctx, c, namespace, name)
		if err != nil {
			return false, err
		}
		if present {
			addr = a
		}
		return present, nil
	})
	if err != nil {
		return "", fmt.Errorf("waiting for Gateway %s/%s load-balancer address: %w", namespace, name, err)
	}
	return addr, nil
}
