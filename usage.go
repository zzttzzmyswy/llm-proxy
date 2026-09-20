package main

import (
	"bytes"
	"encoding/json"
	"io"
)

// tokenUsage is how many tokens one upstream call consumed. Reported is false
// when the upstream did not tell us and the caller had to fall back to a
// length-based estimate, so the UI can label those numbers honestly.
type tokenUsage struct {
	Input    int
	Output   int
	Reported bool
}

func (u tokenUsage) total() int { return u.Input + u.Output }

// maxNonStreamBuffer caps how much of a non-streaming upstream body is retained
// for usage extraction. A single message is far below this; a body that exceeds
// it still reaches the client untouched, only its token accounting is lost.
const maxNonStreamBuffer = 4 << 20

// usageTailBytes is how much of a streaming response is retained to look for its
// trailing usage chunk.
const usageTailBytes = 64 << 10

// bodyCapture is a bounded buffer for a response that is being forwarded
// verbatim: it never changes what the client receives, it only keeps enough to
// read the usage block out of it.
type bodyCapture interface {
	io.Writer
	Bytes() []byte
	// Complete reports whether the retained bytes are a faithful copy of the
	// response, i.e. whether parsing them can yield a real answer.
	Complete() bool
}

// newBodyCapture picks a buffer shape for the response: a stream is inspected at
// its tail, where the usage chunk sits, while a whole body is kept from the start.
func newBodyCapture(isSSE bool) bodyCapture {
	if isSSE {
		return newTailBuffer(usageTailBytes)
	}
	return newLimitedBuffer(maxNonStreamBuffer)
}

// tailBuffer keeps only the last max bytes written to it. Streaming responses are
// inspected for a trailing usage chunk, which lets the proxy read that chunk
// without buffering the whole stream in memory.
type tailBuffer struct {
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.max <= 0 {
		return n, nil
	}
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if overflow := len(t.buf) + len(p) - t.max; overflow > 0 {
		t.buf = append(t.buf[:0], t.buf[overflow:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) Bytes() []byte { return t.buf }

// Complete always reports true: a dropped prefix does not affect the trailing
// usage chunk a stream is inspected for.
func (t *tailBuffer) Complete() bool { return true }

// limitedBuffer keeps the first max bytes written to it and drops the rest, so a
// large non-streaming body cannot grow the proxy's memory without bound.
// truncated reports whether anything was dropped, which tells the caller that
// the buffer is no longer parseable.
type limitedBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func newLimitedBuffer(max int) *limitedBuffer { return &limitedBuffer{max: max} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf }

func (b *limitedBuffer) Complete() bool { return !b.truncated }

// forEachSSEData calls fn with the payload of every non-empty `data:` line,
// skipping the OpenAI stream terminator.
func forEachSSEData(body []byte, fn func([]byte)) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		fn(payload)
	}
}

type anthropicUsageFields struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// extractAnthropicUsage reads token counts from an Anthropic response. A
// non-streaming reply carries usage directly; an SSE stream reports input tokens
// in message_start and the running output total in message_delta, so the last
// non-zero value seen wins.
func extractAnthropicUsage(body []byte, isSSE bool) tokenUsage {
	if !isSSE {
		var m struct {
			Usage anthropicUsageFields `json:"usage"`
		}
		if json.Unmarshal(body, &m) != nil {
			return tokenUsage{}
		}
		if m.Usage.InputTokens == 0 && m.Usage.OutputTokens == 0 {
			return tokenUsage{}
		}
		return tokenUsage{Input: m.Usage.InputTokens, Output: m.Usage.OutputTokens, Reported: true}
	}

	var u tokenUsage
	forEachSSEData(body, func(payload []byte) {
		var ev struct {
			Message *struct {
				Usage anthropicUsageFields `json:"usage"`
			} `json:"message"`
			Usage *anthropicUsageFields `json:"usage"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return
		}
		if ev.Message != nil {
			u = mergeAnthropicUsage(u, ev.Message.Usage)
		}
		if ev.Usage != nil {
			u = mergeAnthropicUsage(u, *ev.Usage)
		}
	})
	return u
}

func mergeAnthropicUsage(u tokenUsage, f anthropicUsageFields) tokenUsage {
	if f.InputTokens > 0 {
		u.Input = f.InputTokens
	}
	if f.OutputTokens > 0 {
		u.Output = f.OutputTokens
	}
	if f.InputTokens > 0 || f.OutputTokens > 0 {
		u.Reported = true
	}
	return u
}

// extractOpenAIUsage reads token counts from an OpenAI-format response. Streaming
// gateways only report usage when the request asked for it (stream_options
// include_usage), so an absent usage block yields Reported=false.
func extractOpenAIUsage(body []byte, isSSE bool) tokenUsage {
	type openAIUsage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	if !isSSE {
		var m struct {
			Usage openAIUsage `json:"usage"`
		}
		if json.Unmarshal(body, &m) != nil {
			return tokenUsage{}
		}
		if m.Usage.PromptTokens == 0 && m.Usage.CompletionTokens == 0 {
			return tokenUsage{}
		}
		return tokenUsage{Input: m.Usage.PromptTokens, Output: m.Usage.CompletionTokens, Reported: true}
	}

	var u tokenUsage
	forEachSSEData(body, func(payload []byte) {
		var ev struct {
			Usage *openAIUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &ev) != nil || ev.Usage == nil {
			return
		}
		if ev.Usage.PromptTokens > 0 || ev.Usage.CompletionTokens > 0 {
			u.Input = ev.Usage.PromptTokens
			u.Output = ev.Usage.CompletionTokens
			u.Reported = true
		}
	})
	return u
}
