package main

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
)

// summaryWindow 面板渲染一个额度窗口所需的全部数据
type summaryWindow struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Desc        string          `json:"desc,omitempty"`
	Unit        string          `json:"unit,omitempty"`
	Percentage  int             `json:"percentage"`
	CurrentText string          `json:"current_text"` // "250 / 4,000" 或 "6%"
	ResetTime   string          `json:"reset_time"`
	Details     []summaryDetail `json:"details,omitempty"`
	ResetCards  *summaryReset   `json:"reset_cards,omitempty"`
}

type summaryDetail struct {
	Name  string `json:"name"`
	Usage string `json:"usage"`
}

type summaryReset struct {
	Count    int    `json:"count"`
	Earliest string `json:"earliest"`
}

type summaryResp struct {
	LoggedIn      bool            `json:"logged_in"`
	Account       string          `json:"account,omitempty"`
	Level         string          `json:"level,omitempty"`
	Windows       []summaryWindow `json:"windows,omitempty"`
	Error         string          `json:"error,omitempty"`
	UpstreamMode  string          `json:"upstream_mode,omitempty"`
	UpstreamLabel string          `json:"upstream_label,omitempty"`
	DirectKeySet  bool            `json:"direct_key_set"`
	// 自动模式 (mode=auto): 体验套餐检测结果与并发/回落信息
	Trial               *trialStatus `json:"relay_trial,omitempty"`
	RelayInflight       int          `json:"relay_inflight"`
	RelayMaxConcurrency int          `json:"relay_max_concurrency"`
	FallbackMode        string       `json:"fallback_mode,omitempty"`
	FallbackLabel       string       `json:"fallback_label,omitempty"`
	RelayHits           int64        `json:"relay_hits"`
	FallbackHits        int64        `json:"fallback_hits"`
	BusyOverflow        int64        `json:"busy_overflow"`
	RelayFailover       int64        `json:"relay_failover"`
}

// handleResourceSummary: GET /v0/resource/plugins/zcode-oauth/summary → 面板注入用的 JSON
func handleResourceSummary() ([]byte, error) {
	cred := loadCredential()
	if cred == nil {
		out, _ := json.Marshal(summaryResp{LoggedIn: false, Error: "未登录"})
		return out, nil
	}
	out := summaryResp{LoggedIn: true}
	out.UpstreamMode = effectiveUpstreamMode()
	out.UpstreamLabel = upstreamStatusLabel(out.UpstreamMode)
	out.DirectKeySet = effectiveUpstreamKey() != ""
	out.RelayMaxConcurrency = relayMaxConcurrency()
	out.RelayInflight, _ = relayGate.stats()
	out.RelayHits, out.FallbackHits, out.BusyOverflow, out.RelayFailover = autoStats.snapshot()
	out.FallbackMode = autoFallbackMode(currentConfig())
	out.FallbackLabel = upstreamStatusLabel(out.FallbackMode)
	if out.UpstreamMode == "auto" {
		trial := currentTrialStatus(currentConfig())
		out.Trial = &trial
	}

	if raw, err := fetchCustomerInfo(); err == nil {
		var c customerResp
		if json.Unmarshal(raw, &c) == nil {
			out.Account = strings.TrimSpace(c.Data.CustomerName)
		}
	}

	if raw, err := fetchWithToken("https://bigmodel.cn/api/monitor/usage/quota/limit", cred.BigmodelAccessToken); err != nil {
		out.Error = "配额获取失败: " + err.Error()
	} else {
		var r quotaLimitResp
		if err := json.Unmarshal(raw, &r); err == nil {
			out.Level = strings.ToUpper(strings.TrimSpace(r.Data.Level))
			for _, l := range orderLimits(r.Data.Limits) {
				title, desc := labelFor(l)
				unit := "次"
				if l.Type == "TIME_LIMIT" {
					unit = ""
				}
				w := summaryWindow{
					Key:        windowKey(l),
					Title:      title,
					Desc:       desc,
					Unit:       unit,
					Percentage: l.Percentage,
					ResetTime:  msToCN(l.NextResetTime),
				}
				if l.Type == "TIME_LIMIT" && l.Usage > 0 {
					w.CurrentText = fmt.Sprintf("%s / %s", comma(l.CurrentValue), comma(l.Usage))
				} else {
					w.CurrentText = fmt.Sprintf("%d%%", l.Percentage)
				}
				for _, d := range l.UsageDetails {
					name := map[string]string{
						"search-prime": "联网搜索 MCP",
						"web-reader":   "网页读取 MCP",
						"zread":        "开源仓库 MCP",
					}[d.ModelCode]
					if name == "" {
						name = d.ModelCode
					}
					w.Details = append(w.Details, summaryDetail{Name: name, Usage: comma(d.Usage)})
				}
				out.Windows = append(out.Windows, w)
			}
		} else {
			out.Error = "配额解析失败"
		}
	}

	if st, err := fetchResetStatus(); err == nil {
		attach := func(key string, cards []resetCardRef, w *summaryWindow) {
			if w == nil {
				return
			}
			if len(cards) == 0 {
				// 0 张也下发，面板据此显示置灰按钮而不是整行消失
				w.ResetCards = &summaryReset{Count: 0}
				return
			}
			w.ResetCards = &summaryReset{Count: len(cards), Earliest: msToCN(minExpire(cards))}
		}
		for i := range out.Windows {
			switch out.Windows[i].Key {
			case "five_hour":
				attach("five_hour", st.Data.AvailableFiveHourResets, &out.Windows[i])
			case "week":
				attach("week", st.Data.AvailableWeekResets, &out.Windows[i])
			}
		}
	}

	payload, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func titleFor(l quotaLimit) string {
	t, _ := labelFor(l)
	return t
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// quotaResourceURLs 供注入脚本拼绝对地址
func quotaResourceURLs() map[string]string {
	base := strings.TrimRight(currentConfig().PublicBase, "/") + "/v0/resource/plugins/" + pluginID
	return map[string]string{
		"quota": base + "/quota",
		"reset": base + "/reset",
	}
}

var _ = html.EscapeString // 保持导入 (summary 本身是 JSON)
