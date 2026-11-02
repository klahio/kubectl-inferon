package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// maxEventLine bounds one server-sent event line; a chunk carries a few tokens, far below this.
const maxEventLine = 1 << 20

// stream collects an OpenAI server-sent event stream: events of "data: {chunk}" lines separated by blank lines,
// ended by "data: [DONE]". Only the first choice is shown, as for complete responses.
type stream struct {
	text         strings.Builder
	model        string
	finishReason string
	usage        *usage
	chunks       int // chunks that carried text
	firstToken   time.Duration
	done         bool   // [DONE] arrived
	err          string // an error event, which servers send with status 200 when generation fails mid-stream
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// read consumes the stream until [DONE] or the end of the body, and returns the raw bytes; start is when the
// request was sent. It stops at [DONE], so a server that keeps the connection open does not run into the timeout.
func (s *stream) read(r io.Reader, start time.Time) ([]byte, error) {
	var raw bytes.Buffer
	sc := bufio.NewScanner(io.TeeReader(r, &raw))
	sc.Buffer(make([]byte, 64<<10), maxEventLine)
	var data []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		// [DONE] ends the stream at once, even when the server sends no blank line after it and keeps the connection.
		if len(data) == 0 && strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]" && strings.HasPrefix(line, "data:") {
			s.done = true
			return raw.Bytes(), nil
		}
		if line != "" {
			// A field is "name: value"; an event may spread its data over several lines, joined by newlines.
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(v, " "))
			}
			continue
		}
		if len(data) > 0 && s.event(strings.Join(data, "\n"), start) {
			return raw.Bytes(), nil
		}
		data = nil
	}
	if len(data) > 0 {
		s.event(strings.Join(data, "\n"), start)
	}
	return raw.Bytes(), sc.Err()
}

// event handles one event's data and reports whether the stream is over.
func (s *stream) event(data string, start time.Time) bool {
	if strings.TrimSpace(data) == "[DONE]" {
		s.done = true
		return true
	}
	var chunk struct {
		Model   string `json:"model"`
		Choices []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
			Delta struct {
				Content string `json:"content"`
				// Reasoning models stream their thinking first, under one of these names; it counts toward the
				// first token and the rate (the usage counts it too), but is not part of the answer.
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *usage          `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		s.err = "unreadable event: " + shorten(data)
		return true
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(chunk.Error, &e) != nil || e.Message == "" {
			e.Message = string(chunk.Error)
		}
		s.err = e.Message
		return true
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	for _, c := range chunk.Choices {
		if c.Index != 0 {
			continue
		}
		piece := c.Delta.Content + c.Text
		if piece != "" || c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" {
			if s.chunks == 0 {
				s.firstToken = time.Since(start)
			}
			s.chunks++
			s.text.WriteString(piece)
		}
		if c.FinishReason != "" {
			s.finishReason = c.FinishReason
		}
	}
	return false
}

// finish fills the result: the response put together from the chunks, the timing, and whether the stream failed.
func (s *stream) finish(res *Result, elapsed time.Duration) {
	res.Streamed = true
	res.StreamError = s.err
	if s.err == "" && !s.done && s.finishReason == "" {
		res.StreamError = "the stream ended before the answer was complete (no finish reason and no [DONE])"
	}
	a := &answer{Text: strings.TrimSpace(s.text.String()), Model: s.model, Finish: s.finishReason}
	tokens := s.chunks
	if s.usage != nil {
		a.PromptTokens, a.CompletionTokens = s.usage.PromptTokens, s.usage.CompletionTokens
		tokens = s.usage.CompletionTokens
	} else {
		a.CompletionTokens = s.chunks
		res.TokensCounted = true
	}
	res.streamed = a
	res.JSONBody = map[string]any{
		"model":   a.Model,
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": a.Text}, "finish_reason": a.Finish}},
		"usage":   map[string]any{"prompt_tokens": a.PromptTokens, "completion_tokens": a.CompletionTokens},
	}
	if s.chunks == 0 {
		return
	}
	ms := s.firstToken.Milliseconds()
	res.FirstTokenMs = &ms
	if window := elapsed - s.firstToken; tokens > 1 && window > 0 {
		rate := float64(tokens-1) / window.Seconds()
		res.TokensPerSecond = &rate
	}
}
