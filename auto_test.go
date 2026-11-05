package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func resetAutoState() {
	resetTrialCache()
	relayGate = &relayGateKind{}
	activeConfig.Store(defaultConfig())
}

// 体验套餐并发只有 1: 超出的请求必须立刻失败(由调用方回落正常渠道), 不能排队。
func TestRelayGateAllowsOnlyConfiguredConcurrency(t *testing.T) {
	resetAutoState()
	if !relayGate.tryAcquire(1) {
		t.Fatal("first acquire should succeed")
	}
	if relayGate.tryAcquire(1) {
		t.Fatal("second acquire must be refused while the trial slot is busy")
	}
	if inflight, limit := relayGate.stats(); inflight != 1 || limit != 1 {
		t.Fatalf("stats = %d/%d, want 1/1", inflight, limit)
	}
	relayGate.release()
	if !relayGate.tryAcquire(1) {
		t.Fatal("slot should be reusable after release")
	}
	relayGate.release()
	if inflight, _ := relayGate.stats(); inflight != 0 {
		t.Fatalf("inflight = %d, want 0", inflight)
	}
}

func TestRelayGateReleaseIsIdempotent(t *testing.T) {
	resetAutoState()
	relayGate.release()
	relayGate.release()
	if inflight, _ := relayGate.stats(); inflight != 0 {
		t.Fatalf("inflight = %d, want 0 (release must not go negative)", inflight)
	}
}

// 并发上限可配置, 且调小后不会泄漏计数。
func TestRelayGateHonoursLargerLimit(t *testing.T) {
	resetAutoState()
	if !relayGate.tryAcquire(2) || !relayGate.tryAcquire(2) {
		t.Fatal("limit 2 should admit two requests")
	}
	if relayGate.tryAcquire(2) {
		t.Fatal("third request must be refused for limit 2")
	}
	relayGate.release()
	relayGate.release()
	relayGate.release()
	if inflight, _ := relayGate.stats(); inflight != 0 {
		t.Fatalf("inflight = %d, want 0", inflight)
	}
}

// "自动把可用模型路由到体验套餐": 默认只覆盖 glm-5.3 / glm-5.3-flash。
func TestTrialCoversModel(t *testing.T) {
	defaults := trialStatus{Available: true, DefaultModels: true, Models: []string{"glm-5.3", "glm-5.3-flash"}}
	cases := []struct {
		name   string
		status trialStatus
		model  string
		want   bool
	}{
		{"default list covers glm-5.3", defaults, "glm-5.3", true},
		{"default list covers glm-5.3-flash", defaults, "glm-5.3-flash", true},
		{"default list is case insensitive", defaults, "GLM-5.3-Flash", true},
		{"provider prefix is ignored", defaults, "bigmodel/glm-5.3", true},
		{"default list does not cover other glm models", defaults, "glm-5.2", false},
		{"default list does not cover flashx", defaults, "glm-5.3-flashx", false},
		{"default list does not cover claude", defaults, "claude-sonnet-4", false},
		{"empty list covers nothing", trialStatus{Available: true}, "glm-5.3", false},
		{"exact match", trialStatus{Available: true, Models: []string{"glm-5.3"}}, "glm-5.3", true},
		{"uncovered model", trialStatus{Available: true, Models: []string{"glm-5.3"}}, "glm-4.6", false},
		{"wildcard", trialStatus{Available: true, Models: []string{"glm-5.*"}}, "glm-5.3-flash", true},
		{"explicit star covers everything", trialStatus{Available: true, Models: []string{"*"}}, "claude-sonnet-4", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trialCoversModel(tc.status, tc.model); got != tc.want {
				t.Fatalf("trialCoversModel(%v, %q) = %v, want %v", tc.status.Models, tc.model, got, tc.want)
			}
		})
	}
}

func TestDefaultTrialModels(t *testing.T) {
	resetAutoState()
	models := defaultTrialModels()
	if len(models) != 2 || models[0] != "glm-5.3" || models[1] != "glm-5.3-flash" {
		t.Fatalf("defaultTrialModels = %v, want [glm-5.3 glm-5.3-flash]", models)
	}
	cfg := normalizeConfig(pluginConfig{TrialModels: []string{"  ", ""}})
	if len(cfg.TrialModels) != 2 {
		t.Fatalf("blank trial_models must fall back to defaults, got %v", cfg.TrialModels)
	}
}

func TestTrialModelLike(t *testing.T) {
	for _, value := range []string{"glm-5.3", "GLM-5.3-Flash", "claude-sonnet-4", "gpt-5"} {
		if !trialModelLike(value) {
			t.Fatalf("%q should look like a model", value)
		}
	}
	for _, value := range []string{"", "code", "search-prime", "web reader", "mcp"} {
		if trialModelLike(value) {
			t.Fatalf("%q must not be treated as a model", value)
		}
	}
}

// 检测结果要缓存, 避免每个请求都去问上游; relay 失败后要立刻标记不可用并跳过检测。
func TestTrialStatusCacheTTLAndInvalidation(t *testing.T) {
	resetAutoState()

	var calls int
	var mu sync.Mutex
	fetch := func() trialStatus {
		mu.Lock()
		calls++
		mu.Unlock()
		return trialStatus{Available: true, Models: []string{"glm-5.3"}, CheckedAt: time.Now()}
	}

	status := trialCache.get(time.Minute, fetch)
	if !status.Available {
		t.Fatal("expected available status")
	}
	trialCache.get(time.Minute, fetch)
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1 (TTL should serve the cache)", calls)
	}

	invalidateTrialCache("relay 请求失败: exceed quota limit")
	status = trialCache.get(time.Minute, fetch)
	if status.Available {
		t.Fatal("invalidated status must report the trial as unavailable")
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times after invalidation, want 1 (cooldown must skip detection)", calls)
	}
}

func TestTrialStatusCacheIsSingleFlight(t *testing.T) {
	resetAutoState()

	var calls int
	var mu sync.Mutex
	started := make(chan struct{})
	release := make(chan struct{})
	fetch := func() trialStatus {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release
		return trialStatus{Available: true, CheckedAt: time.Now()}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); trialCache.get(time.Minute, fetch) }()
	<-started
	go func() { defer wg.Done(); trialCache.get(time.Minute, fetch) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1 (concurrent callers must share one detection)", calls)
	}
}

// auto 模式的默认值与非法值兜底
func TestNormalizeConfigAutoDefaults(t *testing.T) {
	cfg := normalizeConfig(pluginConfig{UpstreamMode: "auto"})
	if cfg.AutoFallbackMode != "ultra" {
		t.Fatalf("fallback = %q, want ultra", cfg.AutoFallbackMode)
	}
	if cfg.RelayMaxConcurrency != 1 {
		t.Fatalf("relay concurrency = %d, want 1", cfg.RelayMaxConcurrency)
	}
	if cfg.PlanDetectTTLSeconds != 300 {
		t.Fatalf("plan detect ttl = %d, want 300", cfg.PlanDetectTTLSeconds)
	}

	cfg = normalizeConfig(pluginConfig{UpstreamMode: "auto", AutoFallbackMode: "nonsense", RelayMaxConcurrency: 3, PlanDetectTTLSeconds: 30})
	if cfg.AutoFallbackMode != "ultra" {
		t.Fatalf("invalid fallback = %q, want ultra", cfg.AutoFallbackMode)
	}
	if cfg.RelayMaxConcurrency != 3 {
		t.Fatalf("relay concurrency = %d, want 3", cfg.RelayMaxConcurrency)
	}
	if autoFallbackMode(cfg) != "ultra" {
		t.Fatalf("autoFallbackMode = %q, want ultra", autoFallbackMode(cfg))
	}
	direct := normalizeConfig(pluginConfig{UpstreamMode: "auto", AutoFallbackMode: "bigmodel_direct"})
	if autoFallbackMode(direct) != "bigmodel_direct" {
		t.Fatalf("autoFallbackMode = %q, want bigmodel_direct", autoFallbackMode(direct))
	}
}

func TestAutoFallbackOnErrorDefault(t *testing.T) {
	if !autoFallbackOnError(defaultConfig()) {
		t.Fatal("fallback on error must default to true")
	}
	disabled := false
	cfg := mergeConfig(defaultConfig(), pluginConfig{AutoFallbackOnError: &disabled})
	if autoFallbackOnError(cfg) {
		t.Fatal("explicit false must be honoured")
	}
}

func TestUpstreamStatusLabelAuto(t *testing.T) {
	resetAutoState()
	label := upstreamStatusLabel("auto")
	if label == "" || label == upstreamStatusLabel("relay") {
		t.Fatalf("auto label = %q, want a dedicated label", label)
	}
}

func TestLoadUpstreamStateAcceptsAuto(t *testing.T) {
	resetAutoState()
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	activeConfig.Store(normalizeConfig(cfg))

	if err := saveUpstreamState(&upstreamState{Mode: "auto"}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if got := effectiveUpstreamMode(); got != "auto" {
		t.Fatalf("effectiveUpstreamMode = %q, want auto", got)
	}
	if err := saveUpstreamState(&upstreamState{Mode: "bogus"}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if got := effectiveUpstreamMode(); got == "bogus" {
		t.Fatalf("invalid mode leaked through: %q", got)
	}
}

// 回落只在额度/鉴权/传输类错误上发生, 请求本身写错不回落 (避免重复计费)。
func TestRelayErrorFallbackWorthy(t *testing.T) {
	badRequest := pluginabi.NewError("upstream_error", "zcode plan upstream HTTP 400: bad model", http.StatusBadRequest)
	if relayErrorFallbackWorthy(badRequest) {
		t.Fatal("400 request errors must not trigger a fallback")
	}
	quota := pluginabi.NewError("upstream_error", "zcode plan upstream code 1005: exceed quota limit", http.StatusBadGateway)
	if !relayErrorFallbackWorthy(quota) {
		t.Fatal("quota errors must trigger a fallback")
	}
	auth := pluginabi.NewError("no_credential", "not logged in", http.StatusUnauthorized)
	if !relayErrorFallbackWorthy(auth) {
		t.Fatal("credential errors must trigger a fallback")
	}
	rate := pluginabi.NewError("upstream_error", "HTTP 429", http.StatusTooManyRequests)
	if !relayErrorFallbackWorthy(rate) {
		t.Fatal("429 must trigger a fallback")
	}
	if relayErrorFallbackWorthy(nil) {
		t.Fatal("nil error must not trigger a fallback")
	}
}

// 路由决策: 体验套餐可用且覆盖该模型 → relay; 否则 → 正常渠道。
func TestAutoRouteTarget(t *testing.T) {
	resetAutoState()
	cfg := normalizeConfig(pluginConfig{UpstreamMode: "auto", AutoFallbackMode: "ultra"})

	unavailable := trialStatus{Available: false, Reason: "no plan"}
	if got := autoRouteTarget(cfg, unavailable, "glm-5.3"); got != "ultra" {
		t.Fatalf("unavailable trial routed to %q, want ultra", got)
	}

	coversAll := trialStatus{Available: true, Models: []string{"*"}}
	if got := autoRouteTarget(cfg, coversAll, "glm-5.3"); got != "relay" {
		t.Fatalf("available trial routed to %q, want relay", got)
	}

	partial := trialStatus{Available: true, DefaultModels: true, Models: []string{"glm-5.3", "glm-5.3-flash"}}
	if got := autoRouteTarget(cfg, partial, "glm-5.3-flash"); got != "relay" {
		t.Fatalf("covered model routed to %q, want relay", got)
	}
	if got := autoRouteTarget(cfg, partial, "glm-5.2"); got != "ultra" {
		t.Fatalf("uncovered model routed to %q, want ultra", got)
	}

	direct := normalizeConfig(pluginConfig{UpstreamMode: "auto", AutoFallbackMode: "bigmodel_direct"})
	if got := autoRouteTarget(direct, unavailable, "glm-5.3"); got != "bigmodel_direct" {
		t.Fatalf("fallback routed to %q, want bigmodel_direct", got)
	}
}

// ---- 用本地 stub 上游做 executeAuto 端到端验证 (不联网) ----

type stubUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	models  []string
	calls   int
	release chan struct{} // 非 nil 时阻塞响应, 用于并发测试
	status  int           // 非 0 时用它作为响应码
}

func newStubUpstream(t *testing.T, path string) *stubUpstream {
	t.Helper()
	stub := &stubUpstream{status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &parsed)
		stub.mu.Lock()
		stub.calls++
		stub.models = append(stub.models, parsed.Model)
		release := stub.release
		status := stub.status
		stub.mu.Unlock()

		if release != nil {
			<-release
		}
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":1005,"msg":"exceed quota limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_stub","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	stub.srv = httptest.NewServer(mux)
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *stubUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubUpstream) seenModels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.models))
	copy(out, s.models)
	return out
}

func (s *stubUpstream) block() {
	s.mu.Lock()
	s.release = make(chan struct{})
	s.mu.Unlock()
}

func (s *stubUpstream) unblock() {
	s.mu.Lock()
	if s.release != nil {
		close(s.release)
		s.release = nil
	}
	s.mu.Unlock()
}

// newAutoTestEnv 搭一个 auto 模式环境: relay 与 ultra 都指向本地 stub。
func newAutoTestEnv(t *testing.T) (relayStub, ultraStub *stubUpstream, cfg pluginConfig) {
	t.Helper()
	relayStub = newStubUpstream(t, "/anthropic/v1/messages")
	ultraStub = newStubUpstream(t, "/ultra/v1/messages")

	dir := t.TempDir()
	cfg = normalizeConfig(pluginConfig{
		UpstreamMode:        "auto",
		AutoFallbackMode:    "ultra",
		DataDir:             dir,
		PlanBaseURL:         relayStub.srv.URL + "/anthropic",
		UltraBaseURL:        ultraStub.srv.URL + "/ultra",
		BigmodelAPIKey:      "sk-test-key",
		RelayMaxConcurrency: 1,
		PlanDetectTTLSeconds: 300,
	})
	activeConfig.Store(cfg)
	relayGate = &relayGateKind{}
	autoStats = autoStatsKind{}
	trialCache.mu.Lock()
	trialCache.value = trialStatus{Available: true, DefaultModels: true, Models: []string{"glm-5.3", "glm-5.3-flash"}, CheckedAt: time.Now()}
	trialCache.at = time.Now()
	trialCache.mu.Unlock()
	// relay 路径需要凭据文件
	cred := credential{AccessToken: "stub-token", Provider: "bigmodel"}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw, _ := json.Marshal(cred)
	if err := os.WriteFile(filepath.Join(dir, "credential.json"), raw, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	return relayStub, ultraStub, cfg
}

func autoRequest(model string) *pluginapi.ExecutorRequest {
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 8,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	return &pluginapi.ExecutorRequest{Model: model, Payload: payload}
}

// 覆盖模型 → 走 relay
func TestExecuteAutoRoutesCoveredModelToRelay(t *testing.T) {
	relayStub, ultraStub, _ := newAutoTestEnv(t)
	if _, err := executeAuto(autoRequest("glm-5.3-flash"), currentConfig()); err != nil {
		t.Fatalf("executeAuto: %v", err)
	}
	if relayStub.callCount() != 1 {
		t.Fatalf("relay calls = %d, want 1", relayStub.callCount())
	}
	if ultraStub.callCount() != 0 {
		t.Fatalf("fallback calls = %d, want 0", ultraStub.callCount())
	}
	if relay, _, _, _ := autoStats.snapshot(); relay != 1 {
		t.Fatalf("relayHits = %d, want 1", relay)
	}
}

// 未覆盖模型 → 直接走正常渠道, 不碰 relay
func TestExecuteAutoSkipsRelayForUncoveredModel(t *testing.T) {
	relayStub, ultraStub, _ := newAutoTestEnv(t)
	if _, err := executeAuto(autoRequest("glm-5.2"), currentConfig()); err != nil {
		t.Fatalf("executeAuto: %v", err)
	}
	if relayStub.callCount() != 0 {
		t.Fatalf("relay calls = %d, want 0", relayStub.callCount())
	}
	if ultraStub.callCount() != 1 {
		t.Fatalf("fallback calls = %d, want 1", ultraStub.callCount())
	}
}

// 体验套餐并发已满 → 第二个请求立刻回落正常渠道 (不排队), 且 relay 同刻只有 1 路
func TestExecuteAutoOverflowsToFallbackWhenRelayBusy(t *testing.T) {
	relayStub, ultraStub, _ := newAutoTestEnv(t)
	relayStub.block()

	firstDone := make(chan error, 1)
	go func() {
		_, err := executeAuto(autoRequest("glm-5.3-flash"), currentConfig())
		firstDone <- err
	}()
	// 等第一个请求真正进入 relay
	deadline := time.Now().Add(2 * time.Second)
	for relayStub.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if relayStub.callCount() != 1 {
		t.Fatalf("relay calls = %d, want 1 before overflow", relayStub.callCount())
	}

	if _, err := executeAuto(autoRequest("glm-5.3-flash"), currentConfig()); err != nil {
		t.Fatalf("overflow request: %v", err)
	}
	if got := ultraStub.callCount(); got != 1 {
		t.Fatalf("fallback calls = %d, want 1 (overflow must not queue)", got)
	}
	if _, _, busy, _ := autoStats.snapshot(); busy != 1 {
		t.Fatalf("busyOverflow = %d, want 1", busy)
	}
	if inflight, _ := relayGate.stats(); inflight != 1 {
		t.Fatalf("relay inflight = %d, want 1 (never above the limit)", inflight)
	}

	relayStub.unblock()
	if err := <-firstDone; err != nil {
		t.Fatalf("first request: %v", err)
	}
	if relayStub.callCount() != 1 {
		t.Fatalf("relay calls = %d, want 1 total", relayStub.callCount())
	}
	if inflight, _ := relayGate.stats(); inflight != 0 {
		t.Fatalf("relay inflight = %d, want 0 after release", inflight)
	}
}

// relay 报额度错 → 本次回落正常渠道, 并把体验套餐标记为短期不可用
func TestExecuteAutoFallsBackWhenRelayFails(t *testing.T) {
	relayStub, ultraStub, _ := newAutoTestEnv(t)
	relayStub.mu.Lock()
	relayStub.status = http.StatusBadGateway
	relayStub.mu.Unlock()

	if _, err := executeAuto(autoRequest("glm-5.3-flash"), currentConfig()); err != nil {
		t.Fatalf("executeAuto: %v", err)
	}
	if ultraStub.callCount() != 1 {
		t.Fatalf("fallback calls = %d, want 1", ultraStub.callCount())
	}
	if _, _, _, failover := autoStats.snapshot(); failover != 1 {
		t.Fatalf("relayFailover = %d, want 1", failover)
	}
	trial := currentTrialStatus(currentConfig())
	if trial.Available {
		t.Fatal("trial must be marked unavailable after a relay failure")
	}
}
