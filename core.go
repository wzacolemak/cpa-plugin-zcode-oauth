package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	pluginVersion = "0.2.2"
	// pluginID 是 CPA 内部的 provider 名, 必须全局唯一 (官方商店已有 id=zcode 的插件)
	pluginID     = "zcode-oauth"
	activeConfig atomic.Value
)

const (
	defaultTokenURL       = "https://zcode.z.ai/api/v1/oauth/token"
	defaultAuthorize      = "https://bigmodel.cn/login"
	defaultRelayBase      = "https://zcode.z.ai/app/oauth/login"
	defaultPlanBaseURL    = "https://zcode.z.ai/api/v1/zcode-plan/anthropic"
	defaultBigmodelDirect = "https://open.bigmodel.cn/api/anthropic"
	defaultUltraBaseURL   = "https://zcode.z.ai/api/v1/ultra/anthropic"
	defaultAppID          = "zcode"
	defaultProvider       = "bigmodel"
	// defaultAppVersion 必须 >= 3.17: 更低的 UA 版本会被 zcode-plan 风控 3012 拦截
	defaultAppVersion = "3.17.2"
	// defaultPublicBase 故意留空: 本实例对外地址因人而异, 由配置 public_base 指定;
	// 为空时页面里的自引用链接退化为相对路径 (浏览器仍可用)。
	defaultPublicBase = ""
	pluginRepository  = "https://github.com/wzacolemak/cpa-plugin-zcode-oauth"
)

func init() {
	activeConfig.Store(defaultConfig())
}

func main() {}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ModelRegistrar        bool                         `json:"model_registrar"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	ManagementAPI         bool                         `json:"management_api"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type managementRegisterRequest struct {
	BasePath         string `json:"BasePath"`
	ResourceBasePath string `json:"ResourceBasePath"`
}

type pluginConfig struct {
	Provider     string            `yaml:"provider"`
	TokenURL     string            `yaml:"token_url"`
	AuthorizeURL string            `yaml:"authorize_url"`
	RelayBase    string            `yaml:"relay_base"`
	AppID        string            `yaml:"app_id"`
	PublicBase   string            `yaml:"public_base"`
	PlanBaseURL  string            `yaml:"plan_base_url"`
	AppVersion   string            `yaml:"app_version"`
	Models       []string          `yaml:"models"`
	ModelMap     map[string]string `yaml:"model_map"`
	TimeoutSecs  int               `yaml:"timeout_seconds"`
	DataDir      string            `yaml:"data_dir"`
	// UpstreamMode: relay (zcode 中转/体验套餐, OAuth JWT) | ultra (长期 key 走 ultra 网关)
	// | bigmodel_direct (Coding Max key 直连 open.bigmodel.cn)
	// | auto (优先体验套餐 relay, 并发占满/不可用时回落正常渠道)
	UpstreamMode string `yaml:"upstream_mode"`
	// AutoFallbackMode 是 auto 模式的「正常渠道」: ultra | bigmodel_direct。
	// relay 不可用、模型不在体验套餐覆盖范围、或并发已占满时走这里。
	AutoFallbackMode string `yaml:"auto_fallback_mode"`
	// RelayMaxConcurrency 是体验套餐(relay)允许的最大并发; <=0 取 1。
	// 超出的请求不排队, 直接回落正常渠道。
	RelayMaxConcurrency int `yaml:"relay_max_concurrency"`
	// PlanDetectTTLSeconds 是 relay 套餐检测结果的缓存秒数; <=0 取 300。
	PlanDetectTTLSeconds int `yaml:"plan_detect_ttl_seconds"`
	// TrialModels 是体验套餐未下发模型清单时, 默认认为被覆盖的模型。
	// 实测体验套餐主要是 glm-5.3 与 glm-5.3-flash。
	TrialModels []string `yaml:"trial_models"`
	// AutoFallbackOnError 控制 relay 请求失败(额度/鉴权/5xx)时是否立即回落正常渠道, 默认 true。
	AutoFallbackOnError *bool `yaml:"auto_fallback_on_error"`
	// BigmodelAPIKey 是直连模式用的 GLM Coding Max API key
	BigmodelAPIKey string `yaml:"bigmodel_api_key"`
	// BigmodelDirectURL 直连模式的 anthropic 兼容端点
	BigmodelDirectURL string `yaml:"bigmodel_direct_url"`
	// UltraBaseURL Ultra 网关 (长期 CodingPlan key + ultra 路由 = 1.5x 额度口径)
	UltraBaseURL string `yaml:"ultra_base_url"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Provider:     defaultProvider,
		TokenURL:     defaultTokenURL,
		AuthorizeURL: defaultAuthorize,
		RelayBase:    defaultRelayBase,
		AppID:        defaultAppID,
		PublicBase:   defaultPublicBase,
		PlanBaseURL:  defaultPlanBaseURL,
		AppVersion:   defaultAppVersion,
		Models:       []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx", "glm-5.2", "glm-5.1", "glm-5", "flash", "flashx", "5.2", "5.1", "5", "GLM-5.3", "GLM-5.3-Flash"},
		ModelMap: map[string]string{
			"flash":  "glm-5.3-flash",
			"flashx": "glm-5.3-flashx",
			"5.2":    "glm-5.2",
			"5.1":    "glm-5.1",
			"5":      "glm-5",
		},
		TimeoutSecs:          300,
		DataDir:              "plugins/zcode-oauth-data",
		UpstreamMode:         "relay",
		AutoFallbackMode:     "ultra",
		RelayMaxConcurrency:  1,
		PlanDetectTTLSeconds: 300,
		TrialModels:          []string{"glm-5.3", "glm-5.3-flash"},
		BigmodelDirectURL:    defaultBigmodelDirect,
		UltraBaseURL:         defaultUltraBaseURL,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		var decoded pluginConfig
		if err := yaml.Unmarshal(req.ConfigYAML, &decoded); err != nil {
			return err
		}
		cfg = mergeConfig(cfg, decoded)
	}
	activeConfig.Store(normalizeConfig(cfg))
	return nil
}

func mergeConfig(base, override pluginConfig) pluginConfig {
	setStr := func(dst *string, src string) {
		if strings.TrimSpace(src) != "" {
			*dst = strings.TrimSpace(src)
		}
	}
	setStr(&base.Provider, override.Provider)
	setStr(&base.TokenURL, override.TokenURL)
	setStr(&base.AuthorizeURL, override.AuthorizeURL)
	setStr(&base.RelayBase, override.RelayBase)
	setStr(&base.AppID, override.AppID)
	setStr(&base.PublicBase, override.PublicBase)
	setStr(&base.PlanBaseURL, override.PlanBaseURL)
	setStr(&base.AppVersion, override.AppVersion)
	setStr(&base.DataDir, override.DataDir)
	setStr(&base.UpstreamMode, override.UpstreamMode)
	setStr(&base.AutoFallbackMode, override.AutoFallbackMode)
	setStr(&base.BigmodelAPIKey, override.BigmodelAPIKey)
	setStr(&base.BigmodelDirectURL, override.BigmodelDirectURL)
	setStr(&base.UltraBaseURL, override.UltraBaseURL)
	if len(override.Models) > 0 {
		base.Models = override.Models
	}
	if len(override.ModelMap) > 0 {
		base.ModelMap = override.ModelMap
	}
	if override.TimeoutSecs > 0 {
		base.TimeoutSecs = override.TimeoutSecs
	}
	if override.RelayMaxConcurrency > 0 {
		base.RelayMaxConcurrency = override.RelayMaxConcurrency
	}
	if override.PlanDetectTTLSeconds > 0 {
		base.PlanDetectTTLSeconds = override.PlanDetectTTLSeconds
	}
	if len(override.TrialModels) > 0 {
		base.TrialModels = override.TrialModels
	}
	if override.AutoFallbackOnError != nil {
		base.AutoFallbackOnError = override.AutoFallbackOnError
	}
	return base
}

func normalizeConfig(cfg pluginConfig) pluginConfig {
	cfg.Provider = strings.TrimSpace(cfg.Provider)
	if cfg.Provider == "" {
		cfg.Provider = defaultProvider
	}
	cfg.PlanBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PlanBaseURL), "/")
	if cfg.PlanBaseURL == "" {
		cfg.PlanBaseURL = defaultPlanBaseURL
	}
	cfg.PublicBase = strings.TrimRight(strings.TrimSpace(cfg.PublicBase), "/")
	if cfg.PublicBase == "" {
		cfg.PublicBase = defaultPublicBase
	}
	if cfg.TimeoutSecs <= 0 {
		cfg.TimeoutSecs = 300
	}
	if len(cfg.Models) == 0 {
		cfg.Models = defaultConfig().Models
	}
	switch strings.TrimSpace(cfg.AutoFallbackMode) {
	case "bigmodel_direct", "ultra":
		cfg.AutoFallbackMode = strings.TrimSpace(cfg.AutoFallbackMode)
	default:
		cfg.AutoFallbackMode = "ultra"
	}
	if cfg.RelayMaxConcurrency <= 0 {
		cfg.RelayMaxConcurrency = 1
	}
	if cfg.PlanDetectTTLSeconds <= 0 {
		cfg.PlanDetectTTLSeconds = 300
	}
	trimmed := make([]string, 0, len(cfg.TrialModels))
	for _, model := range cfg.TrialModels {
		if value := strings.TrimSpace(model); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	if len(trimmed) == 0 {
		trimmed = defaultConfig().TrialModels
	}
	cfg.TrialModels = trimmed
	return cfg
}

// defaultTrialModels 返回体验套餐的默认模型清单 (glm-5.3 / glm-5.3-flash)。
func defaultTrialModels() []string {
	models := currentConfig().TrialModels
	if len(models) == 0 {
		return defaultConfig().TrialModels
	}
	return models
}

// relayMaxConcurrency 返回体验套餐允许的最大并发 (至少 1)。
func relayMaxConcurrency() int {
	if n := currentConfig().RelayMaxConcurrency; n > 0 {
		return n
	}
	return 1
}

// autoFallbackMode 返回 auto 模式的正常渠道, 只会是 ultra 或 bigmodel_direct。
func autoFallbackMode(cfg pluginConfig) string {
	switch strings.TrimSpace(cfg.AutoFallbackMode) {
	case "bigmodel_direct":
		return "bigmodel_direct"
	default:
		return "ultra"
	}
}

// autoFallbackOnError 报告 relay 失败时是否回落正常渠道 (默认 true)。
func autoFallbackOnError(cfg pluginConfig) bool {
	if cfg.AutoFallbackOnError == nil {
		return true
	}
	return *cfg.AutoFallbackOnError
}

func currentConfig() pluginConfig {
	if cfg, ok := activeConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "ZCode",
			Version:          pluginVersion,
			Author:           "colemak",
			GitHubRepository: pluginRepository,
			Logo:             "https://chat.z.ai/favicon.ico",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "provider", Type: pluginapi.ConfigFieldTypeString, Description: "OAuth provider: bigmodel or zai. Default bigmodel."},
				{Name: "public_base", Type: pluginapi.ConfigFieldTypeString, Description: "Base URL of this CPA instance reachable from the user browser, used for the OAuth resource callback."},
				{Name: "plan_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "ZCode plan Anthropic-compatible upstream base URL."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Fallback/static model IDs."},
				{Name: "model_map", Type: pluginapi.ConfigFieldTypeObject, Description: "Requested model to upstream model mapping."},
				{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Upstream request timeout in seconds."},
				{Name: "data_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory (relative to CPA working dir) for the OAuth credential file."},
				{Name: "upstream_mode", Type: pluginapi.ConfigFieldTypeString, Description: "relay | ultra | bigmodel_direct | auto. auto = prefer the relay trial plan and fall back to auto_fallback_mode."},
				{Name: "auto_fallback_mode", Type: pluginapi.ConfigFieldTypeString, Description: "Normal channel used by auto mode when the trial plan is unavailable/busy: ultra (default) or bigmodel_direct."},
				{Name: "relay_max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Description: "Max concurrent requests on the relay trial plan (default 1). Overflow goes straight to the fallback channel."},
				{Name: "trial_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Models the trial plan covers when the plan API returns no model list. Default: glm-5.3, glm-5.3-flash."},
				{Name: "plan_detect_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "How long the trial plan detection result is cached (default 300)."},
			},
		},
		Capabilities: registrationCapabilities{
			ModelRegistrar:        true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  []string{"anthropic"},
			ExecutorOutputFormats: []string{"anthropic"},
			ManagementAPI:         true,
		},
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: pluginID, Models: configuredModels()})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: configuredModels()})
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: pluginID})
	case pluginabi.MethodAuthIdentifier:
		return handleAuthIdentifier()
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)
	case pluginabi.MethodExecutorExecute:
		return handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecuteStream(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRoutes())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), nil
	}
}

func managementRoutes() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "zcode-oauth/status", Description: "Login status"},
			{Method: http.MethodPost, Path: "zcode-oauth/logout", Description: "Drop stored credential"},
			{Method: http.MethodPost, Path: "zcode-oauth/refresh", Description: "Force token refresh"},
			{Method: http.MethodPost, Path: "zcode-oauth/reset", Description: "Consume a reset card (JSON, management-authenticated)"},
			// 兼容旧路径: 插件 ID 从 zcode 改名为 zcode-oauth, 已部署的调用方 (如 Usage Keeper)
			// 仍在用 /v0/management/zcode/reset, 保留别名避免它们跟着改
			{Method: http.MethodPost, Path: "zcode/reset", Description: "Legacy alias of zcode-oauth/reset"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "login", Description: "ZCode OAuth login (unauthenticated)"},
			{Path: "quota", Description: "Coding Plan usage dashboard (unauthenticated)"},
			{Path: "usage", Description: "Coding Plan usage dashboard (standalone)"},
			{Path: "summary", Description: "Coding Plan usage summary JSON (for panel embedding)"},
			{Path: "reset", Description: "Consume a reset card (confirm required)"},
			{Path: "upstream", Description: "Switch executor upstream: zcode relay or bigmodel direct (unauthenticated)"},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	switch {
	case strings.HasSuffix(req.Path, "/management/zcode-oauth/reset"), strings.HasSuffix(req.Path, "/management/zcode/reset"):
		// 管理路由 (带管理密钥), 供 usage keeper 程序化调用;
		// 后缀必须带 management/ 前缀, 否则会把 resource 的 /reset 确认页也匹配进来。
		// /management/zcode/reset 是改名前的中路径, 作为别名继续保留。
		resp := handleManagementZcodeReset(req.Query, req.Body)
		return managementResponseBytes(resp)
	case strings.HasSuffix(req.Path, "/upstream"):
		resp := handleResourceUpstream(req.Query, req.Body)
		return managementResponseBytes(resp)
	case strings.HasSuffix(req.Path, "/login"):
		resp := handleResourceLogin(req.Query)
		return managementResponseBytes(resp)
	case strings.HasSuffix(req.Path, "/quota") || strings.HasSuffix(req.Path, "/usage"):
		resp := handleResourceQuota()
		return managementResponseBytes(resp)
	case strings.HasSuffix(req.Path, "/summary"):
		payload, err := handleResourceSummary()
		if err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.ManagementResponse{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    payload,
		})
	case strings.HasSuffix(req.Path, "/reset"):
		resp := handleResourceReset(req.Method, req.Query, req.Body)
		return managementResponseBytes(resp)
	case strings.HasSuffix(req.Path, "/status"):
		return managementStatus()
	case strings.HasSuffix(req.Path, "/logout"):
		if err := clearCredential(); err != nil {
			return nil, err
		}
		return []byte(`{"ok":true,"status":"logged_out"}`), nil
	case strings.HasSuffix(req.Path, "/refresh"):
		ok, err := refreshCredential()
		if err != nil {
			return nil, err
		}
		if ok {
			return []byte(`{"ok":true,"status":"refreshed"}`), nil
		}
		return []byte(`{"ok":false,"status":"no_refresh_token"}`), nil
	}
	return nil, fmt.Errorf("unhandled management path %s", req.Path)
}

func managementResponseBytes(resp pluginapi.ManagementResponse) ([]byte, error) {
	return okEnvelope(resp)
}

func managementStatus() ([]byte, error) {
	cred := loadCredential()
	out := map[string]any{"provider": currentConfig().Provider, "logged_in": cred != nil}
	if cred != nil {
		out["obtained_at"] = cred.ObtainedAt.Format(time.RFC3339)
		if !cred.ExpiresAt.IsZero() {
			out["expires_at"] = cred.ExpiresAt.Format(time.RFC3339)
		}
		out["has_refresh"] = cred.RefreshToken != ""
	}
	return json.Marshal(out)
}

func okEnvelope(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: payload})
}

func errorEnvelope(code, message string, status int) []byte {
	payload, err := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	if err != nil {
		payload = []byte(`{"ok":false,"error":{"code":"encode_failed"}}`)
	}
	return payload
}
