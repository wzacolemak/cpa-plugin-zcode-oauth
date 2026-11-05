package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// planAPIBase 由 PlanBaseURL(…/anthropic) 推导出套餐 API 基座
func planAPIBase() string {
	return strings.TrimSuffix(strings.TrimRight(currentConfig().PlanBaseURL, "/"), "/anthropic")
}

func fetchWithToken(url, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "ZCode/"+currentConfig().AppVersion)
	req.Header.Set("X-App-Id", "00000000")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

func fetchCustomerInfo() (json.RawMessage, error) {
	cred := loadCredential()
	if cred == nil || cred.BigmodelAccessToken == "" {
		return nil, fmt.Errorf("凭据中没有 bigmodel access_token，请重新登录")
	}
	return fetchWithToken("https://bigmodel.cn/api/biz/customer/getCustomerInfo", cred.BigmodelAccessToken)
}

type quotaLimit struct {
	Type          string `json:"type"`
	Unit          int    `json:"unit"`
	Number        int    `json:"number"`
	Usage         int64  `json:"usage"`
	CurrentValue  int64  `json:"currentValue"`
	Remaining     int64  `json:"remaining"`
	Percentage    int    `json:"percentage"`
	NextResetTime int64  `json:"nextResetTime"` // 毫秒
	UsageDetails  []struct {
		ModelCode string `json:"modelCode"`
		Usage     int64  `json:"usage"`
	} `json:"usageDetails"`
}

type quotaLimitResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Limits []quotaLimit `json:"limits"`
		Level  string       `json:"level"`
	} `json:"data"`
}

type customerResp struct {
	Code int    `json:"code"`
	Data struct {
		CustomerName string `json:"customerName"`
		PhoneNumber  string `json:"phoneNumber"`
		UserType     string `json:"userType"`
	} `json:"data"`
}

// planAPICall 走 zcode JWT 的套餐 API (zcode.z.ai)
func planAPICall(path string) ([]byte, error) {
	cfg := currentConfig()
	cred := loadCredential()
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("未登录")
	}
	u := planAPIBase() + path + "?app_version=" + url.QueryEscape(cfg.AppVersion) + "&platform=win32-x64"
	return fetchWithToken(u, cred.AccessToken)
}

func msToCN(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
}

func labelFor(l quotaLimit) (title, desc string) {
	switch {
	case l.Type == "TIME_LIMIT":
		return "MCP 每月额度", "Coding Plan MCP 工具的每月共享用量"
	case l.Unit == 3 && l.Number == 5:
		return "每 5 小时使用额度", "当前 5 小时窗口内的模型共享用量"
	case l.Unit == 6:
		return "每周使用额度", "当前每周窗口内的模型共享用量"
	default:
		return fmt.Sprintf("额度 (unit=%d number=%d)", l.Unit, l.Number), "模型共享用量"
	}
}

// windowKey: 卡片对应哪个重置卡窗口
func windowKey(l quotaLimit) string {
	switch {
	case l.Unit == 3 && l.Number == 5:
		return "five_hour"
	case l.Unit == 6:
		return "week"
	default:
		return ""
	}
}

func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	return strings.Join(out, ",")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// quotaCard 单张用量卡; resetInfo 非空时在卡内嵌入对应重置卡行 + 按钮
func quotaCard(l quotaLimit, resetInfo string) string {
	title, desc := labelFor(l)
	var main string
	if l.Type == "TIME_LIMIT" && l.Usage > 0 {
		main = fmt.Sprintf("%s / %s", comma(l.CurrentValue), comma(l.Usage))
	} else {
		main = fmt.Sprintf("%d%%", l.Percentage)
	}
	unit := "次"
	if l.Type == "TIME_LIMIT" {
		unit = ""
	}
	bar := fmt.Sprintf(`<div class="bar"><div class="fill" style="width:%d%%"></div></div>`, min(l.Percentage, 100))

	details := ""
	for _, d := range l.UsageDetails {
		name := map[string]string{
			"search-prime": "联网搜索 MCP",
			"web-reader":   "网页读取 MCP",
			"zread":        "开源仓库 MCP",
		}[d.ModelCode]
		if name == "" {
			name = d.ModelCode
		}
		details += fmt.Sprintf(`<div class="drow"><span>%s</span><span class="dnum">%s</span></div>`,
			html.EscapeString(name), comma(d.Usage))
	}

	reset := ""
	if resetInfo != "" {
		reset = `<div class="rsep"></div><div class="rline">` + resetInfo + `</div>`
	}

	return fmt.Sprintf(`<div class="card">
  <div class="ctitle">%s</div>
  <div class="cdesc">%s</div>
  <div class="cmain">%s<span class="cunit">%s</span><span class="cpct">%d%%</span></div>
  %s
  <div class="creset">重置时间： %s</div>
  %s
  %s
</div>`,
		html.EscapeString(title), html.EscapeString(desc),
		html.EscapeString(main), html.EscapeString(unit), l.Percentage,
		bar, msToCN(l.NextResetTime), details, reset)
}

// orderLimits 按截图顺序排: 每5小时 → 每周 → 其余(MCP)
func orderLimits(limits []quotaLimit) []quotaLimit {
	score := func(l quotaLimit) int {
		switch {
		case l.Unit == 3 && l.Number == 5:
			return 0
		case l.Unit == 6:
			return 1
		default:
			return 2
		}
	}
	out := make([]quotaLimit, len(limits))
	copy(out, limits)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && score(out[j]) < score(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// resetBriefFor 返回某窗口类型的紧凑重置卡信息 (嵌在用量卡内); 获取失败时留空
func resetBriefFor(st *resetStatusResp, stErr error, key string) string {
	if stErr != nil || st == nil {
		return ""
	}
	var count int
	var earliest int64
	label := "5 小时重置卡"
	switch key {
	case "five_hour":
		count = len(st.Data.AvailableFiveHourResets)
		earliest = minExpire(st.Data.AvailableFiveHourResets)
	case "week":
		label = "每周重置卡"
		count = len(st.Data.AvailableWeekResets)
		earliest = minExpire(st.Data.AvailableWeekResets)
	default:
		return ""
	}
	if count == 0 {
		return `<span class="rtag none">` + html.EscapeString(label) + `：无可用</span>`
	}
	return fmt.Sprintf(`<span class="rtag">%s <b>%d</b> 张可用 · 最早到期 %s</span><a class="rbtn small" href="%s?type=%s">⚡ 重置</a>`,
		html.EscapeString(label), count, msToCN(earliest),
		html.EscapeString(loginPageURLFor("reset")), key)
}

// handleResourceQuota: 免认证资源页 /v0/resource/plugins/zcode-oauth/quota
func handleResourceQuota() pluginapi.ManagementResponse {
	cred := loadCredential()
	if cred == nil {
		return pluginapi.ManagementResponse{Body: darkPage("额度", "<h2>尚未登录</h2><p><a href=\""+html.EscapeString(loginPageURL())+"\">先去登录</a></p>", 0)}
	}

	// 重置卡状态 (reset.go 的双 token 头实现)
	st, stErr := fetchResetStatus()

	// 配额
	quotaHTML := ""
	if raw, err := fetchWithToken("https://bigmodel.cn/api/monitor/usage/quota/limit", cred.BigmodelAccessToken); err != nil {
		quotaHTML = `<div class="card"><div class="cdesc">配额获取失败: ` + html.EscapeString(err.Error()) + `</div></div>`
	} else {
		var r quotaLimitResp
		if err := json.Unmarshal(raw, &r); err != nil || len(r.Data.Limits) == 0 {
			quotaHTML = `<div class="card"><div class="cdesc">配额数据解析失败</div></div>`
		} else {
			for _, l := range orderLimits(r.Data.Limits) {
				quotaHTML += quotaCard(l, resetBriefFor(st, stErr, windowKey(l)))
			}
		}
	}

	// 账户信息 (一行小字, 不占独立面板)
	account := ""
	if raw, err := fetchCustomerInfo(); err == nil {
		var c customerResp
		if json.Unmarshal(raw, &c) == nil && c.Data.CustomerName != "" {
			account = html.EscapeString(c.Data.CustomerName)
		}
	}
	subLine := "zcode-oauth (bigmodel)"
	if account != "" {
		subLine += " · " + account
	}

	body := `<div class="subline"><span>` + html.EscapeString(subLine) + `</span><a class="rbtn small" href="javascript:location.reload()">⟳ 刷新</a></div>
<div class="grid">` + quotaHTML + `</div>`

	return pluginapi.ManagementResponse{Body: darkPage("Coding Plan 用量", body, 60)}
}

// darkPage 深色主题页面 (参照智谱面板样式)
func darkPage(title, body string, refreshSec int) []byte {
	meta := ""
	if refreshSec > 0 {
		meta = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, refreshSec)
	}
	doc := `<!doctype html><html><head><meta charset=utf-8>` + meta +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(title) +
		`</title><style>
body{background:#17181c;color:#e8e8ea;font-family:system-ui,'Microsoft YaHei',sans-serif;margin:0;padding:18px;line-height:1.6}
.wrap{max-width:960px;margin:0 auto}
.subline{display:flex;justify-content:space-between;align-items:center;color:#9a9ba1;font-size:13px;margin-bottom:12px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:14px}
.card{background:#232529;border-radius:12px;padding:18px 20px}
.ctitle{font-size:15px;font-weight:600}
.cdesc{color:#9a9ba1;font-size:12.5px;margin:4px 0 12px;min-height:18px}
.cmain{font-size:26px;font-weight:700}
.cunit{font-size:13px;color:#9a9ba1;font-weight:400;margin-left:4px}
.cpct{float:right;font-size:14px;color:#9a9ba1;font-weight:400;margin-top:8px}
.bar{background:#3a3c42;border-radius:6px;height:6px;margin:12px 0;overflow:hidden}
.fill{background:#3b82f6;height:6px;border-radius:6px}
.creset{color:#9a9ba1;font-size:12.5px;margin-top:10px}
.drow{display:flex;justify-content:space-between;font-size:13px;color:#c9cacd;margin-top:6px}
.dnum{font-weight:600}
.rsep{border-top:1px solid #3a3c42;margin:12px -20px 0}
.rline{display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap;margin-top:10px;font-size:12.5px;color:#9a9ba1}
.rtag.none{color:#6f7076}
.foot{margin-top:20px;display:flex;gap:10px;justify-content:flex-end}
.rbtn{background:#2b2d33;color:#e8e8ea;padding:8px 18px;border-radius:8px;text-decoration:none;font-size:14px;display:inline-block}
.rbtn.small{padding:4px 12px;font-size:12.5px;border-radius:6px}
a{color:#6ba3f7}
h2{font-size:18px}
</style></head><body><div class="wrap">` + body + `</div></body></html>`
	return []byte(doc)
}
