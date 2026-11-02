package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/klahio/kubectl-inferon/internal/isvc"
)

// Fixtures are captured from real clusters with hack/capture-fixtures.sh, one directory per KServe version and setup.
const (
	fixtureVersion    = "v0.21"             // Kubernetes Ingress (KServe's default)
	fixtureGatewayAPI = "v0.21-gateway-api" // KServe switched to Gateway API on the lab Gateway
)

// fixture holds the typed and unstructured objects captured around one InferenceService.
type fixture struct {
	isvc        *unstructured.Unstructured
	pods        []corev1.Pod
	deployments []appsv1.Deployment
	autoscalers []autoscalingv2.HorizontalPodAutoscaler
	services    []corev1.Service
	ingresses   []networkingv1.Ingress
	classes     []networkingv1.IngressClass
	routes      []unstructured.Unstructured
	gateways    []unstructured.Unstructured
	runtimes    []unstructured.Unstructured
}

func loadFixture(t *testing.T, name string) *fixture {
	t.Helper()
	return loadFixtureFrom(t, fixtureVersion, name)
}

func loadFixtureFrom(t *testing.T, set, name string) *fixture {
	t.Helper()
	root := filepath.Join("testdata", set)
	dir := filepath.Join(root, name)

	f := &fixture{isvc: &unstructured.Unstructured{}}
	readJSON(t, filepath.Join(dir, "inferenceservice.json"), &f.isvc.Object)

	for _, item := range listItems(t, filepath.Join(dir, "objects.json")) {
		switch kindOf(t, item) {
		case "Pod":
			f.pods = append(f.pods, decode[corev1.Pod](t, item))
		case "Deployment":
			f.deployments = append(f.deployments, decode[appsv1.Deployment](t, item))
		case "HorizontalPodAutoscaler":
			f.autoscalers = append(f.autoscalers, decode[autoscalingv2.HorizontalPodAutoscaler](t, item))
		case "Service":
			f.services = append(f.services, decode[corev1.Service](t, item))
		}
	}
	for _, item := range listItems(t, filepath.Join(dir, "ingresses.json")) {
		f.ingresses = append(f.ingresses, decode[networkingv1.Ingress](t, item))
	}
	for _, item := range listItems(t, filepath.Join(root, "ingressclasses.json")) {
		f.classes = append(f.classes, decode[networkingv1.IngressClass](t, item))
	}
	f.routes = unstructuredItems(t, filepath.Join(dir, "httproutes.json"))
	f.gateways = unstructuredItems(t, filepath.Join(root, "gateways.json"))
	f.runtimes = unstructuredItems(t, filepath.Join(root, "clusterservingruntimes.json"))
	return f
}

// snapshot builds the Snapshot that Collect would return, without a cluster.
func (f *fixture) snapshot(t *testing.T) *Snapshot {
	t.Helper()
	m, err := isvc.FromUnstructured(f.isvc)
	if err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{
		ISVC:           m,
		Pods:           f.pods,
		Deployments:    f.deployments,
		Autoscalers:    f.autoscalers,
		IngressClasses: f.classes,
		Unknown:        map[string]string{},
	}
	for _, ing := range f.ingresses {
		if ownedBy(ing.OwnerReferences, m.UID) {
			s.Ingresses = append(s.Ingresses, ing)
		}
	}
	for _, r := range f.routes {
		if ownedBy(r.GetOwnerReferences(), m.UID) {
			s.HTTPRoutes = append(s.HTTPRoutes, r)
		}
	}
	s.Gateways = f.gateways
	if m.Model.Runtime != "" {
		s.Runtime = &Runtime{Name: m.Model.Runtime, Kind: m.Model.RuntimeKind}
		for _, rt := range f.runtimes {
			if rt.GetName() == m.Model.Runtime {
				s.Runtime.Found = true
				s.Runtime.ProtocolVersions, _, _ = unstructured.NestedStringSlice(rt.Object, "spec", "protocolVersions")
			}
		}
	}
	return s
}

// typedObjects returns the captured objects for the fake Kubernetes clientset.
func (f *fixture) typedObjects() []runtime.Object {
	var out []runtime.Object
	for i := range f.pods {
		out = append(out, &f.pods[i])
	}
	for i := range f.deployments {
		out = append(out, &f.deployments[i])
	}
	for i := range f.autoscalers {
		out = append(out, &f.autoscalers[i])
	}
	for i := range f.services {
		out = append(out, &f.services[i])
	}
	for i := range f.ingresses {
		out = append(out, &f.ingresses[i])
	}
	for i := range f.classes {
		out = append(out, &f.classes[i])
	}
	return out
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func listItems(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	readJSON(t, path, &list)
	return list.Items
}

func unstructuredItems(t *testing.T, path string) []unstructured.Unstructured {
	t.Helper()
	var out []unstructured.Unstructured
	for _, item := range listItems(t, path) {
		u := unstructured.Unstructured{}
		if err := json.Unmarshal(item, &u.Object); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func kindOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var meta struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Kind
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
