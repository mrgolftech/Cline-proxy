package app

import (
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// In-memory OAuth login state for async browser login
var (
	oauthSessions   = make(map[string]*oauthSessionState)
	oauthSessionsMu sync.Mutex
)

type oauthSessionState struct {
	DeviceCode string
	UserCode   string
	AuthURL    string
	CreatedAt  time.Time
	Done       bool
	Success    bool
	Email      string
	Error      string
}

type apiResponse struct {
	Success bool        `json:"success"`
	Data    any         `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, resp apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

func registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/", adminStaticHandler)
	mux.HandleFunc("/admin/api/accounts", corsHandler(handleAdminAccounts))
	mux.HandleFunc("/admin/api/accounts/add", corsHandler(handleAdminAccountAdd))
	mux.HandleFunc("/admin/api/accounts/delete", corsHandler(handleAdminAccountDelete))
	mux.HandleFunc("/admin/api/accounts/test", corsHandler(handleAdminAccountTest))
	mux.HandleFunc("/admin/api/accounts/proxy", corsHandler(handleAdminAccountProxy))
	mux.HandleFunc("/admin/api/proxies/add", corsHandler(handleAdminProxyAdd))
	mux.HandleFunc("/admin/api/proxies/delete", corsHandler(handleAdminProxyDelete))
	mux.HandleFunc("/admin/api/proxies/test", corsHandler(handleAdminProxyTest))
	mux.HandleFunc("/admin/api/oauth/start", corsHandler(handleOAuthStart))
	mux.HandleFunc("/admin/api/oauth/status", corsHandler(handleOAuthStatus))
	mux.HandleFunc("/admin/api/sso/import", corsHandler(handleSSOImport))
	mux.HandleFunc("/admin/api/stats", corsHandler(handleAdminStats))
	mux.HandleFunc("/admin/api/batch-import", corsHandler(handleBatchImport))
	mux.HandleFunc("/admin/api/accounts/refresh-all", corsHandler(handleAdminRefreshAll))
	mux.HandleFunc("/admin/api/accounts/delete-all", corsHandler(handleAdminDeleteAll))
	mux.HandleFunc("/admin/api/accounts/reset", corsHandler(handleAdminAccountReset))
	mux.HandleFunc("/admin/api/accounts/export", corsHandler(handleAccountsExport))
	mux.HandleFunc("/admin/api/logs", corsHandler(handleRequestLogs))
	mux.HandleFunc("/admin/api/keys", corsHandler(handleAdminGetKeys))
	mux.HandleFunc("/admin/api/keys/generate", corsHandler(handleAdminGenerateKey))
	mux.HandleFunc("/admin/api/keys/delete", corsHandler(handleAdminDeleteKey))
	mux.HandleFunc("/admin/api/models", corsHandler(handleAdminModels))
	mux.HandleFunc("/admin/api/models/refresh", corsHandler(handleAdminModelsRefresh))
	mux.HandleFunc("/admin/api/config", corsHandler(handleAdminConfig))
	mux.HandleFunc("/admin/api/config/update", corsHandler(handleAdminUpdateConfig))
}

func adminStaticHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/" || r.URL.Path == "/admin" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(adminHTML))
		return
	}
	http.NotFound(w, r)
}

// GET /admin/api/accounts
func handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	accounts := ListAccounts()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"accounts":   accounts,
			"total":      len(accounts),
			"poolIndex":  loadPool().CurrentIdx,
		},
	})
}

// POST /admin/api/accounts/add  body: { refreshToken, email }
func handleAdminAccountAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "refreshToken is required"})
		return
	}

	// Validate by refreshing
	resp, err := cline.RefreshClineToken(req.RefreshToken)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid refreshToken: " + err.Error()})
		return
	}

	if req.Email == "" {
		req.Email = fmt.Sprintf("user_%d", len(loadPool().Accounts)+1)
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        req.Email,
		RefreshToken: req.RefreshToken,
		AccessToken:  "workos:" + resp.Data.AccessToken,
		ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}

	addAccount(acc)
	log.Printf("Account added via API: %s", req.Email)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Account %s added", req.Email),
		Data: map[string]any{
			"accountId": acc.AccountID,
			"email":     acc.Email,
			"status":    acc.Status,
		},
	})
}

// POST /admin/api/accounts/delete  body: { accountId }
func handleAdminAccountDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	if removeAccount(req.AccountID) {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Account deleted"})
	} else {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "Account not found"})
	}
}

// POST /admin/api/oauth/start  -- Start OAuth device login, returns URL
func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	sessionID := fmt.Sprintf("oauth_%d", time.Now().UnixMilli())
	state := &oauthSessionState{
		DeviceCode: device.DeviceCode,
		UserCode:   device.UserCode,
		AuthURL:    authURL,
		CreatedAt:  time.Now(),
	}

	oauthSessionsMu.Lock()
	oauthSessions[sessionID] = state
	oauthSessionsMu.Unlock()

	// Start polling in background
	go func() {
		interval := device.Interval
		if interval < 5 {
			interval = 5
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300
		}

		workosTok, err := cline.PollWorkosToken(device.DeviceCode, interval, expiresIn)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
		if err != nil {
			oauthSessionsMu.Lock()
			state.Error = err.Error()
			state.Done = true
			state.Success = false
			oauthSessionsMu.Unlock()
			return
		}

		email := "unknown"
		if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
			email = reg.Data.UserInfo.Email
		}

		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: reg.Data.RefreshToken,
			AccessToken:  "workos:" + reg.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(reg.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)

		oauthSessionsMu.Lock()
		state.Done = true
		state.Success = true
		state.Email = email
		oauthSessionsMu.Unlock()
		log.Printf("OAuth account added: %s", email)
	}()

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"sessionId":       sessionID,
			"verificationUri": authURL,
			"userCode":        device.UserCode,
		},
	})
}

// GET /admin/api/oauth/status?sessionId=xxx
func handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "sessionId required"})
		return
	}

	oauthSessionsMu.Lock()
	state, ok := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()

	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "session not found"})
		return
	}

	resp := map[string]any{
		"done":    state.Done,
		"success": state.Success,
	}
	if state.Done {
		resp["email"] = state.Email
		if !state.Success {
			resp["error"] = state.Error
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: resp})
}

// POST /admin/api/sso/import  body: { ssoCookies: string, email?: string }
func handleSSOImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		SSOCookies string `json:"ssoCookies"`
		Email      string `json:"email"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if req.SSOCookies == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "ssoCookies is required"})
		return
	}

	// SSO cookies import - try to use WorkOS device auth (requires browser)
	// For direct SSO cookie conversion, we'd need the WorkOS session cookie
	// to exchange for tokens. This is a placeholder that accepts WorkOS session
	// cookies. In practice, users should use OAuth or direct refreshToken.
	//
	// SSO cookie format expected: workos_session=xxx or similar
	lines := strings.Split(req.SSOCookies, "\n")
	imported := 0
	errors := []string{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Try to use the cookie as a refresh token directly (common format)
		if strings.HasPrefix(line, "workos:") || len(line) > 20 {
			token := strings.TrimPrefix(line, "workos:")
			resp, err := cline.RefreshClineToken(token)
			if err != nil {
				errors = append(errors, fmt.Sprintf("token %s...: %v", kit.Truncate(token, 16), err))
				continue
			}
			email := req.Email
			if email == "" {
				email = fmt.Sprintf("sso_user_%d", time.Now().UnixMilli())
			}

			acc := &Account{
				AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
				Email:        email,
				RefreshToken: token,
				AccessToken:  "workos:" + resp.Data.AccessToken,
				ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
				Status:       "active",
				CreatedAt:    time.Now(),
			}
			addAccount(acc)
			imported++
		}
	}

	result := map[string]any{
		"imported": imported,
		"failed":   len(errors),
	}
	if len(errors) > 0 {
		result["errors"] = errors
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data:    result,
	})
}

// POST /admin/api/batch-import  body: { tokens: [{ refreshToken, email }] }
func handleBatchImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []struct {
			RefreshToken string `json:"refreshToken"`
			Email        string `json:"email"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	if len(req.Tokens) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "tokens array is empty"})
		return
	}

	imported := 0
	errors := []string{}

	for _, t := range req.Tokens {
		if t.RefreshToken == "" {
			continue
		}
		resp, err := cline.RefreshClineToken(t.RefreshToken)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.Email, err))
			continue
		}
		email := t.Email
		if email == "" {
			email = fmt.Sprintf("batch_%d", time.Now().UnixMilli())
		}
		acc := &Account{
			AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
			Email:        email,
			RefreshToken: t.RefreshToken,
			AccessToken:  "workos:" + resp.Data.AccessToken,
			ExpiresAt:    cline.ParseExpiry(resp.Data.ExpiresAt) - 60000,
			Status:       "active",
			CreatedAt:    time.Now(),
		}
		addAccount(acc)
		imported++
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: fmt.Sprintf("Imported %d accounts, %d failed", imported, len(errors)),
		Data: map[string]any{
			"imported": imported,
			"failed":   len(errors),
			"errors":   errors,
		},
	})
}

// POST /admin/api/accounts/refresh-all
func handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	accounts := append([]*Account(nil), p.Accounts...)
	poolMu.Unlock()
	for _, a := range accounts {
		if err := refreshAccountToken(a); err != nil {
			log.Printf("Refresh failed for %s: %v", a.Email, err)
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All tokens refreshed"})
}

// POST /admin/api/accounts/delete-all
func handleAdminDeleteAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "All accounts deleted"})
}

// POST /admin/api/accounts/reset  body: { accountId }
// 检测限流并解除：向上游发送探测请求。若上游仍限流（429）则保持冷却，
// 重置无效；若探测成功则清除冷却、恢复正常状态，并重置今日统计。
func handleAdminAccountReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)

	if status == "active" {
		// 探测通过：解除冷却并重置今日统计
		resetTodayUsage(acc)
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: "检测通过：上游未限流，已解除冷却并重置今日统计",
			Data:    result,
		})
		return
	}

	// 仍限流/失效：保持冷却，重置无效
	msg := "上游仍限流，重置无效，保持冷却"
	if status == "expired" {
		msg = "Token 已失效，重置无效"
	} else if status == "error" {
		msg = "探测异常，请稍后重试"
	}
	if until, ok := result["cooldownUntil"].(string); ok && until != "" {
		msg += "（预计恢复 " + until + "）"
	}
	if remaining, ok := result["remaining"].(string); ok && remaining != "" {
		msg += "（剩余 " + remaining + "）"
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: false,
		Message: msg,
		Data:    result,
	})
}

// POST /admin/api/accounts/test  body: { accountId }
// 用指定账号发送一个 max_tokens=1 的极小探测请求，验证该账号是否可用。
// 如果命中 429/INFERENCE_CAP_ERROR，自动标记冷却并返回预计恢复时间。
func handleAdminAccountTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.AccountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	result, status := testAccount(acc)
	reason, _ := result["reason"].(string)
	log.Printf("Test account %s: status=%s reason=%s", truncateEmail(acc.Email), status, reason)

	writeAPI(w, http.StatusOK, apiResponse{
		Success: status == "active",
		Message: status,
		Data:    result,
	})
}

// POST /admin/api/accounts/proxy  body: { accountId, proxies:[节点名], proxy?:"legacy url" }
// 绑定/解绑某账号的出口节点（有序，空=直连）。用于 muse 等地区受限模型 + 账号↔出口IP 绑定。
func handleAdminAccountProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string   `json:"accountId"`
		Proxies   []string `json:"proxies"`
		Proxy     *string  `json:"proxy"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	acc := getAccountByID(req.AccountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	if req.Proxies != nil {
		names := []string{}
		seen := map[string]struct{}{}
		for _, n := range req.Proxies {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if _, ok := poolNodeByName(n); !ok {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown proxy node: " + n})
				return
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			names = append(names, n)
		}
		setAccountProxies(acc, names)
		if req.Proxy == nil {
			setAccountProxy(acc, "")
		}
	}
	if req.Proxy != nil {
		u := strings.TrimSpace(*req.Proxy)
		if u != "" {
			if err := validateProxyURL(u); err != nil {
				writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
				return
			}
		}
		setAccountProxy(acc, u)
	}

	log.Printf("Account %s proxies set to %v", truncateEmail(acc.Email), acc.Proxies)
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "proxy updated",
		Data:    map[string]any{"accountId": acc.AccountID, "proxies": acc.Proxies, "proxy": acc.Proxy},
	})
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return fmt.Errorf("proxy scheme must be http/https/socks5/socks5h")
}

func validProxyNodeName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// POST /admin/api/proxies/add  body: { name, url }
func handleAdminProxyAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	name := strings.TrimSpace(req.Name)
	raw := strings.TrimSpace(req.URL)
	if name == "" || raw == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "name and url are required"})
		return
	}
	if !validProxyNodeName(name) {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "proxy node name may only contain letters, numbers, dot, underscore and hyphen (max 64)"})
		return
	}
	if err := validateProxyURL(raw); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}

	cfg := getProxyConfig()
	replaced := false
	for i := range cfg.Proxies {
		if cfg.Proxies[i].Name == name {
			cfg.Proxies[i].URL = raw
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Proxies = append(cfg.Proxies, ProxyNode{Name: name, URL: raw})
	}
	setProxyConfig(cfg)
	log.Printf("Proxy node %q %s (%s)", name, map[bool]string{true: "updated", false: "added"}[replaced], maskProxyURL(raw))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "proxy added", Data: map[string]any{"proxies": cfg.Proxies}})
}

// POST /admin/api/proxies/delete  body: { name }
func handleAdminProxyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "name is required"})
		return
	}

	cfg := getProxyConfig()
	kept := cfg.Proxies[:0]
	for _, p := range cfg.Proxies {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	cfg.Proxies = kept
	setProxyConfig(cfg)
	log.Printf("Proxy node %q deleted", name)
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "proxy deleted", Data: map[string]any{"proxies": cfg.Proxies}})
}

// POST /admin/api/proxies/test  body: { name }
// 通过该节点出口请求 Cloudflare trace，返回出口 IP 与地区，用于连通性验证。
func handleAdminProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	node, ok := poolNodeByName(strings.TrimSpace(req.Name))
	if !ok {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "proxy node not found"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	probe, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	resp, err := clineClientFor(node.URL).Do(probe)
	if err != nil {
		recordProxyNodeFailure(node.URL, err.Error())
		writeAPI(w, http.StatusOK, apiResponse{Success: false, Message: "connect failed",
			Data: map[string]any{"ok": false, "error": err.Error()}})
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	ip, loc := parseTraceLine(string(b))
	if resp.StatusCode == http.StatusOK {
		recordProxyNodeSuccess(node.URL)
	} else {
		recordProxyNodeFailure(node.URL, fmt.Sprintf("probe HTTP %d", resp.StatusCode))
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "ok",
		Data: map[string]any{"ok": resp.StatusCode == http.StatusOK, "httpStatus": resp.StatusCode, "ip": ip, "loc": loc}})
}

func parseTraceLine(s string) (ip, loc string) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ip="):
			ip = strings.TrimPrefix(line, "ip=")
		case strings.HasPrefix(line, "loc="):
			loc = strings.TrimPrefix(line, "loc=")
		}
	}
	return
}

// testAccount 对单个账号执行轻量探测请求，返回详细结果与最终状态。
// 测试按钮是"升级版重置"：无论账号当前是 active/cooldown/expired，
// 都会尝试刷新 Token 并发起一次真实探测；成功则清除所有异常状态。
// 返回的 status: active / cooldown / expired / error
func testAccount(acc *Account) (map[string]any, string) {
	prevStatus := acc.Status

	token, err := ensureAccountToken(acc)
	if err != nil {
		poolMu.Lock()
		acc.LastReason = "token refresh failed: " + err.Error()
		acc.Status = "expired"
		acc.CooldownUntil = time.Time{}
		savePoolLocked()
		poolMu.Unlock()
		return map[string]any{
			"accountId": acc.AccountID, "email": acc.Email, "status": "expired",
			"reason": acc.LastReason, "prevStatus": prevStatus,
		}, "expired"
	}

	probeModel := getDefaultModel()
	sessionID := fmt.Sprintf("test_%d", time.Now().UnixMilli())
	probeBody := map[string]any{
		"model":            probeModel,
		"max_tokens":       1,
		"session_id":       sessionID,
		"reasoning_effort": defaultReasoningEffort,
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	if modelNeedsStream(probeModel) {
		probeBody["stream"] = true
	}
	bodyJSON, err := json.Marshal(probeBody)
	if err != nil {
		return map[string]any{
			"accountId": acc.AccountID, "email": acc.Email, "status": "error",
			"reason": "marshal probe: " + err.Error(),
		}, "error"
	}

	nodes, err := effectiveNodes(acc)
	if err != nil {
		return map[string]any{
			"accountId": acc.AccountID, "email": acc.Email, "status": "error",
			"reason": err.Error(), "prevStatus": prevStatus,
		}, "error"
	}

	resp, err := tryAccountOverNodes(acc, token, sessionID, bodyJSON, nodes)
	if err != nil {
		switch acc.Status {
		case "cooldown":
			return map[string]any{
				"accountId": acc.AccountID, "email": acc.Email, "status": "cooldown",
				"reason": acc.LastReason,
				"cooldownUntil": acc.CooldownUntil.Format("2006-01-02 15:04:05"),
				"remaining": formatDuration(time.Until(acc.CooldownUntil)),
				"prevStatus": prevStatus,
			}, "cooldown"
		case "expired":
			return map[string]any{
				"accountId": acc.AccountID, "email": acc.Email, "status": "expired",
				"reason": acc.LastReason, "prevStatus": prevStatus,
			}, "expired"
		default:
			return map[string]any{
				"accountId": acc.AccountID, "email": acc.Email, "status": "error",
				"reason": err.Error(), "prevStatus": prevStatus,
			}, "error"
		}
	}
	defer resp.Body.Close()

	poolMu.Lock()
	acc.Status = "active"
	acc.LastReason = ""
	acc.CooldownUntil = time.Time{}
	savePoolLocked()
	poolMu.Unlock()

	maskedNodes := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node == "" {
			maskedNodes = append(maskedNodes, "direct")
		} else {
			maskedNodes = append(maskedNodes, maskProxyURL(node))
		}
	}
	return map[string]any{
		"accountId": acc.AccountID, "email": acc.Email, "status": "active",
		"reason": "ok", "httpStatus": resp.StatusCode, "prevStatus": prevStatus,
		"nodes": maskedNodes,
	}, "active"
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if secs > 0 && days == 0 && hours == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// Global proxy config (mutable via API, persisted to disk)
var (
	proxyConfig   = loadProxyConfig()
	proxyConfigMu sync.RWMutex
)

func proxyConfigPath() string { return kit.ResolveDataPath(".proxy-config.json") }

// loadProxyConfig 启动时从磁盘加载持久化配置；文件缺失或字段为空时回落到默认值。
func loadProxyConfig() *proxyConfigData {
	cfg := defaultProxyConfig()
	if data, err := os.ReadFile(proxyConfigPath()); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Printf("proxy config parse failed: %v", err)
		}
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "round_robin"
	}
	if cfg.Headers == nil {
		cfg.Headers = defaultProxyConfig().Headers
	}
	if cfg.Proxies == nil {
		cfg.Proxies = []ProxyNode{}
	}
	return cfg
}

// saveProxyConfigLocked 把当前配置落盘（调用方需已持有 proxyConfigMu）。
func saveProxyConfigLocked() {
	data, _ := json.MarshalIndent(proxyConfig, "", "  ")
	if err := os.WriteFile(proxyConfigPath(), data, 0600); err != nil {
		log.Printf("proxy config save failed: %v", err)
	}
}

type ProxyNode struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
	Proxies  []ProxyNode       `json:"proxies"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/4.1.19",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "4.1.19",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "4.1.19",
			"X-CORE-VERSION":     "0.0.83",
		},
		Proxies: []ProxyNode{},
	}
}

func cloneProxyConfig(src *proxyConfigData) *proxyConfigData {
	if src == nil {
		return defaultProxyConfig()
	}
	out := &proxyConfigData{
		Strategy: src.Strategy,
		Headers:  make(map[string]string, len(src.Headers)),
		Proxies:  append([]ProxyNode(nil), src.Proxies...),
	}
	for k, v := range src.Headers {
		out.Headers[k] = v
	}
	return out
}

func getProxyConfig() *proxyConfigData {
	proxyConfigMu.RLock()
	defer proxyConfigMu.RUnlock()
	return cloneProxyConfig(proxyConfig)
}

func setProxyConfig(next *proxyConfigData) {
	copy := cloneProxyConfig(next)
	proxyConfigMu.Lock()
	proxyConfig = copy
	saveProxyConfigLocked()
	proxyConfigMu.Unlock()
	resetClineClients()
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	p := loadPool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": p.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "generate API key: " + err.Error()})
		return
	}
	key := "cline_" + hex.EncodeToString(raw)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	address := r.Host
	if address == "" {
		address = proxyListenAddress
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address":      address,
		"strategy":     cfg.Strategy,
		"version":      "go-1.1",
		"poolPath":     poolPath,
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
		"proxies":      cfg.Proxies,
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel string            `json:"defaultModel"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	cfg := getProxyConfig()
	changed := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		for k, v := range req.Headers {
			cfg.Headers[k] = v
		}
		changed = true
	}

	if req.DefaultModel != "" {
		initModelsCache()
		modelsMu.Lock()
		_, ok := modelsCache[req.DefaultModel]
		modelsMu.Unlock()
		if !ok {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown model: " + req.DefaultModel})
			return
		}
		setDefaultModel(req.DefaultModel)
		changed = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":     cfg.Strategy,
		"headers":      cfg.Headers,
		"defaultModel": defaultModel,
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	ensureModelsFresh()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   getFreeModels(),
		"lastSync": modelsLastSync,
	}})
}

// POST /admin/api/models/refresh
func handleAdminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initModelsCache()
	modelsMu.Lock()
	syncing := modelsSyncing
	modelsMu.Unlock()
	if syncing {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "sync already running"})
		return
	}
	go syncModelsOnce()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model sync started"})
}

// GET /admin/api/stats
func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	p := loadPool()
	active, cooldown, expired := 0, 0, 0
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
		case "expired":
			expired++
		}
	}

	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"total":    len(p.Accounts),
			"active":   active,
			"cooldown": cooldown,
			"expired":  expired,
			"strategy": "round_robin",
			"version":  "go-1.1",
		},
	})
}

// GET /admin/api/accounts/export 导出全部账号 refreshToken（JSON 文件下载）
func handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	p := loadPool()
	items := make([]map[string]any, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		items = append(items, map[string]any{
			"refreshToken": a.RefreshToken,
			"email":        a.Email,
		})
	}
	data, _ := json.MarshalIndent(items, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cline-accounts-export.json"`)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// GET /admin/api/logs 最近请求日志（对话/调用历史）
func handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	logs := LoadRequestLogs()
	if logs == nil {
		logs = []RequestLog{}
	}
	// 倒序返回（最新在前）
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"logs": logs,
		},
	})
}
