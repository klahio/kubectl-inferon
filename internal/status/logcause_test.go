package status

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// One log line per known cause, in the wording of the vLLM, PyTorch and transformers releases they come from, with
// the prefix vLLM puts on its lines. Each must be recognized among unrelated start-up output.
func TestExplainLogKnownCauses(t *testing.T) {
	prefix := "(EngineCore pid=236) ERROR 11-02 08:12:01 [core.py:1330] "
	cases := map[string]string{
		"ValueError: To serve at least one request with the models's max seq len (8192), (0.18 GiB KV cache is needed, which is larger than the available KV cache memory (0.05 GiB).":                        "KV cache is too small",
		"ValueError: The model's max seq len (8192) is larger than the maximum number of tokens that can be stored in KV cache (1520).":                                                                       "KV cache is too small",
		"ValueError: Free memory on device (4.1/15.9 GiB) on startup is less than desired GPU memory utilization (0.9, 14.3 GiB).":                                                                            "other processes already use the GPU memory",
		"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB.":                                                                                                                             "GPU memory is too small",
		"ValueError: Tensor parallel size (2) cannot be larger than the number of available GPUs (1).":                                                                                                        "parallel size needs more GPUs",
		"  Value error, World size (2) is larger than the number of available GPUs (1) in this node. If this is intentional and you are using:":                                                               "parallel size needs more GPUs",
		"RuntimeError: Error while creating shared memory segment /dev/shm/nccl-x2Zq (size 9637888)":                                                                                                          "shared memory (/dev/shm) is too small",
		"ValueError: Model architectures ['FooForCausalLM'] are not supported for now.":                                                                                                                       "does not support the model's architecture",
		"ValueError: The repository for org/model contains custom code which must be executed to correctly load the model. Please pass the argument `trust_remote_code=True` to allow custom code to be run.": "needs custom code",
		"huggingface_hub.errors.GatedRepoError: 401 Client Error. Cannot access gated repo for url https://huggingface.co/x":                                                                                  "Hugging Face refuses",
	}
	noise := []string{
		prefix + "INFO Initializing a V1 LLM engine with config: model='x', trust_remote_code=False",
		prefix + "INFO non-default args: {'model': 'x', 'max_model_len': 8192}",
	}
	for line, want := range cases {
		cause, evidence := ExplainLog(append(append([]string(nil), noise...), prefix+line, prefix+"Engine core initialization failed"))
		if cause == nil || !strings.Contains(cause.Meaning, want) || len(evidence) != 1 || !strings.Contains(evidence[0], line[:20]) {
			t.Errorf("%q: got %+v %v, want a cause mentioning %q", line, cause, evidence, want)
		}
	}
}

// With --trust-remote-code on, vLLM's start-up config line contains "trust_remote_code=True"; that must not hide the
// real cause further down.
func TestExplainLogIgnoresTrustRemoteCodeConfigLine(t *testing.T) {
	cause, _ := ExplainLog([]string{
		"(EngineCore pid=7) INFO 11-02 08:12:01 [core.py:80] Initializing a V1 LLM engine with config: model='x', trust_remote_code=True",
		"(EngineCore pid=7) ERROR 11-02 08:12:09 [core.py:1330] torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB.",
	})
	if cause == nil || !strings.Contains(cause.Meaning, "GPU memory is too small") {
		t.Errorf("cause = %+v", cause)
	}
}

func TestListPrefersErrorsOverStarting(t *testing.T) {
	findings := []Finding{
		{Severity: SeverityInfo, Title: "Starting"},
		{Severity: SeverityError, Title: "Out of memory"},
	}
	if got := firstProblem(findings); got != "Out of memory" {
		t.Errorf("firstProblem = %q", got)
	}
	if got := firstProblem(findings[:1]); got != "Starting" {
		t.Errorf("firstProblem = %q", got)
	}
}

// A running transformer must not hide the stopped pods of a predictor that has nothing else left.
func TestDiagnoseStoppedPodsPerComponent(t *testing.T) {
	s := loadFixture(t, "oom").snapshot(t)
	transformer := *s.Pods[0].DeepCopy()
	transformer.Name = "oom-transformer"
	transformer.Labels = map[string]string{"component": "transformer"}
	transformer.Status = corev1.PodStatus{Phase: corev1.PodRunning}
	s.Pods[0].Status.Phase = corev1.PodFailed
	s.Pods = append(s.Pods, transformer)
	found := false
	for _, f := range errorsOnly(Diagnose(s)) {
		if f.Title == "Out of memory" {
			found = true
		}
	}
	if !found {
		t.Error("the predictor's stopped pod is the only evidence and must be diagnosed")
	}
}
