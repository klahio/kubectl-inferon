// Package inference sends a smoke-test inference request to an InferenceService or LLMInferenceService
// (docs/mvp.md sections 5 and 6a.3).
package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/klahio/kubectl-inferon/internal/isvc"
	"github.com/klahio/kubectl-inferon/internal/status"
)

// Protocol is an HTTP inference protocol.
type Protocol string

const (
	V1     Protocol = "v1"
	V2     Protocol = "v2"     // Open Inference Protocol
	OpenAI Protocol = "openai" // OpenAI-compatible chat and completions API
)

// OpenAI endpoints, chosen from the request body.
const (
	EndpointChat        = "chat"
	EndpointCompletions = "completions"
)

// PrefixHuggingFace is where KServe's Hugging Face runtime serves the OpenAI API; vLLM in an LLMInferenceService
// serves it at the root.
const PrefixHuggingFace = "/openai"

// Choice is the selected protocol and the rule that selected it, so the output can explain itself.
type Choice struct {
	Protocol Protocol
	Because  string
	// Endpoint and Prefix apply to the OpenAI protocol only.
	Endpoint string
	Prefix   string
}

// PathTemplate is the request path as it appears in a model server's /openapi.json.
func (c Choice) PathTemplate() string {
	switch c.Protocol {
	case V2:
		return "/v2/models/{model_name}/infer"
	case OpenAI:
		if c.Endpoint == EndpointCompletions {
			return c.Prefix + "/v1/completions"
		}
		return c.Prefix + "/v1/chat/completions"
	default:
		return "/v1/models/{model_name}:predict"
	}
}

// Path is the request path for a model.
func (c Choice) Path(model string) string {
	return strings.Replace(c.PathTemplate(), "{model_name}", model, 1)
}

// shape is what a request body is written for.
type shape struct {
	protocol Protocol
	endpoint string
	key      string
}

// shapeOf reads the top-level keys: "instances" means v1, "inputs" means v2, "messages" means an OpenAI chat
// request and "prompt" an OpenAI completion request.
func shapeOf(body []byte) (shape, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return shape{}, fmt.Errorf("the input is not a JSON object: %w", err)
	}
	if top == nil { // null passes json.Unmarshal
		return shape{}, errors.New("the input is not a JSON object: null")
	}
	var found []shape
	for _, s := range []shape{
		{V1, "", "instances"},
		{V2, "", "inputs"},
		{OpenAI, EndpointChat, "messages"},
		{OpenAI, EndpointCompletions, "prompt"},
	} {
		if _, ok := top[s.key]; ok {
			found = append(found, s)
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return shape{}, nil
}

// ChooseProtocol applies the rules of docs/mvp.md section 5 for an InferenceService, first match wins:
// --protocol, spec.predictor.model.protocolVersion, the input's shape, the runtime's first protocol, v1.
// It fails before any request is sent when the choice contradicts the input or the runtime.
func ChooseProtocol(flag string, m *isvc.InferenceService, rt *status.Runtime, body []byte) (Choice, error) {
	sh, err := shapeOf(body)
	if err != nil {
		return Choice{}, err
	}

	var c Choice
	switch {
	case flag != "":
		p, err := parse(flag)
		if err != nil {
			return Choice{}, err
		}
		c = Choice{Protocol: p, Because: "--protocol"}
	case m.Model.ProtocolVersion != "":
		p, err := parse(m.Model.ProtocolVersion)
		if err != nil {
			return Choice{}, fmt.Errorf("spec.predictor.model.protocolVersion %q: %w", m.Model.ProtocolVersion, err)
		}
		c = Choice{Protocol: p, Because: "spec.predictor.model.protocolVersion"}
	case sh.protocol != "":
		c = Choice{Protocol: sh.protocol, Because: fmt.Sprintf("input has %q", sh.key)}
	case rt != nil && firstHTTP(rt.ProtocolVersions) != "":
		c = Choice{Protocol: firstHTTP(rt.ProtocolVersions), Because: "first protocol of runtime " + rt.Name}
	default:
		c = Choice{Protocol: V1, Because: "default"}
	}

	if sh.protocol != "" && sh.protocol != c.Protocol {
		return Choice{}, fmt.Errorf("protocol %s (from %s) does not match the input, which has %q (a %s request); use --protocol %s or change the input",
			c.Protocol, c.Because, sh.key, sh.protocol, sh.protocol)
	}
	if c.Protocol == OpenAI {
		// Runtimes list v1/v2 in protocolVersions but not the OpenAI API; /openapi.json is the check for it.
		c.Prefix = PrefixHuggingFace
		c.Endpoint = sh.endpoint
		if c.Endpoint == "" {
			c.Endpoint = EndpointChat
		}
		return c, nil
	}
	if rt != nil && rt.Found && len(rt.ProtocolVersions) > 0 && !slices.Contains(rt.ProtocolVersions, string(c.Protocol)) {
		return Choice{}, fmt.Errorf("runtime %s supports %s, not %s", rt.Name, strings.Join(rt.ProtocolVersions, ", "), c.Protocol)
	}
	return c, nil
}

// ChooseLLMProtocol returns the OpenAI protocol for an LLMInferenceService, the only one vLLM serves there.
func ChooseLLMProtocol(flag string, body []byte) (Choice, error) {
	if flag != "" && flag != string(OpenAI) {
		return Choice{}, fmt.Errorf("an LLMInferenceService serves the OpenAI-compatible API only; use --protocol openai or leave it out")
	}
	sh, err := shapeOf(body)
	if err != nil {
		return Choice{}, err
	}
	if sh.protocol != "" && sh.protocol != OpenAI {
		return Choice{}, fmt.Errorf("the input has %q (a %s request), but an LLMInferenceService takes OpenAI requests with \"messages\" or \"prompt\"", sh.key, sh.protocol)
	}
	c := Choice{Protocol: OpenAI, Because: "LLMInferenceService", Endpoint: sh.endpoint}
	if c.Endpoint == "" {
		c.Endpoint = EndpointChat
	}
	return c, nil
}

// PrepareOpenAI builds or completes an OpenAI request body: --prompt becomes a chat request with one user message,
// and a missing "model" is filled in. Streaming is not supported in version 0.1.
func PrepareOpenAI(body []byte, prompt, model string, maxTokens int) ([]byte, error) {
	var req map[string]any
	if prompt != "" {
		req = map[string]any{
			"messages":   []any{map[string]any{"role": "user", "content": prompt}},
			"max_tokens": maxTokens,
		}
	} else if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("the input is not a JSON object: %w", err)
	} else if req == nil {
		return nil, errors.New("the input is not a JSON object: null")
	}
	if stream, _ := req["stream"].(bool); stream {
		return nil, errors.New(`"stream": true is not supported yet; remove it to get one complete response`)
	}
	if _, ok := req["model"]; !ok && model != "" {
		req["model"] = model
	}
	return json.Marshal(req)
}

// ModelOf returns the "model" field of an OpenAI request body.
func ModelOf(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Model
}

func parse(s string) (Protocol, error) {
	switch strings.ToLower(s) {
	case "v1":
		return V1, nil
	case "v2":
		return V2, nil
	case "openai":
		return OpenAI, nil
	case "grpc-v1", "grpc-v2":
		return "", fmt.Errorf("%s is a gRPC protocol; version 0.1 sends HTTP requests only (v1, v2 or openai)", s)
	default:
		return "", fmt.Errorf("unknown protocol %q (use v1, v2 or openai)", s)
	}
}

// firstHTTP returns the first HTTP protocol a runtime lists, skipping gRPC ones.
func firstHTTP(versions []string) Protocol {
	for _, v := range versions {
		if p, err := parse(v); err == nil {
			return p
		}
	}
	return ""
}
