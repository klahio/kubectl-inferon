package logs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/klahio/kubectl-inferon/internal/isvc"
)

func pod(name, component string, running bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{isvc.LabelInferenceService: "m", isvc.LabelComponent: component},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: isvc.ContainerStorageInitial}},
			Containers:     []corev1.Container{{Name: isvc.ContainerServer}, {Name: "agent"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if !running {
		p.Status.Phase = corev1.PodPending
		state = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: isvc.ContainerStorageInitial,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: isvc.ContainerServer, State: state}, {Name: "agent", State: state}}
	return p
}

func names(streams []Stream) []string {
	var out []string
	for _, s := range streams {
		out = append(out, s.Label())
	}
	return out
}

func TestSelect(t *testing.T) {
	pods := []corev1.Pod{pod("t-1", "transformer", true), pod("p-2", "predictor", true), pod("p-1", "predictor", true)}
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{"default: model server of every predictor pod",
			Options{Component: "predictor"},
			[]string{"predictor/p-1/kserve-container", "predictor/p-2/kserve-container"}},
		{"init: storage-initializer only",
			Options{Component: "predictor", Init: true},
			[]string{"predictor/p-1/storage-initializer", "predictor/p-2/storage-initializer"}},
		{"all components, predictor first",
			Options{Component: ComponentAll},
			[]string{"predictor/p-1/kserve-container", "predictor/p-2/kserve-container", "transformer/t-1/kserve-container"}},
		{"all containers of one component",
			Options{Component: "transformer", AllContainers: true},
			[]string{"transformer/t-1/storage-initializer", "transformer/t-1/kserve-container", "transformer/t-1/agent"}},
		{"component without pods",
			Options{Component: "explainer"},
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := names(Select(pods, tt.opts))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectCustomContainerName(t *testing.T) {
	p := pod("p-1", "predictor", true)
	p.Spec.Containers = []corev1.Container{{Name: "my-server"}, {Name: "sidecar"}}
	got := names(Select([]corev1.Pod{p}, Options{Component: "predictor"}))
	if len(got) != 1 || got[0] != "predictor/p-1/my-server" {
		t.Errorf("got %v, want the first container", got)
	}
}

func TestNotStarted(t *testing.T) {
	waiting := pod("p", "predictor", false)
	if reason, ok := notStarted(&waiting, isvc.ContainerServer); !ok || reason != "ImagePullBackOff" {
		t.Errorf("waiting container: reason=%q ok=%v", reason, ok)
	}
	running := pod("p", "predictor", true)
	if _, ok := notStarted(&running, isvc.ContainerServer); ok {
		t.Error("running container must be readable")
	}
	crashing := pod("p", "predictor", true)
	crashing.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name:                 isvc.ContainerServer,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}
	if _, ok := notStarted(&crashing, isvc.ContainerServer); ok {
		t.Error("a container in CrashLoopBackOff still has the log of its last run")
	}
	if _, ok := notStarted(nil, isvc.ContainerServer); !ok {
		t.Error("a missing pod has no log")
	}
}

func run(t *testing.T, opts Options, pods ...corev1.Pod) (string, string, error) {
	t.Helper()
	client := kubefake.NewClientset()
	for i := range pods {
		if _, err := client.CoreV1().Pods("ns").Create(context.Background(), &pods[i], metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var out, notes bytes.Buffer
	s := &Streamer{Kube: client, Namespace: "ns", Name: "m", Options: opts, Out: &out, Notes: &notes}
	err := s.Run(context.Background())
	return out.String(), notes.String(), err
}

// The fake clientset answers every log request with the body "fake logs".
func TestRunPrefixAuto(t *testing.T) {
	out, _, err := run(t, Options{Component: "predictor", Tail: -1}, pod("p-1", "predictor", true))
	if err != nil {
		t.Fatal(err)
	}
	if out != "fake logs\n" {
		t.Errorf("one stream must not be prefixed, got %q", out)
	}

	out, _, err = run(t, Options{Component: "predictor", Tail: 10}, pod("p-1", "predictor", true), pod("p-2", "predictor", true))
	if err != nil {
		t.Fatal(err)
	}
	want := "[predictor/p-1/kserve-container] fake logs\n[predictor/p-2/kserve-container] fake logs\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestRunPrefixForcedOff(t *testing.T) {
	off := false
	out, _, err := run(t, Options{Component: "predictor", Prefix: &off}, pod("p-1", "predictor", true), pod("p-2", "predictor", true))
	if err != nil {
		t.Fatal(err)
	}
	if out != "fake logs\nfake logs\n" {
		t.Errorf("got %q", out)
	}
}

func TestRunNoPods(t *testing.T) {
	_, _, err := run(t, Options{Component: "predictor"})
	if !errors.Is(err, ErrNoPods) {
		t.Fatalf("err = %v, want ErrNoPods", err)
	}
}

func TestRunNothingReadable(t *testing.T) {
	_, notes, err := run(t, Options{Component: "predictor"}, pod("p-1", "predictor", false))
	if !errors.Is(err, ErrNoneReadable) {
		t.Fatalf("err = %v, want ErrNoneReadable", err)
	}
	if !strings.Contains(notes, "[predictor/p-1/kserve-container] not started yet: ImagePullBackOff") {
		t.Errorf("notes = %q", notes)
	}
}

// Pods the kubelet rejected after a node restart stay in phase Failed next to their replacement; their logs are
// left out while a live pod exists, and kept when they are all there is.
func TestSelectSkipsStoppedPods(t *testing.T) {
	live := pod("m-predictor-new", "predictor", true)
	old := pod("m-predictor-old", "predictor", true)
	old.Status.Phase = corev1.PodFailed
	opts := Options{Component: "predictor"}
	if got := Select([]corev1.Pod{old, live}, opts); len(got) != 1 || got[0].Pod != live.Name {
		t.Errorf("want only the live pod, got %+v", got)
	}
	if got := Select([]corev1.Pod{old}, opts); len(got) != 1 {
		t.Error("with only the stopped pod left, its logs must be selected")
	}
}
