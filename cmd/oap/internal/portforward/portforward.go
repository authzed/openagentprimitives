// Package portforward wraps client-go's tools/portforward to expose a Service's
// port (via its backing Pods) on a localhost loopback. Callers pass an explicit
// label selector to choose the backing Pod and an optional desired local port
// (0 means OS-assigned).
package portforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForwarder represents a lazy port-forward. Start blocks until the local
// port is ready or the context is canceled; LocalPort returns the bound local
// port (only meaningful after a successful Start); Stop tears it down.
type PortForwarder struct {
	cfg           *rest.Config
	namespace     string
	service       string // informational
	selector      string // label selector used to pick the backing pod
	targetPort    uint16 // remote port on the pod
	requestedPort uint16 // desired local port; 0 → OS-assigned

	stopCh chan struct{}
	ready  chan struct{}
	// done receives the eventual return value of fwd.ForwardPorts: nil for a
	// clean shutdown via Stop / ctx-cancel, non-nil for a stream-side failure
	// after Start has succeeded. Buffered (1) so the goroutine never leaks.
	done  chan error
	local uint16 // bound after Start
}

// New constructs a PortForwarder. Callers must supply a non-empty label
// selector that selects the backing Pod for the given Service. requestedLocal
// is the desired local TCP port; 0 means "let the OS pick a free one".
func New(cfg *rest.Config, namespace, service, selector string, targetPort, requestedLocal uint16) (*PortForwarder, error) {
	if cfg == nil || namespace == "" || service == "" || selector == "" {
		return nil, errors.New("portforward.New: invalid args (cfg/namespace/service/selector required)")
	}
	return &PortForwarder{
		cfg:           cfg,
		namespace:     namespace,
		service:       service,
		selector:      selector,
		targetPort:    targetPort,
		requestedPort: requestedLocal,
	}, nil
}

// LocalPort returns the bound local port. Valid only after Start has returned
// nil. Returns 0 before Start.
func (p *PortForwarder) LocalPort() uint16 { return p.local }

// Done returns a channel that yields exactly one value: the eventual return
// value of the underlying ForwardPorts call (nil on clean shutdown, non-nil
// when the stream dies). Valid only after Start has returned nil. Callers
// who need to detect mid-session stream death should select on Done()
// alongside their own context cancellation.
func (p *PortForwarder) Done() <-chan error { return p.done }

// Start opens the port-forward to a Pod matched by the configured selector.
// Returns when the local port is bound and ready, or when ctx is canceled.
func (p *PortForwarder) Start(ctx context.Context, out io.Writer) error {
	clientset, err := kubernetes.NewForConfig(p.cfg)
	if err != nil {
		return err
	}
	pods, err := clientset.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: p.selector,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods in %s matching %q (service %q)", p.namespace, p.selector, p.service)
	}
	pod := readyPod(pods.Items)
	if pod == nil {
		return fmt.Errorf("no ready running pods in %s matching %q (service %q)", p.namespace, p.selector, p.service)
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(p.cfg)
	if err != nil {
		return fmt.Errorf("spdy: %w", err)
	}
	host, err := url.Parse(p.cfg.Host)
	if err != nil {
		return fmt.Errorf("parse host: %w", err)
	}
	target := &url.URL{
		Scheme: "https",
		Path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", p.namespace, pod.Name),
		Host:   host.Host,
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, "POST", target)

	p.stopCh = make(chan struct{})
	p.ready = make(chan struct{})

	// Build the ports spec for client-go's portforward: "<local>:<remote>" or
	// ":<remote>" when the caller wants OS assignment.
	var portSpec string
	if p.requestedPort == 0 {
		portSpec = fmt.Sprintf(":%d", p.targetPort)
	} else {
		portSpec = fmt.Sprintf("%d:%d", p.requestedPort, p.targetPort)
	}
	fwd, err := portforward.New(dialer, []string{portSpec}, p.stopCh, p.ready, out, out)
	if err != nil {
		return fmt.Errorf("portforward.New: %w", err)
	}

	p.done = make(chan error, 1)
	go func() { p.done <- fwd.ForwardPorts() }()

	select {
	case <-p.ready:
	case err := <-p.done:
		return err
	case <-ctx.Done():
		close(p.stopCh)
		return ctx.Err()
	}

	ports, err := fwd.GetPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return errors.New("no forwarded ports")
	}
	p.local = ports[0].Local
	return nil
}

// Retained evicted pods and terminating rollout replicas can precede the live
// service pod in a list. Forward only to a replica that can serve requests.
func readyPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return pod
			}
		}
	}
	return nil
}

// URL returns the localhost HTTP URL for the forwarded port. Convenience for
// HTTP callers — gRPC callers should read LocalPort and compose their own
// dial string.
func (p *PortForwarder) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.local)
}

// Stop terminates the forward. Safe to call multiple times.
func (p *PortForwarder) Stop() {
	if p.stopCh != nil {
		select {
		case <-p.stopCh:
		default:
			close(p.stopCh)
		}
	}
}
