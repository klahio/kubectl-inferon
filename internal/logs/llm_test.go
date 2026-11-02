package logs

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/llmisvc"
)

func llmPod(name string, labels map[string]string) corev1.Pod {
	l := map[string]string{llmisvc.LabelPartOf: llmisvc.PartOfValue, llmisvc.LabelName: "smollm"}
	for k, v := range labels {
		l[k] = v
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: isvc.ContainerStorageInitial}},
			Containers:     []corev1.Container{{Name: llmisvc.ContainerServer}},
		},
	}
}

func TestSelectLLMInferenceService(t *testing.T) {
	pods := []corev1.Pod{
		llmPod("decode-0", map[string]string{llmisvc.LabelComponent: "workload", llmisvc.LabelRole: "decode", llmisvc.LabelLWSWorkerIndex: "0"}),
		llmPod("decode-0-1", map[string]string{llmisvc.LabelComponent: "workload", llmisvc.LabelRole: "decode", llmisvc.LabelLWSWorkerIndex: "1"}),
		llmPod("prefill-0", map[string]string{llmisvc.LabelComponent: "workload", llmisvc.LabelRole: "prefill"}),
	}
	tests := []struct {
		component string
		want      []string
	}{
		{"main", []string{"main/decode-0/main"}},
		{"worker", []string{"worker/decode-0-1/main"}},
		{"prefill", []string{"prefill/prefill-0/main"}},
		{ComponentAll, []string{"main/decode-0/main", "prefill/prefill-0/main", "worker/decode-0-1/main"}},
	}
	for _, tt := range tests {
		got := names(Select(pods, Options{Layout: LLMInferenceServiceLayout, Component: tt.component}))
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %v, want %v", tt.component, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: got %v, want %v", tt.component, got, tt.want)
				break
			}
		}
	}
	init := names(Select(pods[:1], Options{Layout: LLMInferenceServiceLayout, Component: "main", Init: true}))
	if len(init) != 1 || init[0] != "main/decode-0/storage-initializer" {
		t.Errorf("--init: %v", init)
	}
}
