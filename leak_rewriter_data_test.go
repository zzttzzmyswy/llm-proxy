package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// textDeltas returns every text_delta in the stream as "index:text" strings.
func textDeltas(t *testing.T, stream string) []string {
	t.Helper()
	var out []string
	for _, p := range ssePayloadsAll(stream, "content_block_delta") {
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(p), &ev) != nil {
			continue
		}
		if ev.Delta.Type == "text_delta" {
			out = append(out, ev.Delta.Text)
		}
	}
	return out
}

// blockTypes returns the type of every content_block_start's content_block.
func blockTypes(t *testing.T, stream string) []string {
	t.Helper()
	var out []string
	for _, p := range ssePayloadsAll(stream, "content_block_start") {
		var ev struct {
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(p), &ev) == nil {
			out = append(out, ev.ContentBlock.Type)
		}
	}
	return out
}

// Text that precedes a leaked call inside the same text block must survive. The
// rewriter used to slice the buffer from the open tag and drop everything before
// it, deleting the model's own words from the answer.
func TestLeakRewriterKeepsTextBeforeTheCall(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check. <invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)

	joined := strings.Join(textDeltas(t, out), "")
	if !strings.Contains(joined, "Let me check.") {
		t.Fatalf("the text before the leaked call must survive, got %q", joined)
	}
	// It is prose plus a call, not a bare call, so it stays text.
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a block that is not exactly one call must not become tool_use, got:\n%s", out)
	}
}

// The mirror case: text AFTER the leaked call. The old code converted the block
// as soon as the closing tag appeared and then sent the trailing prose as a
// text_delta at the tool_use block's own index — protocol-corrupt output that
// also silently dropped the words.
func TestLeakRewriterKeepsTextAfterTheCall(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>done, moving on"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)

	joined := strings.Join(textDeltas(t, out), "")
	if !strings.Contains(joined, "done, moving on") {
		t.Fatalf("the text after the leaked call must survive, got %q", joined)
	}
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a mixed block must not become tool_use, got:\n%s", out)
	}
}

// The same hazard spread across deltas: the closing tag lands in one delta and
// the trailing prose in the next.
func TestLeakRewriterKeepsTextAfterTheCallAcrossDeltas(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" and that is all."}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)

	joined := strings.Join(textDeltas(t, out), "")
	if !strings.Contains(joined, "and that is all.") {
		t.Fatalf("trailing prose must survive, got %q", joined)
	}
	// A block that is XML followed by prose is not a bare call.
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a block with trailing prose must not become tool_use, got:\n%s", out)
	}
}

// The legitimate case must keep working: a block that IS exactly one call still
// converts, and no text_delta may be emitted into the tool_use block's index.
func TestLeakRewriterStillConvertsABareCall(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)

	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a bare leaked call must still convert, got:\n%s", out)
	}
	if got := textDeltas(t, out); len(got) != 0 {
		t.Fatalf("a converted block must carry no text_delta, got %q", got)
	}
	if strings.Contains(out, "<invoke") {
		t.Fatalf("the XML must not reach the client, got:\n%s", out)
	}
}

// Whitespace around the call (the upstream usually pads the block with newlines)
// must not defeat the conversion.
func TestLeakRewriterToleratesSurroundingWhitespace(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\n  <invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>\n"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("a call wrapped in whitespace must still convert, got:\n%s", out)
	}
}

// A text block cut short mid-leak (no content_block_stop before message_stop)
// must still be closed, or the client waits on a block that never ends.
func TestLeakRewriterClosesBlockCutShortMidLeak(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"comm"}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	)
	out := runRewriter(t, req, stream)

	if !strings.Contains(out, `"content_block_stop"`) {
		t.Fatalf("the unterminated text block must be closed, got:\n%s", out)
	}
	// The partial XML carries no usable call, so it is replayed as text: losing
	// it would silently drop whatever the model actually said.
	if !strings.Contains(strings.Join(textDeltas(t, out), ""), "<invoke") {
		t.Fatalf("a half-arrived leak must be replayed as text, got:\n%s", out)
	}
}

// A block whose XML names a tool the request never declared stays text, and the
// whole block must reach the client.
func TestLeakRewriterKeepsUnlistedToolAsText(t *testing.T) {
	req := tools("Bash")
	stream := sse(
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Nope\"><parameter name=\"a\">1</parameter></invoke>"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	)
	out := runRewriter(t, req, stream)
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("an undeclared tool must not be converted, got:\n%s", out)
	}
	if !strings.Contains(strings.Join(textDeltas(t, out), ""), "Nope") {
		t.Fatalf("the un-converted block must reach the client as text, got:\n%s", out)
	}
}

// The reply stream must still parse: block starts and stops have to pair up.
func TestLeakRewriterKeepsBlockStructureBalanced(t *testing.T) {
	for name, stream := range map[string]string{
		"bare call": sse(
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		),
		"text then call": sse(
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi <invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		),
		"call then text": sse(
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke> bye"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		),
	} {
		out := runRewriter(t, tools("Bash"), stream)
		starts, stops := 0, 0
		for _, e := range blockTypes(t, out) {
			if e != "" {
				starts++
			}
		}
		stops = strings.Count(out, `"content_block_stop"`)
		if starts != stops {
			t.Fatalf("%s: %d content_block_start vs %d content_block_stop:\n%s", name, starts, stops, out)
		}
		// Every text_delta must sit inside a text block, never at a tool_use index.
		if strings.Contains(out, `"type":"tool_use"`) {
			toolStarts := 0
			for _, p := range ssePayloadsAll(out, "content_block_start") {
				if strings.Contains(p, `"type":"tool_use"`) {
					toolStarts++
				}
			}
			if toolStarts > 0 && len(textDeltas(t, out)) > 0 {
				// A tool_use block coexisting with text_delta is fine only when the
				// text_delta belongs to a different (text) block. Here a single
				// block is involved, so any text_delta means the mixed block was
				// split across two block types — the failure mode being guarded.
				if len(blockTypes(t, out)) < 2 {
					t.Fatalf("%s: text_delta emitted into a tool_use block:\n%s", name, out)
				}
			}
		}
	}
}
