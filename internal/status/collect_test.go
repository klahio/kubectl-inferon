package status

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
)

func fakeClients(f *fixture) kube.Clients {
	listKinds := map[schema.GroupVersionResource]string{
		isvc.InferenceServiceGVR:      "InferenceServiceList",
		isvc.ServingRuntimeGVR:        "ServingRuntimeList",
		isvc.ClusterServingRuntimeGVR: "ClusterServingRuntimeList",
		httpRouteGVR:                  "HTTPRouteList",
		gatewayGVR:                    "GatewayList",
	}
	objs := []runtime.Object{f.isvc}
	for i := range f.runtimes {
		objs = append(objs, &f.runtimes[i])
	}
	for i := range f.routes {
		objs = append(objs, &f.routes[i])
	}
	for i := range f.gateways {
		objs = append(objs, &f.gateways[i])
	}
	return kube.Clients{
		Kube:    kubefake.NewClientset(f.typedObjects()...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...),
	}
}

func TestCollectHealthy(t *testing.T) {
	f := loadFixture(t, "sklearn-iris")
	s, err := Collect(context.Background(), fakeClients(f), "kserve-test", "sklearn-iris", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.ISVC.Mode != "Standard" || s.ISVC.Ready() != "True" {
		t.Errorf("mode=%q ready=%q", s.ISVC.Mode, s.ISVC.Ready())
	}
	if s.Runtime == nil || !s.Runtime.Found || s.Runtime.Name != "kserve-sklearnserver" {
		t.Fatalf("runtime = %+v", s.Runtime)
	}
	if got := s.Runtime.ProtocolVersions; len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
		t.Errorf("protocols = %v", got)
	}
	if len(s.Pods) != 1 || len(s.Deployments) != 1 || len(s.Autoscalers) != 1 {
		t.Errorf("pods=%d deployments=%d autoscalers=%d", len(s.Pods), len(s.Deployments), len(s.Autoscalers))
	}
	// Only the Ingress owned by this InferenceService, found through its owner reference.
	if len(s.Ingresses) != 1 || s.Ingresses[0].Name != "sklearn-iris" {
		t.Errorf("ingresses = %d", len(s.Ingresses))
	}
	if len(s.Unknown) != 0 {
		t.Errorf("unknown = %v", s.Unknown)
	}
}

func TestCollectNotFound(t *testing.T) {
	f := loadFixture(t, "sklearn-iris")
	_, err := Collect(context.Background(), fakeClients(f), "kserve-test", "missing", false)
	if !errors.Is(err, kube.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
