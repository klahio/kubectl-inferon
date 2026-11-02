package status

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	kubepkg "github.com/klahio/kubectl-inferon/internal/kube"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

// LLMInferenceService fixtures, captured with hack/capture-llm-fixtures.sh from the lab cluster (KServe v0.21).
const fixtureLLM = "v0.21-llmisvc"

func loadLLMSnapshot(t *testing.T, name string) *LLMSnapshot {
	t.Helper()
	dir := filepath.Join("testdata", fixtureLLM, name)
	u := &unstructured.Unstructured{}
	readJSON(t, filepath.Join(dir, "llminferenceservice.json"), &u.Object)
	svc, err := llmisvc.FromUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}
	s := &LLMSnapshot{Service: svc, Unknown: map[string]string{}}
	for _, item := range listItems(t, filepath.Join(dir, "objects.json")) {
		switch kindOf(t, item) {
		case "Pod":
			s.Pods = append(s.Pods, decode[corev1.Pod](t, item))
		case "Deployment":
			s.Deployments = append(s.Deployments, decode[appsv1.Deployment](t, item))
		case "Service":
			s.Services = append(s.Services, decode[corev1.Service](t, item))
		}
	}
	return s
}

func TestDiagnoseLLMHealthy(t *testing.T) {
	s := loadLLMSnapshot(t, "smollm")
	findings := DiagnoseLLM(s)
	if len(findings) != 0 {
		t.Fatalf("healthy sample should have no findings: %+v", findings)
	}
	sum := SummarizeLLM(s, findings)
	if sum.Ready != "True" || sum.Endpoints.InCluster != "http://smollm-kserve-workload-svc.kserve-test.svc.cluster.local" || sum.Endpoints.External != "" {
		t.Errorf("ready=%q endpoints=%+v", sum.Ready, sum.Endpoints)
	}
	if sum.Model.Image != "ghcr.io/llm-d/llm-d-cuda:v0.9.0" || len(sum.Model.Presets) != 1 || sum.Model.Presets[0] != "kserve-config-llm-template" {
		t.Errorf("model = %+v", sum.Model)
	}
	if len(sum.Workloads) != 1 || sum.Workloads[0] != (WorkloadState{Role: "primary", Kind: "Deployment", Name: "smollm-kserve", ReadyReplicas: 1, Replicas: 1}) {
		t.Errorf("workloads = %+v", sum.Workloads)
	}
}

func TestDiagnoseLLMBadModel(t *testing.T) {
	errs := errorsOnly(DiagnoseLLM(loadLLMSnapshot(t, "llm-bad-model")))
	if len(errs) != 1 || errs[0].Title != "Model download failed" {
		t.Fatalf("want one model download failure, got %+v", errs)
	}
	if !strings.Contains(errs[0].Hint, "spec.model.uri") {
		t.Errorf("hint must name the LLMInferenceService field: %q", errs[0].Hint)
	}
}

// The lab cluster's crash: the last log line says only "Engine core initialization failed"; the scan must find the
// real cause dozens of lines earlier.
func TestDiagnoseLLMLibcudaCrash(t *testing.T) {
	errs := errorsOnly(DiagnoseLLM(loadLLMSnapshot(t, "llm-libcuda")))
	if len(errs) != 1 || errs[0].Title != "Server crashes on start" || errs[0].Logs == nil || !errs[0].Logs.Scan {
		t.Fatalf("want one crash finding that asks for a log scan, got %+v", errs)
	}
	data, err := os.ReadFile(filepath.Join("testdata", fixtureLLM, "llm-libcuda", "main-previous.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if last := strings.TrimSpace(lines[len(lines)-2]); !strings.Contains(last, "Engine core initialization failed") {
		t.Fatalf("fixture's last line is %q; the test needs the misleading one", last)
	}
	cause, evidence := ExplainLog(lines)
	if cause == nil || !strings.Contains(cause.Hint, "TRITON_LIBCUDA_PATH") {
		t.Fatalf("cause = %+v", cause)
	}
	if len(evidence) != 1 || !strings.Contains(evidence[0], "cannot find -l:libcuda.so.1") {
		t.Errorf("evidence = %q", evidence)
	}
}

func TestExplainLogFallsBackToErrorLines(t *testing.T) {
	lines := []string{
		"(APIServer pid=1) INFO starting",
		`(EngineCore pid=7) ERROR 10-29 23:48:18 [core.py:1330]   File "/opt/vllm/x.py", line 3, in f`,
		"(EngineCore pid=7) ERROR 10-29 23:48:18 [core.py:1330] ValueError: something specific broke",
		"(EngineCore pid=8) ERROR 10-29 23:48:19 [core.py:1330] ValueError: something specific broke",
		"(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above.",
	}
	cause, evidence := ExplainLog(lines)
	if cause != nil {
		t.Fatalf("no known cause expected, got %+v", cause)
	}
	if len(evidence) != 2 || !strings.Contains(evidence[0], "something specific broke") {
		t.Errorf("evidence = %q (want the specific error first, repeats merged, Python frames skipped)", evidence)
	}
}

func TestDiagnoseLLMStillDownloading(t *testing.T) {
	s := loadLLMSnapshot(t, "llm-bad-model")
	p := &s.Pods[0]
	p.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	p.Status.InitContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	findings := DiagnoseLLM(s)
	if len(findings) != 1 || findings[0].Severity != SeverityInfo || findings[0].Title != "Downloading the model" {
		t.Fatalf("a running download is not a failure: %+v", findings)
	}
}

func TestRenderLLMGolden(t *testing.T) {
	s := loadLLMSnapshot(t, "smollm")
	sum := SummarizeLLM(s, DiagnoseLLM(s))
	var buf bytes.Buffer
	if err := (TextRenderer{Now: sum.Created.Add(33 * time.Minute)}).RenderLLM(&buf, sum); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden", fixtureLLM+"-smollm.txt")
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, buf.String(), want)
	}
}

// v1alpha1 (KServe v0.16) has no status.workloads; the workloads are still found through their labels.
func TestSummarizeLLMWithoutStatusWorkloads(t *testing.T) {
	s := loadLLMSnapshot(t, "smollm")
	s.Service.Workloads, s.Service.WorkloadsReported = nil, false
	sum := SummarizeLLM(s, DiagnoseLLM(s))
	if len(sum.Workloads) != 1 || sum.Workloads[0].Name != "smollm-kserve" || sum.Workloads[0].Replicas != 1 {
		t.Errorf("workloads = %+v", sum.Workloads)
	}
}

func TestCollectLLMPrefersNewestServedVersion(t *testing.T) {
	dir := filepath.Join("testdata", fixtureLLM, "smollm")
	u := &unstructured.Unstructured{}
	readJSON(t, filepath.Join(dir, "llminferenceservice.json"), &u.Object)
	var objs []runtime.Object
	for _, item := range listItems(t, filepath.Join(dir, "objects.json")) {
		switch kindOf(t, item) {
		case "Pod":
			p := decode[corev1.Pod](t, item)
			objs = append(objs, &p)
		case "Deployment":
			d := decode[appsv1.Deployment](t, item)
			objs = append(objs, &d)
		case "Service":
			sv := decode[corev1.Service](t, item)
			objs = append(objs, &sv)
		}
	}
	kube := kubefake.NewClientset(objs...)
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: "serving.kserve.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "llminferenceservices"}}},
		{GroupVersion: "serving.kserve.io/v1alpha2", APIResources: []metav1.APIResource{{Name: "llminferenceservices"}}},
	}
	listKinds := map[schema.GroupVersionResource]string{
		llmisvc.GVR("v1alpha2"): "LLMInferenceServiceList",
		httpRouteGVR:            "HTTPRouteList",
		gatewayGVR:              "GatewayList",
	}
	c := kubepkg.Clients{Kube: kube, Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, u)}

	s, err := CollectLLM(context.Background(), c, "kserve-test", "smollm", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Service.APIVersion != "serving.kserve.io/v1alpha2" || len(s.Pods) != 1 || len(s.Deployments) != 1 || len(s.Services) != 1 {
		t.Errorf("apiVersion=%q pods=%d deployments=%d services=%d", s.Service.APIVersion, len(s.Pods), len(s.Deployments), len(s.Services))
	}
	if _, err := CollectLLM(context.Background(), c, "kserve-test", "missing", false); !errors.Is(err, kubepkg.ErrNotFound) {
		t.Errorf("missing service: err = %v", err)
	}

	empty := kubefake.NewClientset()
	empty.Discovery().(*fakediscovery.FakeDiscovery).Resources = nil
	if _, err := CollectLLM(context.Background(), kubepkg.Clients{Kube: empty, Dynamic: c.Dynamic}, "kserve-test", "smollm", false); !errors.Is(err, kubepkg.ErrNotInstalled) {
		t.Errorf("no CRD: err = %v", err)
	}
}
