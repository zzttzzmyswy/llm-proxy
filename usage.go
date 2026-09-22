package main

import (
	"bytes"
	"encoding/json"
	"io"
)

// tokenUsage is how many tokens one upstream call consumed. Reported is false
// when the upstream did not tell us and the caller had to fall back to a
// length-based estimate, so the UI can label those numbers honestly.
//
// Input is the fresh, uncached prompt. The two gateways disagree about what
// their prompt counter means — Anthropic's input_tokens excludes the cache
// counters, OpenAI's prompt_tokens includes the cached ones — so the OpenAI
// extractors subtract the cached count here, at the boundary. Everything
// downstream (stats, the cache hit rate) then has one meaning to reason about,
// and total() stays the same either way because it is a sum.
type tokenUsage struct {
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
	Reported      bool
}

func (u tokenUsage) total() int { return u.Input + u.Output }

// promptTokens is everything the upstream had to read to answer: the fresh
// prompt plus both cache counters. With Input normalised to exclude cached
// tokens, this is the same expression on both gateways.
func (u tokenUsage) promptTokens() int { return u.Input + u.CacheRead + u.CacheCreation }

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

// maxSSELineBytes bounds the partial line the error watcher holds. A line longer
// than this is dropped rather than buffered.
const maxSSELineBytes = 64 << 10

// sseDataError reports the error carried by one SSE `data:` payload, if any.
// Anthropic wraps it in a `type: "error"` event; OpenAI puts a bare `error`
// object in the chunk.
func sseDataError(payload []byte) (string, bool) {
	var ev struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Error == nil {
		return "", false
	}
	if ev.Type != "" && ev.Type != "error" {
		return "", false
	}
	msg := ev.Error.Message
	if msg == "" {
		msg = "upstream reported an error"
	}
	return msg, true
}

// sseError reports the error a buffered SSE body carries. A gateway can report a
// failure as an event inside an HTTP 200 response, which would otherwise be
// counted as a successful request.
func sseError(body []byte) (string, bool) {
	var msg string
	var found bool
	forEachSSEData(body, func(payload []byte) {
		if m, ok := sseDataError(payload); ok {
			msg, found = m, true
		}
	})
	return msg, found
}

// sseErrorWatcher forwards a stream untouched while remembering the first error
// event it carries. It keeps only a bounded partial line, so watching a long
// stream costs a fixed amount of memory.
type sseErrorWatcher struct {
	w      io.Writer
	line   []byte
	msg    string
	failed bool
}

func newSSEErrorWatcher(w io.Writer) *sseErrorWatcher {
	return &sseErrorWatcher{w: w}
}

func (s *sseErrorWatcher) Write(p []byte) (int, error) {
	if !s.failed {
		s.scan(p)
	}
	return s.w.Write(p)
}

func (s *sseErrorWatcher) scan(p []byte) {
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			break
		}
		s.checkLine(s.line[:i])
		s.line = s.line[i+1:]
	}
	if len(s.line) > maxSSELineBytes {
		s.line = s.line[:0]
	}
}

func (s *sseErrorWatcher) checkLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if msg, ok := sseDataError(payload); ok {
		s.msg, s.failed = msg, true
	}
}

// Error reports the first error the watched stream carried, if any.
func (s *sseErrorWatcher) Error() (string, bool) { return s.msg, s.failed }

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
	// The cache counters ride along in the same usage blocks. A stream may
	// report them in message_start only, or repeat them in message_delta.
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
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
		u := m.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 {
			return tokenUsage{}
		}
		return tokenUsage{
			Input:         u.InputTokens,
			Output:        u.OutputTokens,
			CacheRead:     u.CacheReadTokens,
			CacheCreation: u.CacheCreationTokens,
			Reported:      true,
		}
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

// mergeAnthropicUsage folds one usage block into the running total. Each counter
// is taken only when the block actually carries it: a message_delta that reports
// output tokens alone must not clear the input and cache counts message_start
// already gave.
func mergeAnthropicUsage(u tokenUsage, f anthropicUsageFields) tokenUsage {
	if f.InputTokens > 0 {
		u.Input = f.InputTokens
	}
	if f.OutputTokens > 0 {
		u.Output = f.OutputTokens
	}
	if f.CacheReadTokens > 0 {
		u.CacheRead = f.CacheReadTokens
	}
	if f.CacheCreationTokens > 0 {
		u.CacheCreation = f.CacheCreationTokens
	}
	if f.InputTokens > 0 || f.OutputTokens > 0 || f.CacheReadTokens > 0 || f.CacheCreationTokens > 0 {
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
		// Cached tokens are a subset of prompt_tokens, not an addition to it.
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if !isSSE {
		var m struct {
			Usage openAIUsage `json:"usage"`
		}
		if json.Unmarshal(body, &m) != nil {
			return tokenUsage{}
		}
		u := m.Usage
		if u.PromptTokens == 0 && u.CompletionTokens == 0 {
			return tokenUsage{}
		}
		cached := u.PromptTokensDetails.CachedTokens
		return tokenUsage{
			Input:     freshPrompt(u.PromptTokens, cached),
			Output:    u.CompletionTokens,
			CacheRead: cached,
			Reported:  true,
		}
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
			cached := ev.Usage.PromptTokensDetails.CachedTokens
			u.Input = freshPrompt(ev.Usage.PromptTokens, cached)
			u.Output = ev.Usage.CompletionTokens
			u.CacheRead = cached
			u.Reported = true
		}
	})
	return u
}

// freshPrompt turns OpenAI's prompt_tokens (which counts cached tokens inside
// it) into the fresh prompt the rest of the proxy works with. A gateway that
// reports more cached tokens than prompt tokens would otherwise produce a
// negative input, which would in turn push the cache hit rate above 100%.
func freshPrompt(promptTokens, cached int) int {
	if cached <= 0 {
		return promptTokens
	}
	if cached >= promptTokens {
		return 0
	}
	return promptTokens - cached
}
