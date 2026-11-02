package status

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	kubepkg "github.com/klahio/kubectl-inferon/internal/kube"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

// The problem column uses the pod rules of `status`, so each broken sample names the same cause there.
func TestListItemProblems(t *testing.T) {
	cases := map[string]string{
		"sklearn-iris":  "",
		"bad-image":     "Image cannot be pulled",
		"bad-storage":   "Model download failed",
		"no-runtime":    "No serving runtime supports this model",
		"oom":           "Out of memory",
		"unschedulable": "Pod cannot be scheduled",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			s := loadFixture(t, name).snapshot(t)
			it := InferenceServiceItem(s.ISVC, s.Pods)
			if it.Problem != want || it.Ref() != "isvc/"+name || it.Model != "sklearn" && name != "no-runtime" {
				t.Errorf("got %+v, want problem %q", it, want)
			}
		})
	}
	for name, want := range map[string]string{
		"smollm":        "",
		"llm-libcuda":   "Server crashes on start",
		"llm-bad-model": "Model download failed",
	} {
		t.Run(name, func(t *testing.T) {
			s := loadLLMSnapshot(t, name)
			it := LLMInferenceServiceItem(s.Service, s.Pods)
			if it.Problem != want || it.Ref() != "llmisvc/"+name {
				t.Errorf("got %+v, want problem %q", it, want)
			}
		})
	}
}

func TestListReadsBothTypes(t *testing.T) {
	storage := loadFixture(t, "bad-storage")
	llmDir := filepath.Join("testdata", fixtureLLM, "smollm")
	llm := &unstructured.Unstructured{}
	readJSON(t, filepath.Join(llmDir, "llminferenceservice.json"), &llm.Object)

	objs := storage.typedObjects()
	for _, item := range listItems(t, filepath.Join(llmDir, "objects.json")) {
		if kindOf(t, item) == "Pod" {
			p := decode[corev1.Pod](t, item)
			objs = append(objs, &p)
		}
	}
	kube := kubefake.NewClientset(objs...)
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: "serving.kserve.io/v1alpha2", APIResources: []metav1.APIResource{{Name: "llminferenceservices"}}},
	}
	listKinds := map[schema.GroupVersionResource]string{
		isvc.InferenceServiceGVR: "InferenceServiceList",
		llmisvc.GVR("v1alpha2"):  "LLMInferenceServiceList",
	}
	c := kubepkg.Clients{Kube: kube, Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, storage.isvc, llm)}

	l, err := List(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range l.Items {
		got = append(got, it.Ref()+"="+it.Problem)
	}
	want := "isvc/bad-storage=Model download failed llmisvc/smollm="
	if strings.Join(got, " ") != want || len(l.Unknown) != 0 {
		t.Errorf("got %v (unknown %v), want %s", got, l.Unknown, want)
	}

	l, err = List(context.Background(), c, "other")
	if err != nil || len(l.Items) != 0 {
		t.Errorf("another namespace: %+v, %v", l, err)
	}
}

func TestRenderList(t *testing.T) {
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	l := &Listing{Items: []Item{
		{Type: TypeInferenceService, Namespace: "models", Name: "iris", Ready: "True", Model: "sklearn", Created: created},
		{Type: TypeLLMInferenceService, Namespace: "models", Name: "chat", Ready: "False", Problem: "Server crashes on start", Created: created},
	}}
	var buf bytes.Buffer
	if err := (TextRenderer{Now: created.Add(3 * time.Hour)}).RenderList(&buf, l, true); err != nil {
		t.Fatal(err)
	}
	want := `NAMESPACE   NAME           READY   MODEL     PROBLEM                   AGE
models      isvc/iris      True    sklearn   -                         3h
models      llmisvc/chat   False   -         Server crashes on start   3h
`
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// Seen on the lab cluster after a node restart: the kubelet rejected the old pods (phase Failed,
// UnexpectedAdmissionError) and the ReplicaSet replaced them. Their last exit is not a crash of the running service.
func TestDiagnoseIgnoresStoppedPodsNextToLiveOnes(t *testing.T) {
	s := loadLLMSnapshot(t, "smollm")
	old := *s.Pods[0].DeepCopy()
	old.Name = "smollm-kserve-old"
	old.Status.Phase = corev1.PodFailed
	old.Status.Reason = "UnexpectedAdmissionError"
	old.Status.ContainerStatuses[0].Ready = false
	old.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 255, Reason: "Unknown"}}
	s.Pods = append(s.Pods, old)
	if errs := errorsOnly(DiagnoseLLM(s)); len(errs) != 0 {
		t.Errorf("stopped pod reported next to a live one: %+v", errs)
	}

	s.Pods = []corev1.Pod{old}
	if errs := errorsOnly(DiagnoseLLM(s)); len(errs) == 0 {
		t.Error("with only the stopped pod left, its exit is the evidence and must be reported")
	}
}

func TestListFailsWhenEveryTypeIsRefused(t *testing.T) {
	kube := kubefake.NewClientset()
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = nil
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{isvc.InferenceServiceGVR: "InferenceServiceList"})
	dyn.PrependReactor("list", "inferenceservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "inferenceservices"}, "", errors.New("denied"))
	})
	_, err := List(context.Background(), kubepkg.Clients{Kube: kube, Dynamic: dyn}, "")
	if err == nil || !strings.Contains(err.Error(), "no permission (isvc)") {
		t.Errorf("err = %v", err)
	}
}
