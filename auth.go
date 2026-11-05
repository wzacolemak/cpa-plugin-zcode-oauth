package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const authRecordID = "zcode-bigmodel"
const authFileName = "zcode.json"

// handleAuthIdentifier: auth.identifier
func handleAuthIdentifier() ([]byte, error) {
	return okEnvelope(map[string]string{"identifier": pluginID})
}

// handleAuthParse: auth.parse — 让调度器把 auth-dir 里的凭据文件识别为本插件的 auth 记录
func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var cred credential
	if err := json.Unmarshal(req.RawJSON, &cred); err != nil || cred.AccessToken == "" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authDataFrom(cred),
	})
}

func authDataFrom(cred credential) pluginapi.AuthData {
	return pluginapi.AuthData{
		Provider:     pluginID,
		ID:           authRecordID,
		FileName:     authFileName,
		Label:        "ZCode bigmodel 编程套餐",
		StorageJSON:  mustJSON(cred),
		NextRefreshAfter: time.Now().Add(30 * time.Minute),
	}
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return out
}

// handleAuthLoginStart: auth.login.start — 框架的 -auth-url 端点/面板登录按钮会调到这里
func handleAuthLoginStart(raw []byte) ([]byte, error) {
	flow, err := startPollFlow()
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  pluginID,
		URL:       flow.Authorize,
		State:     flow.State,
		ExpiresAt: flow.ExpiresAt,
	})
}

// handleAuthLoginPoll: auth.login.poll
func handleAuthLoginPoll(raw []byte) ([]byte, error) {
	status, cred, err := pollOnce()
	switch status {
	case "ready":
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "login complete",
			Auth:    authDataFrom(*cred),
		})
	case "failed":
		msg := "登录失败"
		if err != nil {
			msg = err.Error()
		}
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: msg,
		})
	default:
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户完成 bigmodel 授权",
		})
	}
}

// handleAuthRefresh: auth.refresh — 宿主定期调用, 用 refresh_token 续期
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var cred credential
	if err := json.Unmarshal(req.StorageJSON, &cred); err != nil || cred.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token in stored credential")
	}
	fresh, err := refreshTokenCall(cred.RefreshToken)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             authDataFrom(*fresh),
		NextRefreshAfter: time.Now().Add(30 * time.Minute),
	})
}
