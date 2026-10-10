package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeConfigAndLoad writes a config file and loads it, restoring nothing: the
// caller's t.Setenv scopes the path to the test.
func writeConfigAndLoad(t *testing.T, toml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_PROXY_CONFIG", path)
	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
}

// The admin validator accepts a gateway name case-insensitively, so "OpenAI" is a
// value the page will happily save. The router compared the raw string against
// "openai", so such a route silently went to the Anthropic gateway instead — the
// wrong upstream, no error, and the only symptom a 404 or a model rejection from
// the gateway the operator did not mean to use.
func TestUppercaseOpenAIUpstreamSelectsTheOpenAIGateway(t *testing.T) {
	var anthropicCalls, openAICalls int

	anthro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"from anthropic"}],"model":"x","id":"x","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(anthro.Close)

	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","model":"glm-5.3-flash","choices":[{"index":0,"message":{"role":"assistant","content":"from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(openAI.Close)

	writeConfigAndLoad(t, `[upstream]
anthropic_url = "`+anthro.URL+`"
openai_url = "`+openAI.URL+`"

[routing]
flash = { model = "glm-5.3-flash", upstream = "OpenAI" }
`)

	if e := currentRoutes()["flash"]; e.Upstream != "openai" {
		t.Fatalf("upstream %q must be canonicalised to \"openai\", got %q", "OpenAI", e.Upstream)
	}

	callHandleMessages(t, `{"model":"flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if openAICalls == 0 {
		t.Fatalf("upstream=\"OpenAI\" must reach the OpenAI gateway, got %d openai / %d anthropic calls",
			openAICalls, anthropicCalls)
	}
	if anthropicCalls != 0 {
		t.Fatalf("an OpenAI route must not touch the Anthropic gateway, got %d calls", anthropicCalls)
	}
}

// The same for the other spellings the validator accepts, and for the builtin
// fallbacks the default gateway fills in.
func TestUppercaseDefaultUpstreamSelectsTheOpenAIGateway(t *testing.T) {
	var anthropicCalls, openAICalls int

	anthro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"anthropic"}],"model":"x","id":"x","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(anthro.Close)

	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(openAI.Close)

	writeConfigAndLoad(t, `[upstream]
anthropic_url = "`+anthro.URL+`"
openai_url = "`+openAI.URL+`"
default_upstream = "OpenAI"
`)

	if e, ok := currentRoutes()["sonnet"]; !ok || e.Upstream != "openai" {
		t.Fatalf("default_upstream=\"OpenAI\" must fill the builtin routes with \"openai\", got %+v", e)
	}

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if openAICalls == 0 || anthropicCalls != 0 {
		t.Fatalf("default_upstream=\"OpenAI\" must use the OpenAI gateway, got %d openai / %d anthropic",
			openAICalls, anthropicCalls)
	}
}

// Every spelling the admin validator admits must survive a decode, folded to the
// lowercase form. Only the case changes: "" stays "unset" and is deliberately not
// merged with an explicit "anthropic", which must survive the default-gateway pass.
func TestUpstreamNameNormalisation(t *testing.T) {
	for raw, want := range map[string]string{
		"openai":    "openai",
		"OpenAI":    "openai",
		"OPENAI":    "openai",
		" openai ":  "openai",
		"":          "",
		"claude":    "claude",
		"anthropic": "anthropic",
		"Anthropic": "anthropic",
	} {
		if got := normalizeUpstreamName(raw); got != want {
			t.Fatalf("normalizeUpstreamName(%q) = %q, want %q", raw, got, want)
		}
	}
}
