package inference

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
	"github.com/klahio/kubectl-inferon/internal/status"
	"github.com/klahio/kubectl-inferon/internal/version"
)

// Port-forward modes of --port-forward.
const (
	PortForwardAuto   = "auto"
	PortForwardAlways = "always"
	PortForwardNever  = "never"
)

// probeTimeout is how long a route may take to accept a connection, or to answer a probe request, before the next
// route is tried.
const probeTimeout = 2 * time.Second

// portForwardTimeout bounds the port-forward set-up; --timeout covers only the inference request.
const portForwardTimeout = 30 * time.Second

// Endpoint is where the request goes and how it got there.
type Endpoint struct {
	BaseURL string
	// ConnectTo sends the TCP connection to this "address:port" instead of the URL's host, like curl --resolve.
	// The URL keeps the real host name, so the Host header, the TLS server name and certificate checks stay right.
	ConnectTo string
	// Route describes the route for the output, for example "port-forward to svc/sklearn-iris-predictor".
	Route string
	// InsecureTLS skips certificate checks; only for HTTPS through a port-forward to a self-signed server.
	InsecureTLS bool
	close       func()
}

// Close releases the route (stops a port-forward).
func (e *Endpoint) Close() {
	if e != nil && e.close != nil {
		e.close()
	}
}

// TargetComponent says which component receives the request: the transformer when there is one, otherwise the predictor.
func TargetComponent(m *isvc.InferenceService) string {
	for _, c := range m.Components {
		if c.Name == "transformer" {
			return "transformer"
		}
	}
	return "predictor"
}

// Target is what route discovery needs to know about one resource, whatever its kind.
type Target struct {
	External string
	// Routable is true only when the diagnosis found no problem and at least one Ingress or HTTPRoute publishes the
	// address. When the exposure objects could not be read, nothing vouches for the host name (see routable).
	Routable bool
	Exposure []status.Exposure
	// PortForward opens the last-resort route.
	PortForward func(ctx context.Context) (*Endpoint, error)
}

// InferenceServiceTarget routes to the transformer when there is one, otherwise the stable predictor.
func InferenceServiceTarget(c kube.Clients, sum *status.Summary, snap *status.Snapshot) Target {
	return Target{
		External: sum.Endpoints.External,
		Routable: routable(sum.Endpoints),
		Exposure: sum.Endpoints.Exposure,
		PortForward: func(ctx context.Context) (*Endpoint, error) {
			return portForwardISVC(ctx, c, snap, TargetComponent(snap.ISVC))
		},
	}
}

// LLMInferenceServiceTarget routes to the workload Service of an LLMInferenceService.
func LLMInferenceServiceTarget(c kube.Clients, sum *status.LLMSummary, snap *status.LLMSnapshot) Target {
	return Target{
		External: sum.Endpoints.External,
		Routable: routable(sum.Endpoints),
		Exposure: sum.Endpoints.Exposure,
		PortForward: func(ctx context.Context) (*Endpoint, error) {
			svc := llmWorkloadService(snap.Services)
			if svc == nil {
				return nil, fmt.Errorf("no workload Service found for LLMInferenceService %s/%s", snap.Service.Namespace, snap.Service.Name)
			}
			return portForwardService(ctx, c, svc, "llmisvc/"+snap.Service.Name)
		},
	}
}

func routable(e status.Endpoints) bool {
	return e.Routable && slices.ContainsFunc(e.Exposure, status.Exposure.Usable)
}

// llmWorkloadService picks the Service in front of the primary workload ("<name>-kserve-workload-svc").
func llmWorkloadService(svcs []corev1.Service) *corev1.Service {
	for i := range svcs {
		if svcs[i].Labels["app.kubernetes.io/component"] == "llminferenceservice-workload" {
			return &svcs[i]
		}
	}
	if len(svcs) > 0 {
		return &svcs[0]
	}
	return nil
}

// DiscoverOptions control the route selection.
type DiscoverOptions struct {
	URL         string
	PortForward string
}

// Discover picks the route, in the order of docs/mvp.md section 5:
// --url, the external address, the external host name connected to an Ingress or Gateway address, port-forward.
func Discover(ctx context.Context, t Target, o DiscoverOptions) (*Endpoint, error) {
	if o.URL != "" {
		if _, err := url.ParseRequestURI(o.URL); err != nil {
			return nil, fmt.Errorf("--url: %w", err)
		}
		return &Endpoint{BaseURL: trimSlash(o.URL), Route: "--url"}, nil
	}

	var tried []string
	if o.PortForward != PortForwardAlways && t.External != "" {
		ext, err := url.Parse(t.External)
		if err == nil {
			// The diagnosis already knows when the external address cannot work. Probing it anyway would be
			// dangerous: with KServe's default domain the name may resolve to a real, unrelated web site.
			if t.Routable {
				ep := &Endpoint{BaseURL: trimSlash(t.External), Route: "external address"}
				if reachable(ctx, hostPort(ext)) && answers(ctx, ep) {
					return ep, nil
				}
			}
			tried = append(tried, "external address "+ext.Host+" (not routable)")

			for _, e := range t.Exposure {
				if !e.Usable() {
					continue
				}
				addr := net.JoinHostPort(e.Address, port(ext))
				ep := &Endpoint{
					BaseURL:   trimSlash(t.External),
					ConnectTo: addr,
					Route:     fmt.Sprintf("%s %s at %s", e.Kind, e.Name, addr),
				}
				if reachable(ctx, addr) && answers(ctx, ep) {
					return ep, nil
				}
				tried = append(tried, fmt.Sprintf("%s address %s", e.Kind, addr))
			}
		}
	}
	if o.PortForward == PortForwardNever {
		if len(tried) == 0 {
			return nil, errors.New("no external address is published and --port-forward=never was given")
		}
		return nil, fmt.Errorf("no route works without port-forward (tried: %v)", tried)
	}
	return t.PortForward(ctx)
}

// client returns an HTTP client that honors ConnectTo.
func (e *Endpoint) client() *http.Client {
	if e.ConnectTo == "" && !e.InsecureTLS {
		return http.DefaultClient
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if e.ConnectTo != "" {
		dialer := &net.Dialer{}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, e.ConnectTo)
		}
	}
	if e.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // self-signed server behind a port-forward
	}
	return &http.Client{Transport: transport}
}

// answers sends a GET for the root path through the route, which no model server treats as an inference request.
// A TLS failure, a connection reset or a 502, 503 or 504 from a proxy means the route does not reach the model,
// and the next route is tried; any other response, even 404, comes from something behind the route.
func answers(ctx context.Context, ep *Endpoint) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodGet, ep, "/", nil)
	if err != nil {
		return false
	}
	resp, err := ep.client().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false
	}
	return true
}

func reachable(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// portForwardISVC finds the stable Service of an InferenceService component and forwards to it.
func portForwardISVC(ctx context.Context, c kube.Clients, snap *status.Snapshot, component string) (*Endpoint, error) {
	m := snap.ISVC
	svcs, err := c.Kube.CoreV1().Services(m.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{
		isvc.LabelInferenceService: m.Name,
		isvc.LabelComponent:        component,
	}.String()})
	if err != nil {
		return nil, err
	}
	// Canary Services carry the same component label; the stable one is told apart by its app label.
	var svc *corev1.Service
	for i := range svcs.Items {
		if m.CanaryOf(svcs.Items[i].Labels) == "" {
			svc = &svcs.Items[i]
			break
		}
	}
	if svc == nil {
		return nil, fmt.Errorf("no Service found for the %s of %s/%s", component, m.Namespace, m.Name)
	}
	return portForwardService(ctx, c, svc, "isvc/"+m.Name)
}

// portForwardService forwards a free local port to a ready pod behind a Service, as `kubectl port-forward svc/...`.
// statusArg is what to pass to `status` when no pod is ready.
func portForwardService(ctx context.Context, c kube.Clients, svc *corev1.Service, statusArg string) (*Endpoint, error) {
	if len(svc.Spec.Ports) == 0 {
		return nil, fmt.Errorf("service %s has no ports", svc.Name)
	}

	pods, err := c.Kube.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set(svc.Spec.Selector).String()})
	if err != nil {
		return nil, err
	}
	pod := readyPod(pods.Items)
	if pod == nil {
		return nil, fmt.Errorf("no ready pod behind svc/%s; run `%s status %s` to see why", svc.Name, version.Command, statusArg)
	}
	remote, err := containerPort(pod, svc.Spec.Ports[0].TargetPort)
	if err != nil {
		return nil, err
	}

	transport, upgrader, err := spdy.RoundTripperFor(c.Config)
	if err != nil {
		return nil, err
	}
	req := c.Kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	// Newer API servers prefer port-forward over WebSockets; fall back to SPDY like kubectl does.
	if ws, err := portforward.NewSPDYOverWebsocketDialer(req.URL(), c.Config); err == nil {
		dialer = portforward.NewFallbackDialer(ws, dialer, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}

	stop := make(chan struct{})
	ready := make(chan struct{})
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(remote)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return nil, err
	}
	failed := make(chan error, 1)
	go func() { failed <- pf.ForwardPorts() }()

	// The dialer takes no context: a server that accepts the connection but never answers the upgrade would block
	// forever, so the wait is bounded here.
	timeout := time.NewTimer(portForwardTimeout)
	defer timeout.Stop()
	select {
	case <-ready:
	case err := <-failed:
		return nil, fmt.Errorf("port-forward to pod %s: %w", pod.Name, err)
	case <-timeout.C:
		close(stop)
		return nil, fmt.Errorf("port-forward to pod %s did not start within %s", pod.Name, portForwardTimeout)
	case <-ctx.Done():
		close(stop)
		return nil, ctx.Err()
	}
	ports, err := pf.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stop)
		return nil, fmt.Errorf("port-forward to pod %s: no local port", pod.Name)
	}
	ep := &Endpoint{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local),
		Route:   "port-forward to svc/" + svc.Name,
		close:   func() { close(stop) },
	}
	if isHTTPS(svc.Spec.Ports[0]) {
		// The server's certificate names the Service, not 127.0.0.1, and is usually self-signed.
		ep.BaseURL = fmt.Sprintf("https://127.0.0.1:%d", ports[0].Local)
		ep.InsecureTLS = true
		ep.Route += " (HTTPS, certificate not verified)"
	}
	return ep, nil
}

func isHTTPS(p corev1.ServicePort) bool {
	return strings.Contains(strings.ToLower(p.Name), "https") || (p.AppProtocol != nil && strings.EqualFold(*p.AppProtocol, "https"))
}

func readyPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return p
			}
		}
	}
	return nil
}

// containerPort resolves a Service targetPort, which may be a number or the name of a container port.
func containerPort(pod *corev1.Pod, target intstr.IntOrString) (int, error) {
	if target.Type == intstr.Int {
		if target.IntValue() == 0 {
			return 0, errors.New("service has no targetPort")
		}
		return target.IntValue(), nil
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == target.StrVal {
				return int(p.ContainerPort), nil
			}
		}
	}
	return 0, fmt.Errorf("pod %s has no container port named %q", pod.Name, target.StrVal)
}

func hostPort(u *url.URL) string { return net.JoinHostPort(u.Hostname(), port(u)) }

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
