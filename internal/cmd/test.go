package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/klahio/kubectl-inferon/internal/inference"
	"github.com/klahio/kubectl-inferon/internal/status"
)

// openAITimeout is the default request timeout for LLMs: the first request to a freshly started vLLM server took
// 23 seconds on the lab cluster, the next ones milliseconds.
const openAITimeout = 120 * time.Second

type testOptions struct {
	*rootOptions
	timeoutSet  bool
	kind        Kind
	filename    string
	protocol    string
	model       string
	url         string
	portForward string
	headers     []string
	timeout     time.Duration
	output      string
	prompt      string
	maxTokens   int
	stream      bool
}

func newTestCommand(root *rootOptions) *cobra.Command {
	o := &testOptions{rootOptions: root}
	c := &cobra.Command{
		Use:   "test (isvc|llmisvc)/NAME (-f INPUT.json | --prompt TEXT)",
		Short: "Send one smoke-test inference request, finding the address and protocol automatically",
		Example: fmt.Sprintf(`  %[1]s%[2]s test isvc/sklearn-iris -f input.json
  %[1]s%[2]s test isvc/sklearn-iris -f input.json -o summary
  cat input.json | %[1]s%[2]s test isvc/sklearn-iris -f - -o raw | jq .predictions
  %[1]s%[2]s test llmisvc/smollm --prompt "What is Kubernetes?"
  %[1]s%[2]s test llmisvc/smollm --prompt "Hi" --stream         # time to first token and tokens per second
  %[1]s%[2]s test llmisvc/smollm -f chat.json -o summary          # OpenAI body with "messages"`, commandPrefix(), PluginName),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := resourceArg(args[0])
			if err != nil {
				return err
			}
			o.kind = kind
			o.timeoutSet = cmd.Flags().Changed("timeout")
			return o.run(cmd.Context(), name)
		},
	}
	f := c.Flags()
	f.StringVarP(&o.filename, "filename", "f", "", "Request body file; - reads standard input")
	f.StringVar(&o.prompt, "prompt", "", "Send an OpenAI chat request with this one user message instead of -f")
	f.IntVar(&o.maxTokens, "max-tokens", 64, "max_tokens for --prompt")
	f.BoolVar(&o.stream, "stream", false, "Stream the OpenAI answer and show the time to the first token and the tokens per second")
	f.StringVar(&o.protocol, "protocol", "", "v1, v2 or openai (default: automatic)")
	f.StringVar(&o.model, "model", "", "Model name (default: the InferenceService name; for an LLMInferenceService spec.model.name)")
	f.StringVar(&o.url, "url", "", "Send to this base address instead of choosing a route")
	f.StringVar(&o.portForward, "port-forward", inference.PortForwardAuto, "auto, always or never")
	f.StringArrayVarP(&o.headers, "header", "H", nil, `Extra request header "Name: value", repeatable`)
	f.DurationVar(&o.timeout, "timeout", 30*time.Second, "Request timeout (default 2m for OpenAI requests, because a freshly started model server can be slow to answer the first one)")
	f.StringVarP(&o.output, "output", "o", inference.OutputPretty, "Output: "+strings.Join(inference.Outputs, ", "))
	c.MarkFlagsOneRequired("filename", "prompt")
	c.MarkFlagsMutuallyExclusive("filename", "prompt")
	return c
}

func usage(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

func (o *testOptions) run(ctx context.Context, name string) error {
	if !slices.Contains(inference.Outputs, o.output) {
		return usage("unknown output format %q (use %s)", o.output, strings.Join(inference.Outputs, ", "))
	}
	switch o.portForward {
	case inference.PortForwardAuto, inference.PortForwardAlways, inference.PortForwardNever:
	default:
		return usage("--port-forward must be auto, always or never, not %q", o.portForward)
	}
	if o.url != "" && o.portForward == inference.PortForwardAlways {
		return usage("--url and --port-forward=always cannot be used together")
	}
	headers, err := inference.ParseHeaders(o.headers)
	if err != nil {
		return usage("%v", err)
	}
	var body []byte
	if o.prompt == "" {
		if body, err = o.readInput(); err != nil {
			return usage("%v", err)
		}
	} else {
		if o.maxTokens < 1 {
			return usage("--max-tokens must be at least 1")
		}
		body = []byte(`{"messages":[]}`) // shape only; PrepareOpenAI builds the real body
	}

	if ctx == nil {
		ctx = context.Background()
	}
	clients, namespace, err := o.clients()
	if err != nil {
		return err
	}

	var (
		target inference.Target
		choice inference.Choice
		model  = o.model
		ready  string
		arg    = "isvc/" + name
	)
	switch o.kind {
	case KindLLMInferenceService:
		snap, err := status.CollectLLM(ctx, clients, namespace, name, false)
		if err != nil {
			return clusterError(err)
		}
		summary := status.SummarizeLLM(snap, status.DiagnoseLLM(snap))
		target, ready, arg = inference.LLMInferenceServiceTarget(clients, summary, snap), summary.Ready, "llmisvc/"+name
		if choice, err = inference.ChooseLLMProtocol(o.protocol, body); err != nil {
			return usage("%v", err)
		}
		if model == "" {
			model = snap.Service.ModelName
		}
	default:
		snap, err := status.Collect(ctx, clients, namespace, name, false)
		if err != nil {
			return clusterError(err)
		}
		summary := status.Summarize(snap, status.Diagnose(snap))
		target, ready = inference.InferenceServiceTarget(clients, summary, snap), summary.Ready
		if choice, err = inference.ChooseProtocol(o.protocol, snap.ISVC, snap.Runtime, body); err != nil {
			return usage("%v", err)
		}
		if model == "" {
			// KServe's built-in runtimes, the Hugging Face one included, register the model under the resource name.
			model = name
		}
	}
	if ready != "True" {
		fmt.Fprintf(o.streams.ErrOut, "warning: %s/%s is not Ready; `%s%s status %s` shows why\n", namespace, name, commandPrefix(), PluginName, arg)
	}

	if o.stream && choice.Protocol != inference.OpenAI {
		return usage("--stream works only with OpenAI requests (--prompt, or an input with \"messages\" or \"prompt\"); this request uses protocol %s", choice.Protocol)
	}
	ep, err := inference.Discover(ctx, target, inference.DiscoverOptions{URL: o.url, PortForward: o.portForward})
	if err != nil {
		return discoverError(err)
	}
	defer ep.Close()

	if err := inference.CheckPath(ctx, ep, choice); err != nil {
		return &ExitError{Code: ExitUsage, Err: err}
	}
	if choice.Protocol == inference.OpenAI {
		if model == "" && inference.ModelOf(body) == "" {
			if model, err = inference.FirstModel(ctx, ep, choice.Prefix); err != nil {
				return &ExitError{Code: ExitFailure, Err: err}
			}
		}
		if body, o.stream, err = inference.PrepareOpenAI(body, o.prompt, model, o.maxTokens, o.stream); err != nil {
			return usage("%v", err)
		}
		if !o.timeoutSet {
			o.timeout = openAITimeout
		}
	}
	res, err := inference.Send(ctx, inference.Request{
		Endpoint: ep,
		Protocol: choice,
		Model:    model,
		Body:     body,
		Headers:  headers,
		Timeout:  o.timeout,
		Stream:   o.stream,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w\nno answer within %s; the first request to a freshly started model server can take much longer than the next ones, so retry or raise --timeout", err, o.timeout)
		}
		return &ExitError{Code: ExitFailure, Err: err}
	}
	if err := inference.Write(o.streams.Out, o.output, res); err != nil {
		return err
	}
	if !res.OK() {
		return &ExitError{Code: ExitFailure}
	}
	return nil
}

// discoverError maps a route failure to the exit codes of docs/mvp.md section 3: a bad --url is wrong usage, and
// an API server refusal (listing Services or pods, or creating pods/portforward) is a cluster or permission error.
func discoverError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &ExitError{Code: ExitUsage, Err: err}
	}
	var apiErr apierrors.APIStatus
	// The port-forward upgrade reports a refusal as text, not as an API status.
	if errors.As(err, &apiErr) || strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		return &ExitError{Code: ExitClusterOrNF, Err: err}
	}
	return &ExitError{Code: ExitFailure, Err: err}
}

func (o *testOptions) readInput() ([]byte, error) {
	var (
		data []byte
		err  error
	)
	if o.filename == "-" {
		data, err = io.ReadAll(o.streams.In)
	} else {
		data, err = os.ReadFile(o.filename)
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("the input is not valid JSON")
	}
	return data, nil
}
