package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/status"
)

const (
	v1Body = `{"instances":[[6.8,2.8,4.8,1.4]]}`
	v2Body = `{"inputs":[{"name":"input-0","shape":[1,4],"datatype":"FP32","data":[[6.8,2.8,4.8,1.4]]}]}`
)

func TestChooseProtocol(t *testing.T) {
	sklearn := &status.Runtime{Name: "kserve-sklearnserver", Found: true, ProtocolVersions: []string{"v1", "v2"}}
	triton := &status.Runtime{Name: "kserve-tritonserver", Found: true, ProtocolVersions: []string{"v2", "grpc-v2"}}
	tfServing := &status.Runtime{Name: "kserve-tensorflow-serving", Found: true, ProtocolVersions: []string{"grpc-v1", "v1"}}

	tests := []struct {
		name     string
		flag     string
		spec     string
		runtime  *status.Runtime
		body     string
		want     Protocol
		because  string
		errorHas string
	}{
		{name: "flag wins", flag: "v2", runtime: sklearn, body: `{}`, want: V2, because: "--protocol"},
		{name: "spec protocolVersion", spec: "v2", runtime: sklearn, body: `{}`, want: V2, because: "spec.predictor.model.protocolVersion"},
		{name: "input shape v1", runtime: sklearn, body: v1Body, want: V1, because: `input has "instances"`},
		{name: "input shape v2", runtime: sklearn, body: v2Body, want: V2, because: `input has "inputs"`},
		{name: "runtime's first protocol", runtime: triton, body: `{}`, want: V2, because: "first protocol of runtime kserve-tritonserver"},
		{name: "gRPC entries are skipped", runtime: tfServing, body: `{}`, want: V1, because: "first protocol of runtime kserve-tensorflow-serving"},
		{name: "default", body: `{}`, want: V1, because: "default"},
		{name: "flag contradicts input", flag: "v2", runtime: sklearn, body: v1Body, errorHas: "does not match the input"},
		{name: "runtime does not support it", runtime: triton, body: v1Body, errorHas: "supports v2, grpc-v2, not v1"},
		{name: "gRPC flag", flag: "grpc-v2", body: `{}`, errorHas: "HTTP requests only"},
		{name: "OpenAI chat body", runtime: sklearn, body: `{"messages":[]}`, want: OpenAI, because: `input has "messages"`},
		{name: "OpenAI completion body", body: `{"prompt":"x"}`, want: OpenAI, because: `input has "prompt"`},
		{name: "not an object", body: `[1,2]`, errorHas: "not a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &isvc.InferenceService{Model: isvc.Model{ProtocolVersion: tt.spec}}
			got, err := ChooseProtocol(tt.flag, m, tt.runtime, []byte(tt.body))
			if tt.errorHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errorHas) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.errorHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Protocol != tt.want || got.Because != tt.because {
				t.Errorf("got %+v, want %s because %q", got, tt.want, tt.because)
			}
		})
	}
}

// server is a fake model server: it publishes /openapi.json with the given paths and answers inference requests.
func server(t *testing.T, paths ...string) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	doc := map[string]any{"paths": map[string]any{}}
	for _, p := range paths {
		doc["paths"].(map[string]any)[p] = map[string]any{}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		last = *r.Clone(context.Background())
		body, _ := io.ReadAll(r.Body)
		last.Body = io.NopCloser(bytes.NewReader(body))
		if r.URL.Path != "/v1/models/sklearn-iris:predict" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"Model with name nope does not exist."}`)
			return
		}
		_, _ = io.WriteString(w, `{"predictions":[1]}`)
	}))
	t.Cleanup(s.Close)
	return s, &last
}

func TestCheckPath(t *testing.T) {
	s, _ := server(t, Choice{Protocol: V1}.PathTemplate())
	ep := &Endpoint{BaseURL: s.URL}
	if err := CheckPath(context.Background(), ep, Choice{Protocol: V1}); err != nil {
		t.Errorf("v1 is published: %v", err)
	}
	err := CheckPath(context.Background(), ep, Choice{Protocol: V2})
	if err == nil || !strings.Contains(err.Error(), "--protocol v1") {
		t.Errorf("err = %v, want a hint to use v1", err)
	}

	// No document at all: the check is skipped.
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	if err := CheckPath(context.Background(), &Endpoint{BaseURL: plain.URL}, Choice{Protocol: V2}); err != nil {
		t.Errorf("without /openapi.json the check must be skipped: %v", err)
	}
}

func TestSendAndOutputs(t *testing.T) {
	s, last := server(t, Choice{Protocol: V1}.PathTemplate())
	headers, err := ParseHeaders([]string{"Authorization: Bearer test-token"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Send(context.Background(), Request{
		Endpoint: &Endpoint{BaseURL: "http://sklearn-iris.kserve-test.example.invalid", ConnectTo: strings.TrimPrefix(s.URL, "http://"), Route: "Ingress sklearn-iris at 127.0.0.1:80"},
		Protocol: Choice{Protocol: V1, Because: `input has "instances"`},
		Model:    "sklearn-iris",
		Body:     []byte(v1Body),
		Headers:  headers,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || last.Host != "sklearn-iris.kserve-test.example.invalid" || last.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("status=%d host=%q auth=%q", res.StatusCode, last.Host, last.Header.Get("Authorization"))
	}
	if ct := last.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var out bytes.Buffer
	if err := Write(&out, OutputSummary, res); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "HTTP 200 · ") || !strings.HasSuffix(got, " · via Ingress sklearn-iris · predictions: [1]\n") {
		t.Errorf("summary = %q", got)
	}
	out.Reset()
	if err := Write(&out, OutputRaw, res); err != nil || out.String() != `{"predictions":[1]}` {
		t.Errorf("raw = %q, %v", out.String(), err)
	}
	out.Reset()
	if err := Write(&out, OutputJSON, res); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc["statusCode"] != float64(200) || doc["protocol"] != "v1" {
		t.Errorf("json = %s, %v", out.String(), err)
	}
	out.Reset()
	if err := Write(&out, OutputPretty, res); err != nil || !strings.Contains(out.String(), "  connected to "+res.ConnectTo+"\n") {
		t.Errorf("pretty = %q, %v", out.String(), err)
	}
}

func TestSummarizeV2AndErrors(t *testing.T) {
	v2 := &Result{JSONBody: map[string]any{"outputs": []any{map[string]any{"name": "output-0", "data": []any{1.0}}}}}
	if got := summarize(v2); got != "outputs: output-0=[1]" {
		t.Errorf("v2 = %q", got)
	}
	failure := &Result{JSONBody: map[string]any{"error": "Model with name nope does not exist."}}
	if got := summarize(failure); got != `error: "Model with name nope does not exist."` {
		t.Errorf("error = %q", got)
	}
}

// When the external name does not resolve from this machine, the Ingress or Gateway address still reaches the model
// without port-forward: the URL keeps the host name and only the TCP connection goes to the address.
func TestDiscoverHostHeaderRoute(t *testing.T) {
	s, _ := server(t, Choice{Protocol: V1}.PathTemplate())
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))

	sum := &status.Summary{Endpoints: status.Endpoints{
		External: "http://sklearn-iris-kserve-test.example.invalid:" + port,
		Routable: false,
		Exposure: []status.Exposure{{Kind: "HTTPRoute", Name: "sklearn-iris", Via: "Gateway gateway/lab-gateway", Address: "127.0.0.1"}},
	}}
	ep, err := Discover(context.Background(), Target{External: sum.Endpoints.External, Routable: sum.Endpoints.Routable, Exposure: sum.Endpoints.Exposure}, DiscoverOptions{PortForward: PortForwardNever})
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL != "http://sklearn-iris-kserve-test.example.invalid:"+port || ep.ConnectTo != "127.0.0.1:"+port {
		t.Errorf("endpoint = %+v", ep)
	}
	// The request reaches the server through ConnectTo even though the host name does not resolve.
	res, err := Send(context.Background(), Request{Endpoint: ep, Protocol: Choice{Protocol: V1, Because: "test"}, Model: "sklearn-iris", Body: []byte(v1Body), Timeout: 5 * time.Second})
	if err != nil || !res.OK() {
		t.Fatalf("send through ConnectTo: %v %+v", err, res)
	}
	if !strings.HasPrefix(ep.Route, "HTTPRoute sklearn-iris at 127.0.0.1") {
		t.Errorf("route = %q", ep.Route)
	}
}

// A route that accepts TCP but does not reach the model (a proxy answering 503), a route the diagnosis found broken,
// and a host name nothing in the cluster vouches for all lead to port-forward.
func TestDiscoverFallsBackToPortForward(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	good, _ := server(t, Choice{Protocol: V1}.PathTemplate())
	_, proxyPort, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	_, goodPort, _ := net.SplitHostPort(strings.TrimPrefix(good.URL, "http://"))

	forwarded := &Endpoint{Route: "port-forward"}
	portForward := func(context.Context) (*Endpoint, error) { return forwarded, nil }
	tests := []struct {
		name   string
		target Target
	}{
		{"proxy answers 503", Target{
			External: "http://m.example.invalid:" + proxyPort,
			Exposure: []status.Exposure{{Kind: "HTTPRoute", Name: "m", Address: "127.0.0.1"}},
		}},
		{"broken route", Target{
			External: "http://m.example.invalid:" + goodPort,
			Exposure: []status.Exposure{{Kind: "HTTPRoute", Name: "m", Address: "127.0.0.1", Broken: true}},
		}},
		{"exposure unknown", Target{External: good.URL, Routable: routable(status.Endpoints{External: good.URL, Routable: true})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.target.PortForward = portForward
			ep, err := Discover(context.Background(), tt.target, DiscoverOptions{PortForward: PortForwardAuto})
			if err != nil || ep != forwarded {
				t.Errorf("endpoint = %+v, %v; want port-forward", ep, err)
			}
		})
	}
}

func TestDiscoverURLFlag(t *testing.T) {
	ep, err := Discover(context.Background(), Target{}, DiscoverOptions{URL: "http://localhost:8080/"})
	if err != nil || ep.BaseURL != "http://localhost:8080" || ep.Route != "--url" {
		t.Errorf("endpoint = %+v, %v", ep, err)
	}
}

func TestTargetPrefersTransformer(t *testing.T) {
	m := &isvc.InferenceService{Components: []isvc.Component{{Name: "predictor"}, {Name: "transformer"}}}
	if TargetComponent(m) != "transformer" {
		t.Error("requests must go through the transformer")
	}
	if TargetComponent(&isvc.InferenceService{Components: []isvc.Component{{Name: "predictor"}}}) != "predictor" {
		t.Error("without a transformer the predictor receives the request")
	}
}

func TestContainerPort(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "kserve-container", Ports: []corev1.ContainerPort{{Name: "h2c", ContainerPort: 8080}},
	}}}}
	if p, err := containerPort(pod, intstr.FromInt32(8080)); err != nil || p != 8080 {
		t.Errorf("number: %d, %v", p, err)
	}
	if p, err := containerPort(pod, intstr.FromString("h2c")); err != nil || p != 8080 {
		t.Errorf("name: %d, %v", p, err)
	}
	if _, err := containerPort(pod, intstr.FromString("missing")); err == nil {
		t.Error("unknown port name must fail")
	}
}

func TestParseHeaders(t *testing.T) {
	if _, err := ParseHeaders([]string{"no-colon"}); err == nil {
		t.Error("a header without a colon must be rejected")
	}
	h, err := ParseHeaders([]string{"X-A: 1", "X-A: 2"})
	if err != nil || len(h.Values("X-A")) != 2 {
		t.Errorf("repeated headers: %v, %v", h, err)
	}
}

func TestChooseProtocolOpenAIPaths(t *testing.T) {
	chat, err := ChooseProtocol("", &isvc.InferenceService{}, nil, []byte(`{"messages":[]}`))
	if err != nil || chat.PathTemplate() != "/openai/v1/chat/completions" {
		t.Errorf("InferenceService chat: %q, %v", chat.PathTemplate(), err)
	}
	compl, err := ChooseLLMProtocol("", []byte(`{"prompt":"x"}`))
	if err != nil || compl.PathTemplate() != "/v1/completions" {
		t.Errorf("LLMInferenceService completions: %q, %v", compl.PathTemplate(), err)
	}
	if _, err := ChooseLLMProtocol("", []byte(v1Body)); err == nil || !strings.Contains(err.Error(), `"instances"`) {
		t.Errorf("a v1 body must be refused: %v", err)
	}
	if _, err := ChooseLLMProtocol("v2", []byte(`{}`)); err == nil {
		t.Error("--protocol v2 must be refused for an LLMInferenceService")
	}
}

func TestPrepareOpenAI(t *testing.T) {
	body, err := PrepareOpenAI(nil, "What is Kubernetes?", "m", 64)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Model != "m" || req.MaxTokens != 64 || len(req.Messages) != 1 || req.Messages[0].Content != "What is Kubernetes?" {
		t.Errorf("prompt body = %s, %v", body, err)
	}
	kept, err := PrepareOpenAI([]byte(`{"model":"mine","prompt":"x"}`), "", "other", 64)
	if err != nil || ModelOf(kept) != "mine" {
		t.Errorf("an explicit model must be kept: %s, %v", kept, err)
	}
	if _, err := PrepareOpenAI([]byte(`{"messages":[],"stream":true}`), "", "m", 64); err == nil {
		t.Error("streaming must be refused in version 0.1")
	}
	// null passes json.Unmarshal into a map and used to panic when the model was filled in.
	if _, err := PrepareOpenAI([]byte(`null`), "", "m", 64); err == nil {
		t.Error("a null body must be refused")
	}
	if _, err := shapeOf([]byte(`null`)); err == nil {
		t.Error("shapeOf must refuse a null body")
	}
}

func TestOpenAIOutputs(t *testing.T) {
	res := &Result{Protocol: OpenAI, StatusCode: 200, Status: "200 OK", Route: "port-forward to svc/x", DurationMs: 73,
		Body: []byte(`{"model":"HuggingFaceTB/SmolLM2-135M-Instruct","choices":[{"message":{"content":" Kubernetes is\n a platform."},"finish_reason":"stop"}],"usage":{"prompt_tokens":41,"completion_tokens":26}}`)}
	if got := summarize(res); got != `"Kubernetes is a platform." · 41+26 tokens` {
		t.Errorf("summary = %q", got)
	}
	var out bytes.Buffer
	if err := Write(&out, OutputPretty, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Kubernetes is\n a platform.\n\nmodel HuggingFaceTB/SmolLM2-135M-Instruct · finish stop · tokens 41 prompt + 26 completion") {
		t.Errorf("pretty = %q", out.String())
	}
	completion := &Result{Protocol: OpenAI, Body: []byte(`{"choices":[{"text":"Paris."}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)}
	if got := summarize(completion); got != `"Paris." · 5+2 tokens` {
		t.Errorf("completion summary = %q", got)
	}
}

// A vLLM server (LLMInferenceService) publishes the OpenAI paths at /v1; asking for the Hugging Face layout names it.
func TestCheckPathOpenAILayouts(t *testing.T) {
	s, _ := server(t, "/v1/chat/completions", "/v1/models")
	ep := &Endpoint{BaseURL: s.URL}
	if err := CheckPath(context.Background(), ep, Choice{Protocol: OpenAI, Endpoint: EndpointChat}); err != nil {
		t.Errorf("vLLM layout is published: %v", err)
	}
	err := CheckPath(context.Background(), ep, Choice{Protocol: OpenAI, Endpoint: EndpointChat, Prefix: PrefixHuggingFace})
	if err == nil || !strings.Contains(err.Error(), "/v1/chat/completions, an OpenAI request at /v1 (vLLM)") {
		t.Errorf("err = %v", err)
	}
}

func TestFirstModel(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"HuggingFaceTB/SmolLM2-135M-Instruct"}]}`)
	}))
	t.Cleanup(s.Close)
	got, err := FirstModel(context.Background(), &Endpoint{BaseURL: s.URL}, "")
	if err != nil || got != "HuggingFaceTB/SmolLM2-135M-Instruct" {
		t.Errorf("got %q, %v", got, err)
	}
}
