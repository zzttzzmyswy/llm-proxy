package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BurntSushi/toml"
)

// withStoredConfig installs a config directly, bypassing the file, and restores
// the previous one when the test ends.
func withStoredConfig(t *testing.T, c Config, routes map[string]RouteEntry) {
	t.Helper()
	prevCfg, prevRoutes, prevDeclared := currentConfig(), currentRoutes(), currentDeclaredRoutes()
	storeConfig(c, routes, routes)
	t.Cleanup(func() { storeConfig(prevCfg, prevRoutes, prevDeclared) })
}

// P1: a config published while a request is in flight must not be picked up by
// the second half of that request. The original bug paired the model chosen
// before the reload with the upstream URL and key read after it.
func TestReviewRequestKeepsOneConfigSnapshot(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)

	release := make(chan struct{})
	vlmStarted := make(chan struct{}, 1)

	var mu sync.Mutex
	var calls int
	var mainModel, mainKey string

	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)

		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// The image-describe pass. Hold it open so the test can publish a
			// new config while this request is mid-flight.
			select {
			case vlmStarted <- struct{}{}:
			default:
			}
			<-release
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"一只猫"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		mu.Lock()
		mainModel, mainKey = model, r.Header.Get("x-api-key")
		mu.Unlock()
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(oldSrv.Close)

	// Any request reaching the replacement upstream means the snapshot leaked.
	var newHits int64
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&newHits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(newSrv.Close)

	withStoredConfig(t, Config{
		Proxy:    ProxyConfig{VLMModel: "MiniMax-M3", VLMMaxTokens: 100},
		Upstream: UpstreamConfig{AnthropicURL: oldSrv.URL, OpenAIURL: oldSrv.URL},
		Keys:     KeysConfig{Sophnet: "old-key"},
	}, map[string]RouteEntry{"sonnet": {Model: "Old-Model"}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMG-SNAPSHOT"}}]}]}`)
	}()

	<-vlmStarted
	// Publish a replacement config: different model, upstream and key.
	storeConfig(
		Config{
			Proxy:    ProxyConfig{VLMModel: "MiniMax-M3", VLMMaxTokens: 100},
			Upstream: UpstreamConfig{AnthropicURL: newSrv.URL, OpenAIURL: newSrv.URL},
			Keys:     KeysConfig{Sophnet: "new-key"},
		},
		map[string]RouteEntry{"sonnet": {Model: "New-Model"}},
		map[string]RouteEntry{"sonnet": {Model: "New-Model"}},
	)
	close(release)
	<-done

	if got := atomic.LoadInt64(&newHits); got != 0 {
		t.Fatalf("the in-flight request must keep its snapshot; %d request(s) reached the new upstream", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if mainModel != "Old-Model" {
		t.Fatalf("the request must keep the model chosen before the reload, got %q", mainModel)
	}
	if mainKey != "old-key" {
		t.Fatalf("the request must keep the key from its own snapshot, got %q", mainKey)
	}
}

// P1: the model cap must also bound the alias map. On the passthrough endpoint
// the alias is the client-supplied model name, so the (other) bucket used to
// retain one entry per distinct name.
func TestReviewAliasMapStaysBounded(t *testing.T) {
	c, _ := newTestCollector()

	for i := 0; i < 10000; i++ {
		c.beginReq("alias-"+strconv.Itoa(i), "shared-model", "openai").success(tokenUsage{Reported: true})
	}
	m := modelByName(t, c.snapshot(), "shared-model")
	if len(m.Aliases) > maxTrackedAliases+1 {
		t.Fatalf("alias map must stay bounded, got %d entries", len(m.Aliases))
	}
	if got := c.snapshot().Totals.Requests; got != 10000 {
		t.Fatalf("no request may be lost to the alias cap, got %d", got)
	}

	// Overflow models share the (other) bucket; its alias map must be bounded too.
	big, _ := newTestCollector()
	for i := 0; i < 10000; i++ {
		big.beginReq("model-"+strconv.Itoa(i), "model-"+strconv.Itoa(i), "openai").success(tokenUsage{Reported: true})
	}
	snap := big.snapshot()
	if len(snap.Models) > maxTrackedModels+1 {
		t.Fatalf("model buckets must stay bounded, got %d", len(snap.Models))
	}
	other := modelByName(t, snap, otherModelKey)
	if len(other.Aliases) > maxTrackedAliases+1 {
		t.Fatalf("the overflow bucket's alias map must stay bounded, got %d entries", len(other.Aliases))
	}
	if snap.Totals.Requests != 10000 {
		t.Fatalf("no request may be lost, got %d", snap.Totals.Requests)
	}
}

// A client-supplied label must not be retained at unbounded length either.
func TestReviewLabelLengthIsBounded(t *testing.T) {
	c, _ := newTestCollector()
	long := strings.Repeat("x", 10000)
	c.beginReq(long, long, "openai").success(tokenUsage{Reported: true})

	snap := c.snapshot()
	if len(snap.Models) != 1 {
		t.Fatalf("one request must produce one bucket, got %d", len(snap.Models))
	}
	if got := len(snap.Models[0].Model); got > maxLabelLen {
		t.Fatalf("model label must be bounded, got %d bytes", got)
	}
	for alias := range snap.Models[0].Aliases {
		if len(alias) > maxLabelLen {
			t.Fatalf("alias label must be bounded, got %d bytes", len(alias))
		}
	}
}

// P2: a failure a gateway reports as an event inside an HTTP 200 stream must be
// counted as a failure, not hidden from the dashboard.
func TestReviewAnthropicStreamErrorCountsAsFailure(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "text/event-stream",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n")

	out := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(out, "overloaded") {
		t.Fatalf("the client must still receive the error, got %q", out)
	}

	m := findModel(t, "DeepSeek-Flash")
	if m.Failures != 1 || m.Successes != 0 {
		t.Fatalf("an in-stream error must count as a failure, got %+v", m)
	}
	if m.ErrorCounts[catUpstreamStreamError] != 1 {
		t.Fatalf("the failure must be categorised, got %+v", m.ErrorCounts)
	}
}

func TestReviewOpenAIStreamErrorCountsAsFailure(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"flash": {Model: "glm-5.3-flash", Upstream: "openai"}})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.OpenAIURL
	cfg.Upstream.OpenAIURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.OpenAIURL = oldURL })

	callHandleMessages(t, `{"model":"flash","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "glm-5.3-flash")
	if m.Failures != 1 || m.Successes != 0 {
		t.Fatalf("an in-stream error must count as a failure, got %+v", m)
	}
	if m.ErrorCounts[catUpstreamStreamError] != 1 {
		t.Fatalf("the failure must be categorised, got %+v", m.ErrorCounts)
	}
}

func TestReviewPassthroughStreamErrorCountsAsFailure(t *testing.T) {
	withCleanStats(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.OpenAIURL
	cfg.Upstream.OpenAIURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.OpenAIURL = oldURL })

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[]}`))
	w := httptest.NewRecorder()
	handleChatCompletions(w, req)

	if !strings.Contains(w.Body.String(), "overloaded") {
		t.Fatalf("the passthrough body must reach the client unchanged, got %q", w.Body.String())
	}
	m := findModel(t, "glm-5.3-flash")
	if m.Failures != 1 || m.Successes != 0 {
		t.Fatalf("an in-stream error must count as a failure, got %+v", m)
	}
}

// P2: when a streaming gateway reports no usage at all, the input count must be
// estimated from the request rather than reported as a hard zero.
func TestReviewMissingUsageEstimatesInput(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"flash": {Model: "glm-5.3-flash", Upstream: "openai"}})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.OpenAIURL
	cfg.Upstream.OpenAIURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.OpenAIURL = oldURL })

	prompt := strings.Repeat("请解释这段代码。", 20)
	callHandleMessages(t, fmt.Sprintf(`{"model":"flash","max_tokens":10,"stream":true,"messages":[{"role":"user","content":%q}]}`, prompt))

	m := findModel(t, "glm-5.3-flash")
	if m.InputTokens == 0 {
		t.Fatal("a missing usage block must not be reported as zero input tokens")
	}
	if m.OutputTokens == 0 {
		t.Fatal("the output estimate must still be recorded")
	}
	if !m.Estimated {
		t.Fatal("an estimated request must be flagged so the page does not present it as exact")
	}
}

// P2: the editor must expose the routing entries as declared, so a value
// inherited from default_upstream is not frozen into the file by a save.
func TestReviewDefaultGatewayStaysInherited(t *testing.T) {
	const inheritedConfig = `[upstream]
default_upstream = "openai"

[routing]
sonnet = "Model-A"
`
	withTempConfig(t, inheritedConfig)

	if got := snapshotConfig().routeTarget("sonnet").Upstream; got != "openai" {
		t.Fatalf("sonnet must inherit the openai default, got %q", got)
	}

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var view adminConfigView
	decodeBody(t, w, &view)

	// The form must show the declared entry, with no explicit gateway.
	if len(view.Routing) != 1 || view.Routing[0].Alias != "sonnet" {
		t.Fatalf("routing must list the declared aliases, got %+v", view.Routing)
	}
	if view.Routing[0].Upstream != "" {
		t.Fatalf("an inherited gateway must not be materialised into the form, got %q", view.Routing[0].Upstream)
	}

	// Switch the default gateway to Anthropic and save.
	p := payloadFromView(t, view)
	p.Upstream.DefaultUpstream = ""
	res := postConfig(t, p, "")
	if res.Code != http.StatusOK {
		t.Fatalf("save failed: %s", res.Body.String())
	}

	if got := snapshotConfig().routeTarget("sonnet").Upstream; got != "" {
		t.Fatalf("sonnet must follow the new default gateway, still %q", got)
	}
}

// P2: two saves in the same second must not overwrite the first save's copy of
// the original file.
func TestReviewBackupsAreUniquePerSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# original"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, firstPath, err := backupConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, secondPath, err := backupConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if firstPath == secondPath {
		t.Fatalf("each save needs its own backup, both used %q", firstPath)
	}
	if string(first) != "# original" || string(second) != "# rewritten" {
		t.Fatalf("backups must capture the file as it was: %q / %q", first, second)
	}
	onDisk, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("the first backup must survive the second save: %v", err)
	}
	if string(onDisk) != "# original" {
		t.Fatalf("the first backup was overwritten: %q", onDisk)
	}
}

// P2: concurrent saves must not leave the file and the running config disagreeing.
func TestReviewConcurrentSavesStayConsistent(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := payloadFromView(t, view)
			p.Proxy.VLMModel = fmt.Sprintf("VLM-%d", i)
			res := postConfig(t, p, "s3cret")
			if res.Code != http.StatusOK {
				t.Errorf("concurrent save %d failed: %d %s", i, res.Code, res.Body.String())
			}
		}(i)
	}
	wg.Wait()

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Config
	if err := toml.Unmarshal(onDisk, &stored); err != nil {
		t.Fatalf("saved config must stay parseable: %v", err)
	}
	if inMemory := currentConfig().Proxy.VLMModel; inMemory != stored.Proxy.VLMModel {
		t.Fatalf("disk and memory disagree after concurrent saves: disk=%q memory=%q", stored.Proxy.VLMModel, inMemory)
	}
}

// P2: a save warning must not live in the region the stats refresh clears.
func TestReviewSaveWarningsHaveTheirOwnRegion(t *testing.T) {
	page := string(adminHTML)
	if !strings.Contains(page, `id="save-banner"`) {
		t.Fatal("save results need their own message region")
	}
	if !strings.Contains(page, `hideBanner("banner")`) {
		t.Fatal("the stats refresh must hide only the stats region")
	}
	if strings.Contains(page, "hideBanner()") {
		t.Fatal("a bare hideBanner() would clear the save warnings too")
	}
}

// The bypass helper used above must not leave the declared routes behind.
func TestReviewStoreConfigPublishesDeclaredRoutes(t *testing.T) {
	declared := map[string]RouteEntry{"alias": {Model: "declared-model"}}
	withStoredConfig(t, Config{}, declared)

	if got := currentDeclaredRoutes()["alias"].Model; got != "declared-model" {
		t.Fatalf("declared routes must be published, got %q", got)
	}
}

// P2: a failed save must not promise the config was left untouched. The backend
// distinguishes a failed reload from a failed rollback, and a response lost in
// flight cannot be told apart from a write that never happened.
func TestReviewSaveFailureMessageDoesNotPromiseRollback(t *testing.T) {
	page := string(adminHTML)
	if strings.Contains(page, "配置未改动") {
		t.Fatal("a save failure must not claim the config is unchanged")
	}
	if !strings.Contains(page, `"保存失败："`) {
		t.Fatal("the save failure banner must keep the backend's own status text")
	}
}

// P3: LLM_PROXY_ADMIN_TOKEN outranks the file, so clearing the file password
// leaves the page open. The warning must describe that, not claim every /admin*
// now returns 404.
func TestReviewClearWarningRespectsEnvPassword(t *testing.T) {
	withTempConfig(t, testConfigTOML)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env-token")

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "env-token")
	var view adminConfigView
	decodeBody(t, w, &view)

	p := payloadFromView(t, view)
	p.Admin = adminAuthPayload{Action: "clear"}

	var out struct {
		Warnings []string `json:"warnings"`
	}
	res := postConfig(t, p, "env-token")
	if res.Code != http.StatusOK {
		t.Fatalf("clear failed: %d %s", res.Code, res.Body.String())
	}
	decodeBody(t, res, &out)

	joined := strings.Join(out.Warnings, " | ")
	if strings.Contains(joined, "管理页面现已关闭") || strings.Contains(joined, "404") {
		t.Fatalf("the env password keeps the page open, got warnings: %v", out.Warnings)
	}
	if !strings.Contains(joined, "仍然生效") {
		t.Fatalf("the warning must say the env password still applies, got: %v", out.Warnings)
	}

	// And the page really is still reachable with the environment password.
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "env-token"); w.Code != http.StatusOK {
		t.Fatalf("the env password must keep the page open, got %d", w.Code)
	}
}
