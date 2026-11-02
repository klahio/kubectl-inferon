package status

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/kube"
)

func phaseNames(s *Startup) string {
	var names []string
	for _, p := range s.Phases {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// Seen on the lab cluster: after a node restart the pod keeps its creation and scheduling times, but its download and
// server ran again two days later. The phases must describe that run, not a two-day "pod start".
func TestStartupAfterSandboxRestart(t *testing.T) {
	s := loadFixture(t, "sklearn-iris").snapshot(t)
	p := s.Pods[0].DeepCopy()
	shift := 48 * time.Hour
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type != corev1.PodScheduled && c.Type != corev1.PodInitialized {
			c.LastTransitionTime = metav1.NewTime(c.LastTransitionTime.Add(shift))
		}
	}
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(p.Status.InitContainerStatuses[0].State.Terminated.StartedAt.Add(shift - time.Second)),
	})
	init := p.Status.InitContainerStatuses[0].State.Terminated
	init.StartedAt, init.FinishedAt = metav1.NewTime(init.StartedAt.Add(shift)), metav1.NewTime(init.FinishedAt.Add(shift))
	srv := p.Status.ContainerStatuses[0].State.Running
	srv.StartedAt = metav1.NewTime(srv.StartedAt.Add(shift))

	// Without restarts, a late sandbox is only a slow first network setup, and the phases keep the creation.
	if st := startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer); st.SandboxRestarted != nil || st.Phases[0].Name != PhaseScheduling {
		t.Errorf("slow first sandbox taken for a restart: %+v", st)
	}

	// The lab pod had restarted twice with the node.
	p.Status.ContainerStatuses[0].RestartCount = 2
	st := startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer)
	if st.SandboxRestarted == nil || phaseNames(st) != "pod start, model download, model load" || st.Restarts != 2 {
		t.Fatalf("got %+v", st)
	}
	if d := st.Phases[0].End.Sub(st.Phases[0].Start); d > time.Minute {
		t.Errorf("pod start took %s; it must start at the sandbox restart", d)
	}
}

// A download retry that is running now is in progress, not failed, even though the last attempt failed.
func TestStartupDownloadRetryInProgress(t *testing.T) {
	s := loadFixture(t, "bad-storage").snapshot(t)
	p := s.Pods[0].DeepCopy()
	cs := &p.Status.InitContainerStatuses[0]
	cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-5 * time.Second))}}
	st := startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer)
	last := st.Phases[len(st.Phases)-1]
	if st.Failed || last.Name != PhaseDownload || last.End != nil || !strings.HasPrefix(last.Note, "attempt ") {
		t.Errorf("got %+v", st)
	}
}

// A service mesh proxy that is not ready hides the server's own ready time.
func TestStartupServerReadyButPodNotReady(t *testing.T) {
	s := loadFixture(t, "sklearn-iris").snapshot(t)
	p := s.Pods[0].DeepCopy()
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == corev1.ContainersReady {
			p.Status.Conditions[i].Status = corev1.ConditionFalse
		}
	}
	st := startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer)
	if last := st.Phases[len(st.Phases)-1]; last.Name != PhaseLoad || !last.Untimed {
		t.Fatalf("got %+v", st)
	}
	var buf bytes.Buffer
	(TextRenderer{}).renderStartup(&printer{w: &buf}, st, time.Now())
	if strings.Contains(buf.String(), "so far") {
		t.Errorf("an untimed phase must not count up:\n%s", buf.String())
	}
}

func TestPodGPUWithSidecar(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	gpu := func(n int64) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)}}
	}
	p := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "sidecar", RestartPolicy: &always, Resources: gpu(1)},
			{Name: "setup", Resources: gpu(3)},
		},
		Containers: []corev1.Container{{Name: "server", Resources: gpu(1)}},
	}}
	// setup runs next to the sidecar (1+3); the server runs next to it too (1+1). The scheduler takes the larger.
	if n := podGPU(p)["nvidia.com/gpu"]; n != 4 {
		t.Errorf("effective request = %d, want 4", n)
	}
	p.Spec.InitContainers[1].Resources = gpu(0)
	if n := podGPU(p)["nvidia.com/gpu"]; n != 2 {
		t.Errorf("effective request = %d, want 2 (server plus sidecar)", n)
	}
}

func TestGPUWithoutPodPermission(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	k := kubefake.NewClientset(gpuNode("gpu-a", 4))
	k.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
	})
	s.GPU = collectGPU(context.Background(), kube.Clients{Kube: k}, s.Pods, s.podComponent, s.Unknown)
	if s.GPU == nil || !s.GPU.UsageUnknown || len(s.GPU.Nodes) != 1 {
		t.Fatalf("gpu = %+v", s.GPU)
	}
	for _, f := range Diagnose(s) {
		if strings.Contains(strings.Join(f.Details, " "), "free nvidia.com/gpu") {
			t.Errorf("free GPUs claimed without knowing the usage: %+v", f)
		}
	}
}

func TestStartupCrashingServer(t *testing.T) {
	s := loadFixture(t, "oom").snapshot(t)
	st := startupOf(&s.Pods[0], isvc.ContainerStorageInitial, isvc.ContainerServer)
	last := st.Phases[len(st.Phases)-1]
	if !st.Failed || last.Name != PhaseLoad || !strings.Contains(last.Note, "exit code 137") {
		t.Fatalf("got %+v", st)
	}
	var buf bytes.Buffer
	(TextRenderer{Now: time.Now()}).renderStartup(&printer{w: &buf}, st, time.Now())
	if strings.Contains(buf.String(), "total") {
		t.Errorf("a start that never completed has no total:\n%s", buf.String())
	}
}

func TestStartupUnscheduledPod(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	st := startupOf(&s.Pods[0], isvc.ContainerStorageInitial, isvc.ContainerServer)
	if phaseNames(st) != PhaseScheduling || st.Phases[0].End != nil {
		t.Fatalf("got %+v", st)
	}
}

func gpuNode(name string, allocatable int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelGPUProduct: "NVIDIA-L4"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			"nvidia.com/gpu": *resource.NewQuantity(allocatable, resource.DecimalSI),
		}},
	}
}

func gpuPod(namespace, name, node string, n int64, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name:      "c",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)}},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// The unschedulable sample asks for 8 GPUs on a lab node that has 4; the diagnosis must say how many are free.
func TestGPUShortage(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	c := kube.Clients{Kube: kubefake.NewClientset(
		gpuNode("gpu-a", 4), gpuNode("gpu-b", 2),
		gpuPod("other", "busy", "gpu-a", 3, corev1.PodRunning),
		gpuPod("other", "done", "gpu-a", 1, corev1.PodSucceeded),
	)}
	s.GPU = collectGPU(context.Background(), c, s.Pods, s.podComponent, s.Unknown)
	if s.GPU == nil || len(s.GPU.Requests) != 1 || s.GPU.Requests[0].PerPod != 8 || len(s.GPU.Nodes) != 2 {
		t.Fatalf("gpu = %+v", s.GPU)
	}
	if a := s.GPU.Nodes[0]; a.Name != "gpu-a" || a.Requested != 3 || a.Free() != 1 || a.Product != "NVIDIA-L4" {
		t.Errorf("node gpu-a = %+v", a)
	}

	var f *Finding
	for _, x := range Diagnose(s) {
		if x.Title == titleUnschedulable {
			f = &x
		}
	}
	if f == nil {
		t.Fatal("no scheduling finding")
	}
	details := strings.Join(f.Details, "\n")
	if !strings.Contains(details, "free nvidia.com/gpu: gpu-a 1 of 4, gpu-b 2 of 2") || !strings.Contains(f.Hint, "asks for 8 nvidia.com/gpu") || !strings.Contains(f.Hint, "more than 2 free") {
		t.Errorf("details %q, hint %q", details, f.Hint)
	}
}

func TestGPUWithoutNodePermission(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	k := kubefake.NewClientset()
	k.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("denied"))
	})
	g := collectGPU(context.Background(), kube.Clients{Kube: k}, s.Pods, s.podComponent, s.Unknown)
	if g == nil || len(g.Requests) != 1 || len(g.Nodes) != 0 || s.Unknown["gpu nodes"] != "no permission" {
		t.Errorf("gpu = %+v, unknown = %v", g, s.Unknown)
	}
}

func TestNoGPUSectionWithoutGPURequests(t *testing.T) {
	s := loadFixture(t, "sklearn-iris").snapshot(t)
	if g := collectGPU(context.Background(), kube.Clients{Kube: kubefake.NewClientset()}, s.Pods, s.podComponent, s.Unknown); g != nil {
		t.Errorf("gpu = %+v for a CPU model", g)
	}
}

// During a rollout the running pod asks for 8 GPUs and the pending one for 2; the hint must quote the pending pod.
func TestGPUShortageUsesThePendingPodsRequest(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	pending := &s.Pods[0]
	two := *resource.NewQuantity(2, resource.DecimalSI)
	pending.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = two
	if pending.Spec.Containers[0].Resources.Requests != nil {
		pending.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"] = two
	}
	running := gpuPod(pending.Namespace, "unschedulable-predictor-old", "gpu-a", 8, corev1.PodRunning)
	running.Labels = pending.Labels
	s.Pods = append(s.Pods, *running)
	c := kube.Clients{Kube: kubefake.NewClientset(gpuNode("gpu-a", 9), running)}
	s.GPU = collectGPU(context.Background(), c, s.Pods, s.podComponent, s.Unknown)
	for _, f := range Diagnose(s) {
		if f.Title != titleUnschedulable {
			continue
		}
		if !strings.Contains(f.Hint, "asks for 2 nvidia.com/gpu") || strings.Count(strings.Join(f.Details, "\n"), "free nvidia.com/gpu") != 1 {
			t.Errorf("details %q, hint %q", f.Details, f.Hint)
		}
		return
	}
	t.Fatal("no scheduling finding")
}

func TestGPUShortageWithoutGPUNodes(t *testing.T) {
	s := loadFixture(t, "unschedulable").snapshot(t)
	s.GPU = collectGPU(context.Background(), kube.Clients{Kube: kubefake.NewClientset()}, s.Pods, s.podComponent, s.Unknown)
	for _, f := range Diagnose(s) {
		if f.Title == titleUnschedulable && !strings.Contains(f.Hint, "no node offers") {
			t.Errorf("hint %q", f.Hint)
		}
	}
}

func TestMentionsInsufficient(t *testing.T) {
	if mentionsInsufficient("0/1 nodes are available: 1 Insufficient nvidia.com/gpumem.", "nvidia.com/gpu") {
		t.Error("nvidia.com/gpu must not match nvidia.com/gpumem")
	}
	if !mentionsInsufficient("0/1 nodes are available: 1 Insufficient nvidia.com/gpu.", "nvidia.com/gpu") {
		t.Error("the whole name must match")
	}
}

func TestRenderGPUOverCommitted(t *testing.T) {
	g := &GPU{Requests: []GPURequest{{Component: "predictor", Resource: "nvidia.com/gpu", PerPod: 1, Pods: 1}},
		Nodes: []GPUNode{{Name: "n", Resource: "nvidia.com/gpu", Allocatable: 8, Requested: 10}}}
	var buf bytes.Buffer
	(TextRenderer{}).renderGPU(&printer{w: &buf}, g)
	if !strings.Contains(buf.String(), "10 requested, -2 free") {
		t.Errorf("output:\n%s", buf.String())
	}
}

// A runtime that did not record when the download ended must not produce a year-long phase.
func TestStartupMissingFinishTime(t *testing.T) {
	s := loadFixture(t, "bad-storage").snapshot(t)
	p := s.Pods[0].DeepCopy()
	cs := &p.Status.InitContainerStatuses[0]
	cs.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, StartedAt: metav1.Now()}}
	var buf bytes.Buffer
	(TextRenderer{}).renderStartup(&printer{w: &buf}, startupOf(p, isvc.ContainerStorageInitial, isvc.ContainerServer), time.Now())
	if strings.Contains(buf.String(), "invalid") || strings.Contains(buf.String(), "y") {
		t.Errorf("output:\n%s", buf.String())
	}
}
