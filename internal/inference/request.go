package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAPITimeout bounds the /openapi.json look-up, which is only a safety check.
const openAPITimeout = 3 * time.Second

// Request is one smoke-test request.
type Request struct {
	Endpoint *Endpoint
	Protocol Choice
	Model    string
	Body     []byte
	Headers  http.Header
	Timeout  time.Duration
	// Stream reads an OpenAI server-sent event stream and measures the time to the first token.
	Stream bool
}

// Result is what came back.
type Result struct {
	Route      string        `json:"route"`
	Protocol   Protocol      `json:"protocol"`
	ProtocolBy string        `json:"protocolChosenBy"`
	Method     string        `json:"method"`
	URL        string        `json:"url"`
	ConnectTo  string        `json:"connectTo,omitempty"`
	StatusCode int           `json:"statusCode"`
	Status     string        `json:"status"`
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"durationMs"`
	Body       []byte        `json:"-"`
	// JSONBody is the parsed body for -o json; it is nil when the body is not JSON.
	JSONBody any `json:"body,omitempty"`
	// TextBody holds a body that is not JSON.
	TextBody string `json:"bodyText,omitempty"`
	// Streamed is set when the answer came as a stream; Body then holds the raw events and JSONBody the
	// answer put together from them.
	Streamed bool `json:"streamed,omitempty"`
	// FirstTokenMs is the time until the first chunk with text arrived.
	FirstTokenMs *int64 `json:"firstTokenMs,omitempty"`
	// TokensPerSecond is the generation rate after the first token: completion tokens other than the first,
	// divided by the time between the first token and the end of the stream.
	TokensPerSecond *float64 `json:"tokensPerSecond,omitempty"`
	// TokensCounted is set when the server sent no usage, so the tokens were counted as the chunks with text.
	TokensCounted bool `json:"tokensCountedFromChunks,omitempty"`
	// StreamError is an error event in the stream, or a stream that ended early; the request then failed even
	// though the status was 200.
	StreamError string `json:"streamError,omitempty"`

	streamed *answer
}

// OK reports a 2xx response, and for a stream one that completed without an error event.
func (r *Result) OK() bool { return r.StatusCode >= 200 && r.StatusCode < 300 && r.StreamError == "" }

// CheckPath looks the request path up in the server's /openapi.json, if it publishes one, so a request that is
// bound to fail is never sent. A server without the document (or an unreadable one) skips the check.
func CheckPath(ctx context.Context, ep *Endpoint, c Choice) error {
	ctx, cancel := context.WithTimeout(ctx, openAPITimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodGet, ep, "/openapi.json", nil)
	if err != nil {
		return nil
	}
	resp, err := ep.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil || len(doc.Paths) == 0 {
		return nil
	}
	if _, ok := doc.Paths[c.PathTemplate()]; ok {
		return nil
	}
	// Name what the server does publish, so the user knows which --protocol to use.
	candidates := []struct {
		choice Choice
		flag   string
	}{
		{Choice{Protocol: V1}, "--protocol v1"},
		{Choice{Protocol: V2}, "--protocol v2"},
		{Choice{Protocol: OpenAI, Endpoint: c.Endpoint, Prefix: PrefixHuggingFace}, "--protocol openai (Hugging Face runtime)"},
		{Choice{Protocol: OpenAI, Endpoint: c.Endpoint}, "an OpenAI request at /v1 (vLLM)"},
	}
	var known []string
	for _, k := range candidates {
		if _, ok := doc.Paths[k.choice.PathTemplate()]; ok {
			known = append(known, k.choice.PathTemplate()+", "+k.flag)
		}
	}
	hint := "it publishes none of the v1, v2 or OpenAI inference paths; it may use its own API (see its /openapi.json)"
	if len(known) > 0 {
		hint = "it publishes " + strings.Join(known, "; ")
	}
	return fmt.Errorf("the server's /openapi.json has no %s path %s: %s", c.Protocol, c.PathTemplate(), hint)
}

// Send performs the request.
func Send(ctx context.Context, r Request) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	path := r.Protocol.Path(r.Model)
	req, err := newRequest(ctx, http.MethodPost, r.Endpoint, path, r.Body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range r.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	start := time.Now()
	resp, err := r.Endpoint.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", req.URL, err)
	}
	defer resp.Body.Close()
	// An error answer to a streaming request is plain JSON; only an event stream is read chunk by chunk.
	var st *stream
	if r.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		st = &stream{}
	}
	var body []byte
	if st != nil {
		body, err = st.read(resp.Body, start)
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	elapsed := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}

	res := &Result{
		Route:      r.Endpoint.Route,
		Protocol:   r.Protocol.Protocol,
		ProtocolBy: r.Protocol.Because,
		Method:     http.MethodPost,
		URL:        req.URL.String(),
		ConnectTo:  r.Endpoint.ConnectTo,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Duration:   elapsed,
		DurationMs: elapsed.Milliseconds(),
		Body:       body,
	}
	if st != nil {
		st.finish(res, elapsed)
		return res, nil
	}
	var parsed any
	if json.Unmarshal(body, &parsed) == nil {
		res.JSONBody = parsed
	} else {
		res.TextBody = string(body)
	}
	return res, nil
}

func newRequest(ctx context.Context, method string, ep *Endpoint, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return http.NewRequestWithContext(ctx, method, ep.BaseURL+path, reader)
}

// ParseHeaders turns repeated "Name: value" flags into a header set.
func ParseHeaders(values []string) (http.Header, error) {
	h := http.Header{}
	for _, v := range values {
		name, value, ok := strings.Cut(v, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("header %q must look like \"Name: value\"", v)
		}
		h.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return h, nil
}

// FirstModel asks an OpenAI-compatible server for its models and returns the first id; used when neither the input
// nor the resource names the model.
func FirstModel(ctx context.Context, ep *Endpoint, prefix string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, openAPITimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodGet, ep, prefix+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := ep.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("listing models: %w", err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil || len(list.Data) == 0 {
		return "", fmt.Errorf("listing models at %s/v1/models: no model id in the answer", prefix)
	}
	return list.Data[0].ID, nil
}
