package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// upstreamState 是面板切换按钮写入的运行时上游选择; 存在时覆盖 config.yaml 里的静态配置
type upstreamState struct {
	Mode      string `json:"mode"`
	APIKey    string `json:"api_key,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// 四种上游/套餐上下文 (与 ZCode 桌面端/官方口径一致):
//
//	relay           = start plan（免费）· 体验套餐: OAuth JWT → zcode relay
//	ultra           = Ultra 网关: 长期 CodingPlan key → zcode.z.ai/api/v1/ultra/anthropic (1.5x 额度, 扣减≈0.67x)
//	bigmodel_direct = Bigmodel（个人）: GLM Coding Max 订阅 → open.bigmodel.cn 直连
//	auto            = 自动: 检测到体验套餐就优先用它跑它覆盖的模型 (并发 relay_max_concurrency,
//	                  默认 1), 并发占满/套餐不可用/模型不覆盖时回落正常渠道 (ultra 或 bigmodel_direct)
func upstreamStatusLabel(mode string) string {
	switch mode {
	case "bigmodel_direct":
		return "Bigmodel（个人）· GLM Coding Max"
	case "ultra":
		return "Ultra（个人 · 1.5x 额度）· CodingPlan key"
	case "auto":
		return fmt.Sprintf("自动 · 优先体验套餐（relay，并发 %d）→ 回落 %s",
			relayMaxConcurrency(), upstreamStatusLabel(autoFallbackMode(currentConfig())))
	default:
		return "start plan（免费 · 体验套餐）· zcode 中转"
	}
}

func upstreamStatePath() string { return filepath.Join(dataDir(), "upstream.json") }

func loadUpstreamState() *upstreamState {
	raw, err := os.ReadFile(upstreamStatePath())
	if err != nil || len(raw) == 0 {
		return nil
	}
	var st upstreamState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	if st.Mode != "relay" && st.Mode != "bigmodel_direct" && st.Mode != "ultra" && st.Mode != "auto" {
		return nil
	}
	return &st
}

func saveUpstreamState(st *upstreamState) error {
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().Format(time.RFC3339)
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(upstreamStatePath(), payload, 0o600)
}

// effectiveUpstreamMode 优先级: 面板状态文件 > config.yaml > relay
func effectiveUpstreamMode() string {
	if st := loadUpstreamState(); st != nil {
		return st.Mode
	}
	cfg := currentConfig()
	switch strings.TrimSpace(cfg.UpstreamMode) {
	case "bigmodel_direct", "ultra", "auto":
		return strings.TrimSpace(cfg.UpstreamMode)
	}
	return "relay"
}

// effectiveUpstreamKey 直连 key: 状态文件 > config.yaml
func effectiveUpstreamKey() string {
	if st := loadUpstreamState(); st != nil && strings.TrimSpace(st.APIKey) != "" {
		return strings.TrimSpace(st.APIKey)
	}
	return strings.TrimSpace(currentConfig().BigmodelAPIKey)
}

// ---- OAuth 套餐检测 ----

type relayPlanInfo struct {
	PlanID    string `json:"plan_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	Window    string `json:"window,omitempty"`    // 起止时间
	Effective string `json:"effective,omitempty"` // 首个权益生效时间
	Models    string `json:"models,omitempty"`    // 权益模型
}

type directPlanInfo struct {
	Product string `json:"product,omitempty"`
	Status  string `json:"status,omitempty"`
	Valid   string `json:"valid,omitempty"`
}

type upstreamStatus struct {
	Mode         string           `json:"mode"`
	Label        string           `json:"label"`
	DirectKeySet bool             `json:"direct_key_set"`
	RelayPlans   []relayPlanInfo  `json:"relay_plans,omitempty"`
	DirectPlans  []directPlanInfo `json:"direct_plans,omitempty"`
	DetectError  string           `json:"detect_error,omitempty"`
	// 自动模式相关 (mode=auto 时才填充 Trial; 并发/回落字段总是给面板看)
	Trial               *trialStatus `json:"relay_trial,omitempty"`
	RelayInflight       int          `json:"relay_inflight"`
	RelayMaxConcurrency int          `json:"relay_max_concurrency"`
	FallbackMode        string       `json:"fallback_mode,omitempty"`
	FallbackLabel       string       `json:"fallback_label,omitempty"`
	// 路由计数 (累计): 看清 model 到底走了体验套餐还是正常渠道
	RelayHits     int64 `json:"relay_hits"`
	FallbackHits  int64 `json:"fallback_hits"`
	BusyOverflow  int64 `json:"busy_overflow"`
	RelayFailover int64 `json:"relay_failover"`
}

func msToCNSec(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
}

// detectRelayPlans 用 OAuth JWT 查 relay 账单, 返回当前账号在中转侧的套餐
func detectRelayPlans() []relayPlanInfo {
	cred := loadCredential()
	if cred == nil || cred.AccessToken == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, "https://zcode.z.ai/api/v1/zcode-plan/billing/current", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("User-Agent", "Zcode/"+currentConfig().AppVersion)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans []struct {
				PlanID       string `json:"plan_id"`
				Name         string `json:"name"`
				Status       string `json:"status"`
				StartsAt     int64  `json:"starts_at"`
				EndsAt       int64  `json:"ends_at"`
				Entitlements []struct {
					ShowName     string   `json:"show_name"`
					Capabilities []string `json:"capabilities"`
					EffectiveAt  int64    `json:"effective_at"`
				} `json:"entitlements"`
			} `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Code != 0 {
		return nil
	}
	out := make([]relayPlanInfo, 0, len(parsed.Data.Plans))
	for _, p := range parsed.Data.Plans {
		info := relayPlanInfo{
			PlanID: p.PlanID,
			Name:   p.Name,
			Status: p.Status,
		}
		if p.StartsAt > 0 || p.EndsAt > 0 {
			info.Window = msToCNSec(p.StartsAt) + " ~ " + msToCNSec(p.EndsAt)
		}
		for _, e := range p.Entitlements {
			if info.Effective == "" && e.EffectiveAt > 0 {
				info.Effective = msToCNSec(e.EffectiveAt)
			}
			if info.Models == "" && len(e.Capabilities) > 0 {
				info.Models = strings.Join(e.Capabilities, ", ")
			}
		}
		out = append(out, info)
	}
	return out
}

// detectDirectPlans 用 bigmodel token 查订阅列表, 返回 Bigmodel（个人）侧的套餐
func detectDirectPlans() []directPlanInfo {
	cred := loadCredential()
	if cred == nil || cred.BigmodelAccessToken == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, "https://bigmodel.cn/api/biz/subscription/list", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cred.BigmodelAccessToken)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Code int `json:"code"`
		Data []struct {
			ProductName string `json:"productName"`
			Status      string `json:"status"`
			Valid       string `json:"valid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Code != 200 {
		return nil
	}
	out := make([]directPlanInfo, 0, len(parsed.Data))
	for _, s := range parsed.Data {
		out = append(out, directPlanInfo{Product: s.ProductName, Status: s.Status, Valid: s.Valid})
	}
	return out
}

func detectUpstreamStatus() *upstreamStatus {
	cfg := currentConfig()
	st := &upstreamStatus{
		Mode:                effectiveUpstreamMode(),
		DirectKeySet:        effectiveUpstreamKey() != "",
		RelayMaxConcurrency: relayMaxConcurrency(),
		FallbackMode:        autoFallbackMode(cfg),
	}
	st.RelayInflight, _ = relayGate.stats()
	st.RelayHits, st.FallbackHits, st.BusyOverflow, st.RelayFailover = autoStats.snapshot()
	st.Label = upstreamStatusLabel(st.Mode)
	st.FallbackLabel = upstreamStatusLabel(autoFallbackMode(cfg))
	var wgLike []func()
	done := make(chan struct{}, 2)
	wgLike = append(wgLike, func() {
		st.RelayPlans = detectRelayPlans()
		done <- struct{}{}
	}, func() {
		st.DirectPlans = detectDirectPlans()
		done <- struct{}{}
	})
	for _, fn := range wgLike {
		go fn()
	}
	<-done
	<-done
	if st.Mode == "auto" {
		trial := currentTrialStatus(cfg)
		st.Trial = &trial
		if st.Trial.Available && len(st.RelayPlans) == 0 {
			for _, name := range trial.Plans {
				st.RelayPlans = append(st.RelayPlans, relayPlanInfo{Name: name, Status: "可用"})
			}
		}
	}
	return st
}

// handleResourceUpstream: 无参数=状态+切换表单; 带 mode= 参数=执行切换
// (宿主对 resource 路由只转发 GET, 表单用 GET 提交; LAN 信任模型与 reset 一致)
func handleResourceUpstream(query url.Values, body []byte) pluginapi.ManagementResponse {
	jsonMode := jsonRequested(query, body)
	mode := strings.TrimSpace(query.Get("mode"))

	if mode != "" {
		if mode != "relay" && mode != "bigmodel_direct" && mode != "ultra" && mode != "auto" {
			msg := "mode 必须是 auto / relay / ultra / bigmodel_direct"
			if jsonMode {
				return jsonResetResponse(http.StatusBadRequest, false, msg)
			}
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: darkPage("套餐切换", "<h2>参数错误</h2><p>"+msg+"</p>", 0)}
		}
		st := &upstreamState{Mode: mode, APIKey: strings.TrimSpace(query.Get("key"))}
		if st.APIKey == "" {
			st.APIKey = effectiveUpstreamKey()
		}
		if err := saveUpstreamState(st); err != nil {
			msg := "保存失败: " + err.Error()
			if jsonMode {
				return jsonResetResponse(http.StatusInternalServerError, false, msg)
			}
			return pluginapi.ManagementResponse{Body: darkPage("套餐切换", "<h2>❌ "+html.EscapeString(msg)+"</h2>", 0)}
		}
		msg := "已切换到 " + upstreamStatusLabel(mode)
		if jsonMode {
			return jsonResetResponse(http.StatusOK, true, msg)
		}
		back := loginPageURLFor("upstream")
		return pluginapi.ManagementResponse{Body: darkPage("套餐切换", "<h2>✅ "+html.EscapeString(msg)+"</h2><p><a href=\""+html.EscapeString(back)+"\">返回切换页</a></p>", 0)}
	}

	// 状态 + 表单
	current := effectiveUpstreamMode()
	if jsonMode {
		status := detectUpstreamStatus()
		payload, err := json.Marshal(status)
		if err != nil {
			return jsonResetResponse(http.StatusInternalServerError, false, err.Error())
		}
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Body: payload}
	}
	keyHint := "未配置"
	if effectiveUpstreamKey() != "" {
		keyHint = "已配置"
	}
	content := `<h2>套餐切换</h2>
<p>当前: <strong>` + html.EscapeString(upstreamStatusLabel(current)) + `</strong> · CodingPlan key: ` + keyHint + `</p>
<form method="GET" action="` + html.EscapeString(loginPageURLFor("upstream")) + `">
<p><label><input type="radio" name="mode" value="auto"` + checkedIf(current == "auto") + `> 自动 · 优先体验套餐（relay，并发 ` + fmt.Sprintf("%d", relayMaxConcurrency()) + `），占满或不可用时回落正常渠道</label></p>
<p><label><input type="radio" name="mode" value="relay"` + checkedIf(current == "relay") + `> start plan（免费 · 体验套餐）· zcode 中转</label></p>
<p><label><input type="radio" name="mode" value="ultra"` + checkedIf(current == "ultra") + `> Ultra（个人 · 1.5x 额度）· 长期 key 走 ultra 网关</label></p>
<p>CodingPlan API key (留空保留现有): <input name="key" style="background:#2b2d33;color:#e8e8ea;border:1px solid #3a3c42;border-radius:6px;padding:6px 10px;width:80%" autocomplete="off" placeholder="xxxx.yyyy"></p>
<button class="rbtn" type="submit" style="border:0;cursor:pointer">切换</button>
</form>`
	return pluginapi.ManagementResponse{Body: darkPage("套餐切换", content, 0)}
}

// ---- relay/体验套餐 自动调度 ----

// trialStatus 是 relay(体验套餐) 的可用性快照, 决定 auto 模式能不能走 relay
// 以及哪些模型应该走 relay。
type trialStatus struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Plans     []string `json:"plans,omitempty"`
	Models    []string `json:"models,omitempty"`
	// DefaultModels 表示套餐未下发模型清单, 按默认规则 (仅 GLM 系列) 判定覆盖
	DefaultModels bool      `json:"default_models,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

type trialCacheState struct {
	mu       sync.Mutex
	value    trialStatus
	at       time.Time
	fetching bool
	done     chan struct{}
}

var trialCache trialCacheState

// currentTrialStatus 返回缓存的体验套餐状态 (TTL 内不重复请求上游)。
func currentTrialStatus(cfg pluginConfig) trialStatus {
	ttl := time.Duration(cfg.PlanDetectTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return trialCache.get(ttl, fetchTrialStatus)
}

func (s *trialCacheState) get(ttl time.Duration, fetch func() trialStatus) trialStatus {
	s.mu.Lock()
	if !s.at.IsZero() && time.Since(s.at) < ttl {
		value := s.value
		s.mu.Unlock()
		return value
	}
	if s.fetching {
		done := s.done
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		value := s.value
		s.mu.Unlock()
		return value
	}
	s.fetching = true
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	value := fetch()

	s.mu.Lock()
	s.value = value
	s.at = time.Now()
	s.fetching = false
	close(done)
	s.mu.Unlock()
	return value
}

// invalidateTrialCache 把体验套餐标为短期不可用 (relay 报错后避免每个请求都先失败一次)。
func invalidateTrialCache(reason string) {
	trialCache.mu.Lock()
	trialCache.value = trialStatus{Available: false, Reason: reason, CheckedAt: time.Now()}
	trialCache.at = time.Now()
	trialCache.mu.Unlock()
}

func resetTrialCache() {
	trialCache.mu.Lock()
	trialCache.value = trialStatus{}
	trialCache.at = time.Time{}
	trialCache.mu.Unlock()
}

// trialModelLike 判断 entitlement 里的 capability 是不是模型名 (如 glm-5.3),
// 而不是功能标签 (如 code/search)。
func trialModelLike(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" || strings.ContainsAny(lower, " \t") {
		return false
	}
	hasDigit := false
	for _, r := range lower {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return false
	}
	for _, prefix := range []string{"glm", "claude", "gpt", "gemini", "chatglm", "codegeex"} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// fetchTrialStatus 用 OAuth JWT 查 relay 侧套餐, 判断体验套餐是否可用及其覆盖模型。
//
// 判定口径:
//   - 必须已登录 (有 access_token), 否则 relay 无凭据可用
//   - billing/current 返回的 plans 里至少有一个处于生效期
//     (starts_at 已到 且 ends_at 为空或未过期)
//   - 生效套餐的 entitlements[].capabilities 中, 看起来像模型名的那些
//     就是体验套餐覆盖的模型; 一个都识别不出来时, 退回默认清单
//     (trial_models, 默认 glm-5.3 / glm-5.3-flash)
func fetchTrialStatus() trialStatus {
	now := time.Now()
	cred := loadCredential()
	if cred == nil || strings.TrimSpace(cred.AccessToken) == "" {
		return trialStatus{Reason: "未登录 zcode（无 OAuth 凭据）, 无法走体验套餐", CheckedAt: now}
	}
	req, err := http.NewRequest(http.MethodGet, "https://zcode.z.ai/api/v1/zcode-plan/billing/current", nil)
	if err != nil {
		return trialStatus{Reason: err.Error(), CheckedAt: now}
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("User-Agent", "Zcode/"+currentConfig().AppVersion)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return trialStatus{Reason: "套餐检测失败: " + err.Error(), CheckedAt: now}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans []struct {
				PlanID       string `json:"plan_id"`
				Name         string `json:"name"`
				Status       string `json:"status"`
				StartsAt     int64  `json:"starts_at"`
				EndsAt       int64  `json:"ends_at"`
				Entitlements []struct {
					ShowName     string   `json:"show_name"`
					Capabilities []string `json:"capabilities"`
				} `json:"entitlements"`
			} `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return trialStatus{Reason: "套餐返回解析失败", CheckedAt: now}
	}
	if parsed.Code != 0 {
		return trialStatus{Reason: fmt.Sprintf("套餐接口返回 code=%d %s", parsed.Code, parsed.Msg), CheckedAt: now}
	}

	nowSec := now.Unix()
	status := trialStatus{CheckedAt: now}
	modelSet := map[string]struct{}{}
	for _, plan := range parsed.Data.Plans {
		if plan.StartsAt > 0 && plan.StartsAt > nowSec {
			continue // 尚未生效
		}
		if plan.EndsAt > 0 && plan.EndsAt <= nowSec {
			continue // 已过期
		}
		label := strings.TrimSpace(plan.Name)
		if label == "" {
			label = strings.TrimSpace(plan.PlanID)
		}
		if label == "" {
			label = "未知套餐"
		}
		status.Plans = append(status.Plans, label)
		for _, ent := range plan.Entitlements {
			for _, capability := range ent.Capabilities {
				if !trialModelLike(capability) {
					continue
				}
				if name := normalizeModelName(capability); name != "" {
					modelSet[name] = struct{}{}
				}
			}
		}
	}
	if len(status.Plans) == 0 {
		status.Reason = "账号当前没有生效的 relay 套餐"
		return status
	}
	status.Available = true
	for model := range modelSet {
		status.Models = append(status.Models, model)
	}
	sortStrings(status.Models)
	if len(status.Models) == 0 {
		// 套餐没下发模型清单: 默认只把 glm-5.3 / glm-5.3-flash 当成体验套餐覆盖
		status.DefaultModels = true
		status.Models = defaultTrialModels()
	}
	return status
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && strings.ToLower(values[j]) < strings.ToLower(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// trialCoversModel 判断体验套餐是否覆盖该模型。
//
// 体验套餐默认只覆盖 GLM 系列模型 (glm-* / chatglm-* / codegeex-*):
//   - 检测到套餐下发明确模型清单时, 以清单为准
//   - 清单为空时, 不要退化成「全模型」, 否则会把 claude/gpt 类渠道也打去 relay
func trialCoversModel(status trialStatus, model string) bool {
	target := normalizeModelName(model)
	if target == "" {
		return false
	}
	if len(status.Models) == 0 {
		return false
	}
	for _, raw := range status.Models {
		pattern := normalizeModelName(raw)
		if pattern == "" {
			continue
		}
		if pattern == "*" || pattern == target {
			return true
		}
		if strings.ContainsAny(pattern, "*?[") {
			if matched, err := path.Match(pattern, target); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// normalizeModelName 把上游下发的 capability / 模型名归一到可比较形式:
//
//	"model:glm-5.3-flash" -> "glm-5.3-flash"
//	"bigmodel/GLM-5.3"    -> "glm-5.3"
//	"GLM-5.3-Flash"       -> "glm-5.3-flash"
func normalizeModelName(model string) string {
	value := strings.ToLower(strings.TrimSpace(model))
	// 去掉 provider 前缀 (a/b -> b)
	if idx := strings.LastIndex(value, "/"); idx >= 0 && idx < len(value)-1 {
		value = value[idx+1:]
	}
	// 去掉 capability 命名空间前缀 (model:glm-5.3-flash -> glm-5.3-flash),
	// 但只在冒号后缀看起来像模型名时才剔, 避免误伤带冒号的模型 ID
	if idx := strings.LastIndex(value, ":"); idx >= 0 && idx < len(value)-1 {
		if tail := value[idx+1:]; trialModelLike(tail) {
			value = tail
		}
	}
	return value
}

func checkedIf(cond bool) string {
	if cond {
		return " checked"
	}
	return ""
}
