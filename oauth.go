package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type credential struct {
	Provider            string          `json:"provider"`
	AccessToken         string          `json:"access_token"`
	RefreshToken        string          `json:"refresh_token,omitempty"`
	BigmodelAccessToken string          `json:"bigmodel_access_token,omitempty"`
	UserInfo            string          `json:"user_info,omitempty"`
	RawPoll             json.RawMessage `json:"raw_poll,omitempty"`
	ObtainedAt          time.Time       `json:"obtained_at"`
	ExpiresAt           time.Time       `json:"expires_at,omitempty"`
}

var (
	credMu  sync.Mutex
	pending *loginFlow
)

type loginFlow struct {
	PollToken   string
	FlowID      string
	PollURL     string
	Authorize   string
	State       string
	ExpiresAt   time.Time
	IntervalSec int
	NextPollAt  time.Time
}

func dataDir() string {
	cfg := currentConfig()
	dir := cfg.DataDir
	if dir == "" {
		dir = "plugins/zcode-oauth-data"
	}
	return dir
}

func credentialPath() string { return filepath.Join(dataDir(), "credential.json") }

func loadCredential() *credential {
	raw, err := os.ReadFile(credentialPath())
	if err != nil || len(raw) == 0 {
		return nil
	}
	var cred credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil
	}
	if cred.AccessToken == "" {
		return nil
	}
	return &cred
}

func saveCredential(cred *credential) error {
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(credentialPath(), payload, 0o600)
}

func clearCredential() error {
	err := os.Remove(credentialPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// startPollFlow 发起 CLI 轮询登录: 已有未过期的流则复用, 避免重复 init
func startPollFlow() (*loginFlow, error) {
	credMu.Lock()
	existing := pending
	credMu.Unlock()
	if existing != nil && time.Now().Before(existing.ExpiresAt) {
		return existing, nil
	}
	cfg := currentConfig()
	pollToken := randomHex(32)
	initURL := strings.TrimRight(cfg.TokenURL, "/")
	initURL = strings.Replace(initURL, "/oauth/token", "/oauth/cli/init", 1)
	body, _ := json.Marshal(map[string]string{"provider": cfg.Provider})
	req, err := http.NewRequest(http.MethodPost, initURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("init HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID         string  `json:"flow_id"`
			AuthorizeURL   string  `json:"authorize_url"`
			ExpiresAt      float64 `json:"expires_at"`
			PollIntervalSec int    `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("init decode: %w", err)
	}
	if env.Code != 0 || env.Data.FlowID == "" || env.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("init rejected (code=%d msg=%s)", env.Code, env.Msg)
	}

	authorize := env.Data.AuthorizeURL
	if cfg.Provider == defaultProvider {
		// 与官方客户端一致: bigmodel 授权链接的 redirect 参数覆写为桌面中转页
		if u, err := url.Parse(authorize); err == nil {
			relay := cfg.RelayBase + "?redirect=" + url.QueryEscape("zcode://oauth/callback") + "&app_version=" + url.QueryEscape(cfg.AppVersion)
			q := u.Query()
			q.Set("redirect", relay)
			u.RawQuery = q.Encode()
			authorize = u.String()
		}
	}

	flow := &loginFlow{
		PollToken:   pollToken,
		FlowID:      env.Data.FlowID,
		Authorize:   authorize,
		ExpiresAt:   time.Unix(int64(env.Data.ExpiresAt), 0),
		IntervalSec: env.Data.PollIntervalSec,
	}
	if flow.IntervalSec < 1 {
		flow.IntervalSec = 3
	}
	if u, err := url.Parse(env.Data.AuthorizeURL); err == nil {
		flow.State = u.Query().Get("state")
	}
	pollBase := strings.Replace(initURL, "/oauth/cli/init", "/oauth/cli/poll/", 1)
	flow.PollURL = pollBase + url.PathEscape(flow.FlowID)

	credMu.Lock()
	pending = flow
	credMu.Unlock()
	return flow, nil
}

// pollOnce 查询一次; 返回 ready(含凭据) / pending / failed
func pollOnce() (string, *credential, error) {
	credMu.Lock()
	flow := pending
	credMu.Unlock()
	if flow == nil {
		return "none", nil, fmt.Errorf("no pending login")
	}
	if time.Now().After(flow.ExpiresAt) || time.Now().Before(flow.NextPollAt) {
		return "pending", nil, nil
	}
	credMu.Lock()
	flow.NextPollAt = time.Now().Add(time.Duration(flow.IntervalSec) * time.Second)
	credMu.Unlock()

	req, err := http.NewRequest(http.MethodGet, flow.PollURL, nil)
	if err != nil {
		return "failed", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+flow.PollToken)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "pending", nil, nil // 网络抖动, 下次再试
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "pending", nil, nil
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "failed", nil, fmt.Errorf("poll decode: %w", err)
	}
	var detail struct {
		Status   string `json:"status"`
		Token    string `json:"token"`
		User     struct {
			UserID   string `json:"user_id"`
			Name     string `json:"name"`
			Email    string `json:"email"`
		} `json:"user"`
		Bigmodel struct {
			AccessToken  string `json:"access_token"`
			AccessToken2 string `json:"accessToken"`
			RefreshToken string `json:"refresh_token"`
			RefreshTok2  string `json:"refreshToken"`
		} `json:"bigmodel"`
	}
	_ = json.Unmarshal(env.Data, &detail)
	switch {
	case detail.Status == "pending" || (env.Code == 0 && detail.Status == ""):
		return "pending", nil, nil
	case detail.Status == "failed":
		credMu.Lock()
		pending = nil
		credMu.Unlock()
		return "failed", nil, fmt.Errorf("授权失败 (msg=%s)", env.Msg)
	case detail.Status == "ready":
		jwt := detail.Token
		if jwt == "" {
			return "failed", nil, fmt.Errorf("ready 但缺少 token")
		}
		refresh := detail.Bigmodel.RefreshToken
		if refresh == "" {
			refresh = detail.Bigmodel.RefreshTok2
		}
		bmToken := detail.Bigmodel.AccessToken
		if bmToken == "" {
			bmToken = detail.Bigmodel.AccessToken2
		}
		cred := &credential{
			Provider:            currentConfig().Provider,
			AccessToken:         jwt,
			RefreshToken:        refresh,
			BigmodelAccessToken: bmToken,
			UserInfo:            string(detail.User.UserID),
			RawPoll:             env.Data,
			ObtainedAt:          time.Now(),
		}
		if err := saveCredential(cred); err != nil {
			return "failed", nil, err
		}
		// 同步写入 auth-dir, 让调度器立即拥有可用的 auth 记录 (best-effort)
		if payload, err := json.Marshal(map[string]any{
			"Name": authFileName,
			"JSON": json.RawMessage(mustJSON(*cred)),
		}); err == nil {
			_, _ = hostCall("host.auth.save", payload)
		}
		credMu.Lock()
		pending = nil
		credMu.Unlock()
		return "ready", cred, nil
	}
	return "pending", nil, nil
}

func refreshTokenCall(refreshToken string) (*credential, error) {
	cfg := currentConfig()
	body, _ := json.Marshal(map[string]string{
		"provider":      cfg.Provider,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	req, err := http.NewRequest(http.MethodPost, cfg.TokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	candidate := env.Data
	if len(candidate) == 0 {
		candidate = raw
	}
	var td struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(candidate, &td); err != nil || td.AccessToken == "" {
		return nil, fmt.Errorf("refresh payload has no access_token")
	}
	cred := &credential{
		Provider:     cfg.Provider,
		AccessToken:  td.AccessToken,
		RefreshToken: refreshToken,
		ObtainedAt:   time.Now(),
	}
	if td.RefreshToken != "" {
		cred.RefreshToken = td.RefreshToken
	}
	return cred, nil
}

func refreshCredential() (bool, error) {
	cred := loadCredential()
	if cred == nil || cred.RefreshToken == "" {
		return false, nil
	}
	fresh, err := refreshTokenCall(cred.RefreshToken)
	if err != nil {
		return false, err
	}
	return true, saveCredential(fresh)
}

func pageHTML(title, body string, refreshSec int) []byte {
	meta := ""
	if refreshSec > 0 {
		meta = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, refreshSec)
	}
	doc := "<!doctype html><html><head><meta charset=utf-8>" + meta + "<title>" + html.EscapeString(title) +
		"</title><style>body{font-family:system-ui;max-width:680px;margin:60px auto;padding:0 16px;line-height:1.7}a.btn{display:inline-block;background:#1652f0;color:#fff;padding:10px 22px;border-radius:8px;text-decoration:none;font-size:16px}code{background:#f4f4f4;padding:2px 6px;border-radius:4px;word-break:break-all}</style></head><body>" +
		body + "</body></html>"
	return []byte(doc)
}

func loginPageURL() string {
	cfg := currentConfig()
	return cfg.PublicBase + "/v0/resource/plugins/" + pluginID + "/login"
}

// handleResourceLogin: 默认=确保登录流存在并展示授权链接(3秒后自动进入等待页); ?poll=1=轮询+自动刷新
func handleResourceLogin(query url.Values) pluginapi.ManagementResponse {
	flow, err := startPollFlow()
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusBadGateway, Body: pageHTML("发起登录失败", "<h2>发起登录失败</h2><p>"+html.EscapeString(err.Error())+"</p>", 0)}
	}
	_, doPoll := query["poll"]

	authorizeBody := "<p>点击按钮登录你的 bigmodel 账号并完成授权（授权后浏览器可能尝试打开 ZCode 桌面端，不用管，回到本页即可）：</p>" +
		"<p><a class=\"btn\" href=\"" + html.EscapeString(flow.Authorize) + "\">打开 bigmodel 授权页</a></p>" +
		"<p>授权链接（也可复制到别的浏览器打开）：<br><code>" + html.EscapeString(flow.Authorize) + "</code></p>"

	if !doPoll {
		return pluginapi.ManagementResponse{Body: pageHTML("ZCode 登录", "<h2>ZCode bigmodel 登录</h2>"+authorizeBody+"<p>本页 3 秒后自动进入等待状态，无需手动操作…</p>", 3)}
	}

	status, _, pollErr := pollOnce()
	switch status {
	case "ready":
		return pluginapi.ManagementResponse{Body: pageHTML("登录成功", "<h2>✅ 登录成功</h2><p>bigmodel 编程套餐凭据已保存，插件模型现在可用。</p><p>本页面可以关闭。</p>", 0)}
	case "failed":
		msg := ""
		if pollErr != nil {
			msg = pollErr.Error()
		}
		return pluginapi.ManagementResponse{Body: pageHTML("登录失败", "<h2>❌ 登录失败</h2><p>"+html.EscapeString(msg)+"</p><p><a href=\""+html.EscapeString(loginPageURL())+"\">重新开始</a></p>", 0)}
	default:
		return pluginapi.ManagementResponse{Body: pageHTML("等待授权", "<h2>⏳ 等待 bigmodel 授权完成…</h2>"+authorizeBody+"<p>本页每 3 秒自动检测，授权完成后会自动变成登录成功。</p>", 3)}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
