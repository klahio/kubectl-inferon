package status

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"
)

// ScanLines is how many lines of a crashed container's log are searched for a root cause. vLLM prints its traceback
// twice and much start-up output after the cause: on the lab cluster the cause was 490 lines before the end.
const ScanLines = 2000

// LogCause is a known start-up failure that can be recognized in a log (docs/mvp.md section 6a.2).
type LogCause struct {
	// Any of these substrings (compared case-insensitively) identifies the cause.
	Patterns []string
	Meaning  string
	Hint     string
}

// knownCauses is the set from the specification. Cases reproduced on the lab cluster have a fixture; the others
// match messages from the vLLM, PyTorch and transformers sources (docs/mvp.md section 6a.2).
var knownCauses = []LogCause{
	{
		Patterns: []string{"cannot find -l:libcuda.so.1"},
		Meaning:  "Triton cannot find the NVIDIA driver library libcuda.so.1",
		Hint:     "set TRITON_LIBCUDA_PATH to the node's driver directory: /usr/lib/x86_64-linux-gnu on Ubuntu or Debian nodes, /usr/lib64 on RHEL-family nodes",
	},
	{
		// vLLM checks that one request of the full context fits in the KV cache; the wording changed across releases.
		Patterns: []string{"larger than the maximum number of tokens that can be stored in kv cache", "larger than the available kv cache memory"},
		Meaning:  "the KV cache is too small for one request of the full context length (--max-model-len)",
		Hint:     "lower --max-model-len, or raise --gpu-memory-utilization",
	},
	{
		// Typical on time-sliced or shared GPUs, where other processes hold part of the memory.
		Patterns: []string{"less than desired gpu memory utilization"},
		Meaning:  "other processes already use the GPU memory that --gpu-memory-utilization asks for",
		Hint:     "lower --gpu-memory-utilization; a time-sliced GPU shares its memory with every pod on it",
	},
	{
		Patterns: []string{"cuda out of memory", "no available memory for the cache blocks", "torch.outofmemoryerror"},
		Meaning:  "GPU memory is too small for the model and its KV cache",
		Hint:     "lower --max-model-len, raise --gpu-memory-utilization, or use a larger GPU",
	},
	{
		Patterns: []string{"no cuda gpus are available", "failed to infer device type"},
		Meaning:  "no GPU is visible in the container",
		Hint:     "check the GPU request (nvidia.com/gpu) and, where the cluster needs one, runtimeClassName",
	},
	{
		// vLLM 0.26 on the lab cluster: "World size (2) is larger than the number of available GPUs (1) in this node".
		Patterns: []string{"is larger than the number of available gpus", "cannot be larger than the number of available gpus", "exceeds the total number of available gpus"},
		Meaning:  "the tensor or pipeline parallel size needs more GPUs than the pod has",
		Hint:     "request as many GPUs as tensor × pipeline parallel size (spec.parallelism in an LLMInferenceService), or lower the parallel size",
	},
	{
		Patterns: []string{"error while creating shared memory segment", "unable to write to file </torch_"},
		Meaning:  "the container's shared memory (/dev/shm) is too small",
		Hint:     "mount an emptyDir with medium: Memory at /dev/shm",
	},
	{
		Patterns: []string{"] are not supported for now"},
		Meaning:  "this vLLM version does not support the model's architecture",
		Hint:     "use a newer vLLM image, or a model with a supported architecture",
	},
	{
		// The error wording only: with the flag on, vLLM's start-up config line also contains "trust_remote_code=True".
		Patterns: []string{"pass the argument `trust_remote_code=true`"},
		Meaning:  "the model needs custom code from its repository",
		Hint:     "add --trust-remote-code to the vLLM arguments if you trust the model repository",
	},
	{
		Patterns: []string{"gated repo", "401 client error", "access to model"},
		Meaning:  "Hugging Face refuses the model download",
		Hint:     "provide a Hugging Face token Secret for gated or private models",
	},
}

// maxErrorLines bounds how many error lines are shown when no known cause matches.
const maxErrorLines = 3

// ExplainLog returns the first known cause found in the lines, with the line that matched. When nothing known
// matches, it returns the first lines that look like errors, which are usually closer to the cause than the last
// line ("Engine core initialization failed").
func ExplainLog(lines []string) (*LogCause, []string) {
	for _, l := range lines {
		lower := strings.ToLower(l)
		for i := range knownCauses {
			for _, p := range knownCauses[i].Patterns {
				if strings.Contains(lower, p) {
					return &knownCauses[i], []string{strings.TrimSpace(l)}
				}
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		msg := stripPrefix(t)
		lower := strings.ToLower(msg)
		// Skip traceback noise: Python frames, the indented code lines and the ^^^ markers under them.
		if !looksLikeError(lower) || strings.Contains(lower, `file "`) || isTracebackLine(msg) {
			continue
		}
		key := msg
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == maxErrorLines {
			break
		}
	}
	return nil, out
}

func isTracebackLine(msg string) bool {
	if msg == "" || msg[0] == ' ' || msg[0] == '\t' {
		return true
	}
	return strings.Trim(msg, "^~ ") == ""
}

func looksLikeError(lower string) bool {
	for _, w := range []string{"error:", "error ", "exception", "fatal", "failed"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// stripPrefix drops the prefixes vLLM puts on lines, "(EngineCore pid=7) ERROR 10-29 23:48:18 [core.py:1330] ",
// keeping the indentation of what follows, so repeated messages are recognized and code lines stay indented.
func stripPrefix(line string) string {
	if strings.HasPrefix(line, "(") {
		if i := strings.Index(line, ") "); i > 0 {
			line = line[i+2:]
		}
	}
	if i := strings.Index(line, "] "); i >= 0 && strings.Contains(line[:i], "[") {
		return line[i+2:]
	}
	return line
}

func since(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return duration.HumanDuration(time.Since(t))
}
