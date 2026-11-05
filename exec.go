package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func configuredModels() []pluginapi.ModelInfo {
	cfg := currentConfig()
	models := make([]pluginapi.ModelInfo, 0, len(cfg.Models))
	for _, id := range cfg.Models {
		if id == "" {
			continue
		}
		models = append(models, pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    pluginID,
			DisplayName:                id,
			Name:                       mappedModel(id, cfg),
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              200000,
			MaxCompletionTokens:        128000,
			UserDefined:                true,
		})
	}
	return models
}

func mappedModel(model string, cfg pluginConfig) string {
	if upstream, ok := cfg.ModelMap[model]; ok && upstream != "" {
		return upstream
	}
	return model
}

// rewriteModel 把客户端请求体里的 model 字段替换为上游名, 其余原样保留
func rewriteModel(payload []byte, model string) []byte {
	if len(payload) == 0 {
		return payload
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return payload
	}
	body["model"] = encoded
	out, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return out
}

func handleExecute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	resp, err := executeUpstream(&req.ExecutorRequest)
	if err != nil {
		return nil, err
	}
	return okEnvelope(resp)
}

func handleExecuteStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	resp, err := executeUpstream(&req.ExecutorRequest)
	if err != nil {
		return nil, err
	}
	chunks := splitSSE(resp.Payload)
	out := rpcExecutorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  make([]pluginapi.ExecutorStreamChunk, 0, len(chunks)),
	}
	for _, chunk := range chunks {
		out.Chunks = append(out.Chunks, pluginapi.ExecutorStreamChunk{Payload: chunk})
	}
	if len(out.Chunks) == 0 {
		out.Chunks = append(out.Chunks, pluginapi.ExecutorStreamChunk{Payload: resp.Payload})
	}
	return okEnvelope(out)
}

func upstreamError(format string, args ...any) error {
	return pluginabi.NewError("upstream_error", fmt.Sprintf(format, args...), http.StatusBadGateway)
}

// upstreamEnvelopeError 识别 zcode 网关的 JSON 错误信封 (code != 0 且无 anthropic type 字段);
// 成功响应要么是 SSE, 要么是 type=message 的 anthropic JSON, 都不会命中
func upstreamEnvelopeError(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var probe struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return ""
	}
	if probe.Code != 0 && probe.Type == "" {
		return fmt.Sprintf("zcode plan upstream code %d: %s", probe.Code, probe.Msg)
	}
	return ""
}

func credentialForRequest(storageJSON []byte) *credential {
	if len(storageJSON) > 0 {
		var cred credential
		if err := json.Unmarshal(storageJSON, &cred); err == nil && cred.AccessToken != "" {
			return &cred
		}
	}
	return loadCredential()
}

// ---- 体验套餐(relay) 并发闸门 + 自动调度 ----

// relayGate 记录 relay 正在处理的请求数。体验套餐只有 1 路并发,
// 超出上限的请求不排队, 直接回落正常渠道。
type relayGateKind struct {
	mu       sync.Mutex
	limit    int
	inflight int
}

var relayGate = &relayGateKind{}

// autoStats 记录 auto 模式的路由结果, 供面板/排查用 (累计值)。
type autoStatsKind struct {
	relayHits      atomic.Int64 // 真正走了体验套餐(relay) 的请求数
	fallbackHits   atomic.Int64 // 因套餐不可用/模型不覆盖而走正常渠道的请求数
	busyOverflow   atomic.Int64 // 因体验套餐并发占满而回落正常渠道的请求数
	relayFallbacks atomic.Int64 // 走了 relay 但报错后回落正常渠道的请求数
}

var autoStats autoStatsKind

func (s *autoStatsKind) snapshot() (relay, fallback, busy, errFallback int64) {
	return s.relayHits.Load(), s.fallbackHits.Load(), s.busyOverflow.Load(), s.relayFallbacks.Load()
}

func (g *relayGateKind) tryAcquire(limit int) bool {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = limit
	if g.inflight >= limit {
		return false
	}
	g.inflight++
	return true
}

func (g *relayGateKind) release() {
	g.mu.Lock()
	if g.inflight > 0 {
		g.inflight--
	}
	g.mu.Unlock()
}

func (g *relayGateKind) stats() (inflight, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight, g.limit
}

// executeByMode 按指定上游执行, auto 模式回落时也走这里。
func executeByMode(req *pluginapi.ExecutorRequest, cfg pluginConfig, mode string) (*pluginapi.ExecutorResponse, error) {
	switch mode {
	case "bigmodel_direct":
		return executeBigmodelDirect(req, cfg)
	case "ultra":
		return executeUltra(req, cfg)
	default:
		return executeRelay(req, cfg)
	}
}

// autoRouteTarget 决定一次请求该走哪个上游。
// mode 为 "relay" 时表示可用体验套餐, 否则为回落渠道 (ultra / bigmodel_direct)。
func autoRouteTarget(cfg pluginConfig, trial trialStatus, model string) string {
	if !trial.Available {
		return autoFallbackMode(cfg)
	}
	if !trialCoversModel(trial, model) {
		return autoFallbackMode(cfg)
	}
	return "relay"
}

// executeAuto: 检测到体验套餐 → 用 relay 跑它覆盖的模型 (并发受限);
// 套餐不可用 / 模型不覆盖 / 并发已满 / relay 报错 → 回落正常渠道。
func executeAuto(req *pluginapi.ExecutorRequest, cfg pluginConfig) (*pluginapi.ExecutorResponse, error) {
	fallback := autoFallbackMode(cfg)
	trial := currentTrialStatus(cfg)
	target := autoRouteTarget(cfg, trial, mappedModel(req.Model, cfg))

	if target != "relay" {
		autoStats.fallbackHits.Add(1)
		return executeByMode(req, cfg, target)
	}
	if !relayGate.tryAcquire(relayMaxConcurrency()) {
		// 体验套餐并发已满: 不排队, 直接走正常渠道
		autoStats.busyOverflow.Add(1)
		return executeByMode(req, cfg, fallback)
	}

	resp, err := executeRelay(req, cfg)
	relayGate.release()
	if err == nil {
		autoStats.relayHits.Add(1)
		return resp, nil
	}
	if !relayErrorFallbackWorthy(err) || !autoFallbackOnError(cfg) {
		return nil, err
	}
	// relay 侧额度/鉴权/5xx 失败: 把体验套餐标为短期不可用, 本次先回落正常渠道。
	invalidateTrialCache("relay 请求失败: " + err.Error())
	if fallbackResp, fallbackErr := executeByMode(req, cfg, fallback); fallbackErr == nil {
		autoStats.relayFallbacks.Add(1)
		return fallbackResp, nil
	}
	return nil, err
}

// relayErrorFallbackWorthy 判断 relay 错误是否值得回落正常渠道。
// 请求本身写错(4xx 但不是额度/鉴权类) 不回落, 否则会对同一请求重复计费。
func relayErrorFallbackWorthy(err error) bool {
	if err == nil {
		return false
	}
	var pluginErr *pluginabi.Error
	if errors.As(err, &pluginErr) {
		switch pluginErr.HTTPStatus {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge:
			return false
		}
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"quota", "exceed", "limit", "plan", "expired", "unauthorized", "forbidden",
		"rate", "too many", "unavailable", "timeout", "connection", "tls", "eof",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	var pluginErr2 *pluginabi.Error
	if errors.As(err, &pluginErr2) {
		status := pluginErr2.HTTPStatus
		return status == 0 || status == http.StatusUnauthorized || status == http.StatusForbidden ||
			status == http.StatusTooManyRequests || status >= 500
	}
	return false
}

func executeUpstream(req *pluginapi.ExecutorRequest) (*pluginapi.ExecutorResponse, error) {
	cfg := currentConfig()
	switch mode := effectiveUpstreamMode(); mode {
	case "auto":
		return executeAuto(req, cfg)
	case "bigmodel_direct":
		return executeBigmodelDirect(req, cfg)
	case "ultra":
		return executeUltra(req, cfg)
	default:
		return executeRelay(req, cfg)
	}
}

// executeRelay 用 OAuth JWT 请求 zcode relay (体验套餐/start plan)。
func executeRelay(req *pluginapi.ExecutorRequest, cfg pluginConfig) (*pluginapi.ExecutorResponse, error) {
	cred := credentialForRequest(req.StorageJSON)
	if cred == nil || cred.AccessToken == "" {
		return nil, pluginabi.NewError("no_credential", "not logged in; open the plugin login resource page first", http.StatusUnauthorized)
	}
	model := mappedModel(req.Model, cfg)
	payload := rewriteModel(req.Payload, model)
	endpoint := cfg.PlanBaseURL + "/v1/messages"

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSecs) * time.Second}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			ok, err := refreshCredential()
			if err != nil {
				return nil, upstreamError("token refresh failed: %v", err)
			}
			if !ok {
				return nil, pluginabi.NewError("credential_expired", "credential expired and no refresh token; re-login required", http.StatusUnauthorized)
			}
			cred = loadCredential()
			if cred == nil || cred.AccessToken == "" {
				return nil, pluginabi.NewError("no_credential", "credential lost after refresh", http.StatusUnauthorized)
			}
		}

		httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("User-Agent", "Zcode/"+cfg.AppVersion)
		httpReq.Header.Set("X-Platform", "win32-x64")
		httpReq.Header.Set("X-App-Id", cfg.AppID)

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = upstreamError("upstream request failed: %v", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			lastErr = nil
			continue // 刷新后重试
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, pluginabi.NewError("upstream_error", fmt.Sprintf("zcode plan upstream HTTP %d: %s", resp.StatusCode, truncate(string(body), 400)), resp.StatusCode)
		}
		// 网关对业务失败也返回 HTTP 200 (如 {"code":1005,"msg":"exceed quota limit"}),
		// 必须在这里拦下, 否则会被翻译成空内容的 200 补全响应
		if msg := upstreamEnvelopeError(body); msg != "" {
			return nil, pluginabi.NewError("upstream_error", msg, http.StatusBadGateway)
		}
		headers := http.Header{}
		for _, key := range []string{"Content-Type", "Request-Id", "X-Request-Id"} {
			if v := resp.Header.Get(key); v != "" {
				headers.Set(key, v)
			}
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
		return &pluginapi.ExecutorResponse{Payload: body, Headers: headers}, nil
	}
	if lastErr == nil {
		lastErr = upstreamError("upstream request failed after retry")
	}
	return nil, lastErr
}

// executeUltra 用长期 CodingPlan API key 请求 zcode Ultra 网关
// (zcode.z.ai/api/v1/ultra/anthropic): 官方 1.5x 额度口径, 实测扣减 ≈0.67x 直连。
// 无需任何 ZCode 客户端头/伪造身份 —— 触发条件就是 长期 key + ultra 路由。
func executeUltra(req *pluginapi.ExecutorRequest, cfg pluginConfig) (*pluginapi.ExecutorResponse, error) {
	key := effectiveUpstreamKey()
	if key == "" {
		return nil, pluginabi.NewError("no_credential", "ultra 模式未配置 CodingPlan API key; 在套餐切换页填入", http.StatusUnauthorized)
	}
	model := mappedModel(req.Model, cfg)
	payload := rewriteModel(req.Payload, model)
	endpoint := strings.TrimRight(cfg.UltraBaseURL, "/") + "/v1/messages"

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSecs) * time.Second}
	// 校园出口抖动, 传输失败重试一次 (与 relay 路径一致)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+key)
		httpReq.Header.Set("x-api-key", key)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("anthropic-version", "2023-06-01")

		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = upstreamError("ultra request failed: %v", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, pluginabi.NewError("upstream_error", fmt.Sprintf("ultra HTTP %d: %s", resp.StatusCode, truncate(string(body), 400)), resp.StatusCode)
		}
		// 网关对业务失败也回 200 信封 ({"code":1005,...}), 拦下透传
		if msg := upstreamEnvelopeError(body); msg != "" {
			return nil, pluginabi.NewError("upstream_error", msg, http.StatusBadGateway)
		}
		headers := http.Header{}
		for _, key := range []string{"Content-Type", "Request-Id", "X-Request-Id"} {
			if v := resp.Header.Get(key); v != "" {
				headers.Set(key, v)
			}
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
		return &pluginapi.ExecutorResponse{Payload: body, Headers: headers}, nil
	}
	return nil, lastErr
}

// executeBigmodelDirect 用 GLM Coding Max 的 API key 直连 open.bigmodel.cn,
// 不经过 zcode relay (relay 被活动套餐门禁遮蔽时的切换路径)
func executeBigmodelDirect(req *pluginapi.ExecutorRequest, cfg pluginConfig) (*pluginapi.ExecutorResponse, error) {
	key := effectiveUpstreamKey()
	if key == "" {
		return nil, pluginabi.NewError("no_credential", "bigmodel_direct 模式未配置 API key; 在切换页填入 GLM Coding Max 的 API key", http.StatusUnauthorized)
	}
	model := mappedModel(req.Model, cfg)
	payload := rewriteModel(req.Payload, model)
	endpoint := strings.TrimRight(cfg.BigmodelDirectURL, "/") + "/v1/messages"

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", key)
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSecs) * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, upstreamError("bigmodel direct request failed: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, pluginabi.NewError("upstream_error", fmt.Sprintf("bigmodel direct HTTP %d: %s", resp.StatusCode, truncate(string(body), 400)), resp.StatusCode)
	}
	headers := http.Header{}
	for _, key := range []string{"Content-Type", "Request-Id", "X-Request-Id"} {
		if v := resp.Header.Get(key); v != "" {
			headers.Set(key, v)
		}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	return &pluginapi.ExecutorResponse{Payload: body, Headers: headers}, nil
}

// splitSSE 把上游 SSE 输出按事件块切开, 每块原样作为一个 chunk
func splitSSE(body []byte) [][]byte {
	if len(body) == 0 {
		return nil
	}
	var chunks [][]byte
	start := 0
	for i := 0; i+1 < len(body); i++ {
		if body[i] == '\n' && body[i+1] == '\n' {
			end := i + 2
			chunks = append(chunks, body[start:end])
			start = end
			i++
		}
	}
	if start < len(body) {
		chunks = append(chunks, body[start:])
	}
	return chunks
}
