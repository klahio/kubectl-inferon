package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Output formats of `test -o` (docs/mvp.md section 5).
const (
	OutputPretty  = "pretty"
	OutputSummary = "summary"
	OutputRaw     = "raw"
	OutputJSON    = "json"
)

// Outputs lists the accepted formats.
var Outputs = []string{OutputPretty, OutputSummary, OutputRaw, OutputJSON}

// summaryBodyLimit keeps the one-line summary on one terminal line.
const summaryBodyLimit = 120

// Write prints the result in the chosen format.
func Write(w io.Writer, format string, r *Result) error {
	switch format {
	case OutputRaw:
		_, err := w.Write(r.Body)
		return err
	case OutputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case OutputSummary:
		_, err := fmt.Fprintf(w, "HTTP %d · %d ms · via %s · %s%s\n", r.StatusCode, r.DurationMs, shortRoute(r.Route), summarize(r), streamMetrics(r, " · "))
		return err
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s\n", r.Method, r.URL)
		if r.ConnectTo != "" {
			fmt.Fprintf(&b, "  connected to %s\n", r.ConnectTo)
		}
		fmt.Fprintf(&b, "  protocol %s (%s) · via %s\n", r.Protocol, r.ProtocolBy, r.Route)
		fmt.Fprintf(&b, "%s · %d ms\n\n", r.Status, r.DurationMs)
		if r.StreamError != "" {
			fmt.Fprintf(&b, "stream error: %s\n", r.StreamError)
		}
		if a, ok := openAIAnswer(r); ok && r.StatusCode >= 200 && r.StatusCode < 300 {
			b.WriteString(a.Text + "\n\n")
			fmt.Fprintf(&b, "model %s · finish %s · tokens %d prompt + %d completion\n", dash(a.Model), dash(a.Finish), a.PromptTokens, a.CompletionTokens)
			if m := streamMetrics(r, ""); m != "" {
				b.WriteString(m + "\n")
			}
		} else {
			b.WriteString(prettyBody(r))
		}
		_, err := io.WriteString(w, b.String())
		return err
	}
}

// streamMetrics prints the time to the first token and the generation rate of a streamed answer, after sep.
func streamMetrics(r *Result, sep string) string {
	if r.FirstTokenMs == nil {
		return ""
	}
	s := fmt.Sprintf("%sfirst token %d ms", sep, *r.FirstTokenMs)
	if r.TokensPerSecond != nil {
		s += fmt.Sprintf(" · %.1f tokens/s after it", *r.TokensPerSecond)
	}
	if r.TokensCounted {
		s += " (tokens counted as chunks; the server sent no usage)"
	}
	return s
}

func prettyBody(r *Result) string {
	var buf bytes.Buffer
	if r.JSONBody != nil && json.Indent(&buf, r.Body, "", "  ") == nil {
		buf.WriteByte('\n')
		return buf.String()
	}
	s := string(r.Body)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// summarize picks the interesting part of a body: v1 predictions, v2 outputs, an error message, or the body itself.
func summarize(r *Result) string {
	if r.StreamError != "" {
		return "stream error: " + shorten(r.StreamError)
	}
	if a, ok := openAIAnswer(r); ok {
		return fmt.Sprintf("%q · %d+%d tokens", shorten(strings.Join(strings.Fields(a.Text), " ")), a.PromptTokens, a.CompletionTokens)
	}
	obj, ok := r.JSONBody.(map[string]any)
	if !ok {
		return shorten(strings.TrimSpace(string(r.Body)))
	}
	if p, ok := obj["predictions"]; ok {
		return "predictions: " + shorten(compact(p))
	}
	if outs, ok := obj["outputs"].([]any); ok {
		var parts []string
		for _, o := range outs {
			om, _ := o.(map[string]any)
			parts = append(parts, fmt.Sprintf("%v=%s", om["name"], compact(om["data"])))
		}
		return "outputs: " + shorten(strings.Join(parts, " "))
	}
	for _, key := range []string{"error", "detail", "message"} {
		if v, ok := obj[key]; ok {
			return key + ": " + shorten(compact(v))
		}
	}
	return shorten(compact(obj))
}

func compact(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return strings.ReplaceAll(string(data), ",", ", ")
}

func shorten(s string) string {
	if len(s) <= summaryBodyLimit {
		return s
	}
	return s[:summaryBodyLimit] + "…"
}

// shortRoute keeps the first words of a route ("port-forward", "external address", "Ingress …").
func shortRoute(route string) string {
	if strings.HasPrefix(route, "port-forward") {
		return "port-forward"
	}
	if i := strings.Index(route, " at "); i > 0 {
		return route[:i]
	}
	return route
}

// answer is the useful part of an OpenAI chat or completion response.
type answer struct {
	Text             string
	Model            string
	Finish           string
	PromptTokens     int
	CompletionTokens int
}

// openAIAnswer reads choices[0] (message.content for chat, text for completions) and the token usage.
func openAIAnswer(r *Result) (answer, bool) {
	if r.Protocol != OpenAI {
		return answer{}, false
	}
	if r.streamed != nil {
		return *r.streamed, true
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Text    string `json:"text"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(r.Body, &resp); err != nil || len(resp.Choices) == 0 {
		return answer{}, false
	}
	c := resp.Choices[0]
	text := c.Message.Content
	if text == "" {
		text = c.Text
	}
	return answer{
		Text:             strings.TrimSpace(text),
		Model:            resp.Model,
		Finish:           c.FinishReason,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}, true
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
