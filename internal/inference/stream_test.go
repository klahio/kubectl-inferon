package inference

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer answers like vLLM: one chunk per token, then a usage chunk and [DONE]. gap delays each token.
func sseServer(t *testing.T, tokens []string, gap time.Duration, withUsage bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		f := w.(http.Flusher)
		for i, tok := range tokens {
			time.Sleep(gap)
			finish := "null"
			if i == len(tokens)-1 {
				finish = `"stop"`
			}
			fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":%s}]}\n\n", tok, finish)
			f.Flush()
		}
		if withUsage {
			fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":%d}}\n\n", len(tokens))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSendStream(t *testing.T) {
	s := sseServer(t, []string{"Hello", "!", " How", " can", " I", " help", "?"}, 20*time.Millisecond, true)
	res, err := Send(context.Background(), Request{
		Endpoint: &Endpoint{BaseURL: s.URL},
		Protocol: Choice{Protocol: OpenAI, Endpoint: EndpointChat},
		Body:     []byte(`{"messages":[],"stream":true}`),
		Timeout:  5 * time.Second,
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Streamed || res.FirstTokenMs == nil || *res.FirstTokenMs < 15 || res.TokensPerSecond == nil || res.TokensCounted {
		t.Fatalf("result = %+v", res)
	}
	// 6 tokens after the first, about 20 ms apart: roughly 50 tokens/s, with slack for a busy machine.
	if rate := *res.TokensPerSecond; rate < 10 || rate > 60 {
		t.Errorf("rate = %.1f tokens/s", rate)
	}
	var buf bytes.Buffer
	if err := Write(&buf, OutputPretty, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Hello! How can I help?", "tokens 7 prompt + 7 completion", "first token ", " tokens/s after it"} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty output lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := Write(&buf, OutputRaw, res); err != nil || !strings.HasPrefix(buf.String(), "data: ") {
		t.Errorf("raw output must be the events as sent: %q, %v", buf.String(), err)
	}
}

// Servers that ignore stream_options send no usage; the tokens are then counted as chunks, and the output says so.
func TestSendStreamWithoutUsage(t *testing.T) {
	s := sseServer(t, []string{"a", "b", "c"}, 5*time.Millisecond, false)
	res, err := Send(context.Background(), Request{
		Endpoint: &Endpoint{BaseURL: s.URL},
		Protocol: Choice{Protocol: OpenAI, Endpoint: EndpointChat},
		Body:     []byte(`{"messages":[],"stream":true}`),
		Timeout:  5 * time.Second,
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, OutputSummary, res); err != nil {
		t.Fatal(err)
	}
	if !res.TokensCounted || !strings.Contains(buf.String(), `"abc" · 0+3 tokens · first token`) || !strings.Contains(buf.String(), "counted as chunks") {
		t.Errorf("summary = %q", buf.String())
	}
}

// An error answer to a streaming request is plain JSON and must be shown as such.
func TestSendStreamErrorAnswer(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"model not found"}}`)
	}))
	defer s.Close()
	res, err := Send(context.Background(), Request{
		Endpoint: &Endpoint{BaseURL: s.URL},
		Protocol: Choice{Protocol: OpenAI, Endpoint: EndpointChat},
		Body:     []byte(`{}`),
		Timeout:  5 * time.Second,
		Stream:   true,
	})
	if err != nil || res.Streamed || res.OK() || res.JSONBody == nil {
		t.Errorf("result = %+v, %v", res, err)
	}
}

// sendRawStream serves body as an event stream, then keeps the connection open for hold.
func sendRawStream(t *testing.T, body string, hold time.Duration) *Result {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(s.Close)
	res, err := Send(context.Background(), Request{
		Endpoint: &Endpoint{BaseURL: s.URL},
		Protocol: Choice{Protocol: OpenAI, Endpoint: EndpointChat},
		Body:     []byte(`{}`),
		Timeout:  2 * time.Second,
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStreamEdgeCases(t *testing.T) {
	chunk := func(index int, text, finish string) string {
		f := "null"
		if finish != "" {
			f = `"` + finish + `"`
		}
		return fmt.Sprintf("data: {\"choices\":[{\"index\":%d,\"delta\":{\"content\":%q},\"finish_reason\":%s}]}\n\n", index, text, f)
	}

	// A server that keeps the connection open after [DONE] must not run into the timeout.
	start := time.Now()
	res := sendRawStream(t, chunk(0, "hi", "stop")+"data: [DONE]\n\n", 10*time.Second)
	if !res.OK() || time.Since(start) > time.Second {
		t.Errorf("[DONE] must end the read: ok=%v after %s", res.OK(), time.Since(start))
	}

	// An error event with status 200 is a failure, and its message is shown.
	res = sendRawStream(t, chunk(0, "par", "")+"data: {\"error\":{\"message\":\"engine died\"}}\n\n", 0)
	if res.OK() || res.StreamError != "engine died" {
		t.Errorf("error event: ok=%v, streamError=%q", res.OK(), res.StreamError)
	}

	// A stream cut off without a finish reason or [DONE] is incomplete.
	res = sendRawStream(t, chunk(0, "half an ans", ""), 0)
	if res.OK() || !strings.Contains(res.StreamError, "before the answer was complete") {
		t.Errorf("cut-off stream: ok=%v, streamError=%q", res.OK(), res.StreamError)
	}

	// One event may spread its data over several lines; with n=2 only the first choice is shown.
	multi := "data: {\"choices\":[{\"index\":0,\n" + "data: \"delta\":{\"content\":\"one\"}}]}\n\n"
	res = sendRawStream(t, multi+chunk(1, "TWO", "")+chunk(0, " two", "stop")+"data: [DONE]\n\n", 0)
	if a, _ := openAIAnswer(res); !res.OK() || a.Text != "one two" {
		t.Errorf("multi-line event and second choice: ok=%v text=%q err=%q", res.OK(), a.Text, res.StreamError)
	}
}

func TestPrepareOpenAIMergesStreamOptions(t *testing.T) {
	body, stream, err := PrepareOpenAI([]byte(`{"messages":[],"stream":true,"stream_options":{"continuous_usage_stats":true}}`), "", "m", 64, false)
	if err != nil || !stream || !strings.Contains(string(body), `"include_usage":true`) || !strings.Contains(string(body), `"continuous_usage_stats":true`) {
		t.Errorf("body = %s, stream = %v, err = %v", body, stream, err)
	}
}

func TestStreamDoneWithoutBlankLineAndReasoning(t *testing.T) {
	start := time.Now()
	res := sendRawStream(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"+
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ab\"},\"finish_reason\":\"stop\"}]}\n\n"+
		"data: [DONE]\n", 10*time.Second)
	a, _ := openAIAnswer(res)
	if !res.OK() || time.Since(start) > time.Second || a.Text != "ab" || a.CompletionTokens != 2 {
		t.Errorf("ok=%v after %s, text=%q, tokens=%d, err=%q", res.OK(), time.Since(start), a.Text, a.CompletionTokens, res.StreamError)
	}
}
