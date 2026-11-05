package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type resetCardRef struct {
	ExpireAt int64 `json:"expire_at"`
}

func minExpire(cards []resetCardRef) int64 {
	if len(cards) == 0 {
		return 0
	}
	m := cards[0].ExpireAt
	for _, c := range cards {
		if c.ExpireAt < m {
			m = c.ExpireAt
		}
	}
	return m
}

type resetStatusResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AvailableFiveHourResets []resetCardRef `json:"available_five_hour_resets"`
		AvailableWeekResets     []resetCardRef `json:"available_week_resets"`
		LatestFiveHourResetHistory struct {
			UsedAt int64 `json:"used_at"`
		} `json:"latest_five_hour_reset_history"`
		LatestWeekResetHistory struct {
			UsedAt int64 `json:"used_at"`
		} `json:"latest_week_reset_history"`
	} `json:"data"`
}

// fetchResetStatus GET /api/v1/coding-plan/reset/status (双 token 头)
func fetchResetStatus() (*resetStatusResp, error) {
	cred := loadCredential()
	if cred == nil {
		return nil, fmt.Errorf("未登录")
	}
	cfg := currentConfig()
	u := "https://zcode.z.ai/api/v1/coding-plan/reset/status?app_version=" + url.QueryEscape(cfg.AppVersion) + "&platform=win32-x64"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Bigmodel-Authorization", "Bearer "+cred.BigmodelAccessToken)
	req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	req.Header.Set("User-Agent", "ZCode/"+cfg.AppVersion)
	req.Header.Set("X-App-Id", "00000000")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r resetStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("status code=%d msg=%s", r.Code, r.Msg)
	}
	return &r, nil
}

// useResetCard POST /api/v1/coding-plan/reset/use
func useResetCard(resetType string) (bool, error) {
	cred := loadCredential()
	if cred == nil {
		return false, fmt.Errorf("未登录")
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	body, _ := json.Marshal(map[string]string{
		"idempotency_key": hex.EncodeToString(buf),
		"reset_type":      resetType,
	})
	req, err := http.NewRequest(http.MethodPost, "https://zcode.z.ai/api/v1/coding-plan/reset/use?app_version="+url.QueryEscape(currentConfig().AppVersion), strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Bigmodel-Authorization", "Bearer "+cred.BigmodelAccessToken)
	req.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+currentConfig().AppVersion)
	// 与 reset/status 调用对齐: 缺 X-App-Id/X-Platform 时上游报 3001 parameter error
	req.Header.Set("X-App-Id", "00000000")
	req.Header.Set("X-Platform", "win32-x64")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Used bool `json:"used"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, fmt.Errorf("HTTP %d, 响应解析失败", resp.StatusCode)
	}
	if r.Code != 0 {
		return false, fmt.Errorf("code=%d msg=%s", r.Code, r.Msg)
	}
	return r.Data.Used, nil
}

func resetCardRow(label string, cards []resetCardRef, lastUsed int64) string {
	if len(cards) == 0 {
		return `<div class="drow"><span>` + label + `</span><span class="dnum">无可用</span></div>`
	}
	earliest := cards[0].ExpireAt
	for _, c := range cards {
		if c.ExpireAt < earliest {
			earliest = c.ExpireAt
		}
	}
	last := "从未使用"
	if lastUsed > 0 {
		last = msToCN(lastUsed)
	}
	return fmt.Sprintf(`<div class="drow"><span>%s</span><span class="dnum">%d 张可用 · 最早到期 %s · 最后使用 %s</span></div>`,
		html.EscapeString(label), len(cards), msToCN(earliest), html.EscapeString(last))
}

// resetSection 渲染重置卡区 (失败时优雅降级为提示)
func resetSection() string {
	st, err := fetchResetStatus()
	if err != nil {
		return `<div class="card"><div class="ctitle">重置卡</div><div class="cdesc">状态获取失败: ` + html.EscapeString(err.Error()) + `</div></div>`
	}
	usageURL := loginPageURLFor("quota")
	rows := resetCardRow("5 小时重置卡", st.Data.AvailableFiveHourResets, st.Data.LatestFiveHourResetHistory.UsedAt)
	rows += resetCardRow("每周重置卡", st.Data.AvailableWeekResets, st.Data.LatestWeekResetHistory.UsedAt)
	// 按钮: 跳确认页 (消耗重置卡属不可逆操作, 需二次确认)
	five := loginPageURLFor("reset") + "?type=five_hour"
	week := loginPageURLFor("reset") + "?type=week"
	return `<div class="card"><div class="ctitle">重置卡</div>
<div class="cdesc">消耗一张重置卡，立即重置对应的额度窗口</div>
` + rows + `
<div style="margin-top:12px;display:flex;gap:10px">
  <a class="rbtn" href="` + html.EscapeString(five) + `">⚡ 重置 5 小时窗口</a>
  <a class="rbtn" href="` + html.EscapeString(week) + `">⚡ 重置每周窗口</a>
</div>
<div class="cdesc" style="margin-top:8px">完整数据: <a href="` + html.EscapeString(usageURL) + `">用量面板</a></div>
</div>`
}

func loginPageURLFor(name string) string {
	cfg := currentConfig()
	return cfg.PublicBase + "/v0/resource/plugins/" + pluginID + "/" + name
}

// jsonRequested 识别程序化调用 (usage keeper): query format=json 或 body {"format":"json"}
func jsonRequested(query url.Values, body []byte) bool {
	if query.Get("format") == "json" {
		return true
	}
	var probe struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Format == "json" {
		return true
	}
	return false
}

func jsonResetResponse(statusCode int, ok bool, msg string) pluginapi.ManagementResponse {
	payload, _ := json.Marshal(map[string]any{"ok": ok, "message": msg})
	return pluginapi.ManagementResponse{StatusCode: statusCode, Body: payload}
}

// executeResetCore 是 HTML 流程与管理 API 共用的重置执行核心
func executeResetCore(resetType string) (int, bool, string) {
	if resetType != "five_hour" && resetType != "week" {
		return http.StatusBadRequest, false, "type 必须是 five_hour 或 week"
	}
	used, err := useResetCard(resetType)
	if err != nil {
		return http.StatusBadGateway, false, err.Error()
	}
	_ = used
	label := "5 小时窗口"
	if resetType == "week" {
		label = "每周窗口"
	}
	return http.StatusOK, true, label + "已重置"
}

// handleManagementZcodeReset 管理 API 路由: POST /v0/management/zcode/reset (type 在 body 或 query)
func handleManagementZcodeReset(query url.Values, body []byte) pluginapi.ManagementResponse {
	resetType := strings.TrimSpace(query.Get("type"))
	if resetType == "" {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &probe); err == nil {
			resetType = strings.TrimSpace(probe.Type)
		}
	}
	code, ok, msg := executeResetCore(resetType)
	return jsonResetResponse(code, ok, msg)
}

// handleResourceReset: 无 confirm 时返回确认页; 携带 confirm=RESET 即执行
// (宿主对 plugin resource 路由只转发 GET, 所以确认表单用 method=GET);
// format=json 时全部返回 JSON, 供程序化调用
func handleResourceReset(method string, query url.Values, body []byte) pluginapi.ManagementResponse {
	resetType := query.Get("type")
	if resetType != "five_hour" && resetType != "week" {
		if jsonRequested(query, body) {
			return jsonResetResponse(http.StatusBadRequest, false, "type 必须是 five_hour 或 week")
		}
		return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: darkPage("重置", "<h2>参数错误</h2><p>type 必须是 five_hour 或 week</p>", 0)}
	}
	label := "5 小时窗口"
	if resetType == "week" {
		label = "每周窗口"
	}
	confirmed := strings.TrimSpace(query.Get("confirm")) == "RESET" || strings.Contains(string(body), "RESET")

	if !confirmed {
		if jsonRequested(query, body) {
			return jsonResetResponse(http.StatusBadRequest, false, "缺少确认词 RESET")
		}
		// 确认页 (GET 提交: 宿主只转发 resource GET)
		back := loginPageURLFor("quota")
		content := `<h2>确认重置` + html.EscapeString(label) + `？</h2>
<p>此操作会<strong>立即消耗一张` + html.EscapeString(label) + `重置卡</strong>，不可撤销。</p>
<form method="GET" action="` + html.EscapeString(loginPageURLFor("reset")) + `">
<input type="hidden" name="type" value="` + resetType + `">
<p>输入 <code>RESET</code> 确认: <input name="confirm" style="background:#2b2d33;color:#e8e8ea;border:1px solid #3a3c42;border-radius:6px;padding:6px 10px" autocomplete="off"></p>
<button class="rbtn" type="submit" style="border:0;cursor:pointer">⚡ 执行重置</button>
<a class="rbtn" href="` + html.EscapeString(back) + `" style="margin-left:8px">取消</a>
</form>`
		return pluginapi.ManagementResponse{Body: darkPage("确认重置", content, 0)}
	}

	code, ok, msg := executeResetCore(resetType)
	if jsonRequested(query, body) {
		return jsonResetResponse(code, ok, msg)
	}
	if !ok {
		return pluginapi.ManagementResponse{StatusCode: code, Body: darkPage("重置失败", "<h2>❌ 重置失败</h2><p>"+html.EscapeString(msg)+"</p><p><a href=\""+html.EscapeString(loginPageURLFor("quota"))+"\">返回用量面板</a></p>", 0)}
	}
	return pluginapi.ManagementResponse{Body: darkPage("重置成功", "<h2>✅ "+html.EscapeString(msg)+"</h2><p>额度窗口已刷新。</p><p><a href=\""+html.EscapeString(loginPageURLFor("quota"))+"\">返回用量面板</a></p>", 0)}
}
