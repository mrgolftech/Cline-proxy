package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// 官方 Cline 客户端的请求头同步（后台「🔄 同步官方版本」按钮触发）。
//
// 身份口径：网关模拟的是**官方 CLI**（X-CLIENT-TYPE: cline-cli），所以版本必须跟
// apps/cli/package.json（CLI 自己的版本），平台固定 "cli"——不是 VS Code 扩展的
// 4.x，也不是 "terminal"（后者是 getCliBuildInfo() 给 RuntimeEnv 用的值，不是请求头）。
// 权威依据：apps/cli/src/utils/cline-client-identity.ts
//
//	setClineClientIdentity({ name, version, platform: "cli", platformVersion: version })
//
// 头名/默认值定义：sdk/packages/llms/src/providers/request-headers.ts
// var 而非 const：单元测试要把它指向 httptest 服务器，避免测试依赖外网。
var clineRawBase = "https://raw.githubusercontent.com/cline/cline/main"

// officialVersionURL 拼接官方 raw 文件地址。**必须补斜杠**：
// clineRawBase 结尾没有 "/"，直接拼 "apps/cli/package.json" 会得到
// ".../mainapps/cli/package.json" → GitHub 返回 404（踩过）。
func officialVersionURL(path string) string {
	return clineRawBase + "/" + strings.TrimPrefix(path, "/")
}

// headerSyncResult 同步结果（返回给后台，便于展示「从什么变成了什么」）。
type headerSyncResult struct {
	Changed bool              `json:"changed"`
	CLI     string            `json:"cli"`
	Core    string            `json:"core"`
	Before  map[string]string `json:"before,omitempty"`
	Headers map[string]string `json:"headers"`
}

func fetchOfficialVersion(path string) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(officialVersionURL(path))
	if err != nil {
		return "", fmt.Errorf("拉取 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("拉取 %s 失败: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if v.Version == "" {
		return "", fmt.Errorf("%s 里没有 version 字段", path)
	}
	return v.Version, nil
}

// buildOfficialHeaders 组装官方 CLI 的那套头（与 /root/cline2api/update-headers.sh 同口径）。
func buildOfficialHeaders(cli, core string) map[string]string {
	return map[string]string{
		"User-Agent":         "Cline/" + cli,
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-IS-MULTIROOT":     "false",
		"X-CLIENT-TYPE":      "cline-cli",
		"X-CLIENT-VERSION":   cli,
		"X-PLATFORM":         "cli",
		"X-PLATFORM-VERSION": cli,
		"X-CORE-VERSION":     core,
	}
}

// SyncOfficialHeaders 拉官方 CLI/core 版本 → 组装头 → 与当前生效配置比对 → 有变化才落盘。
// 拉取失败时**不改动任何配置**（宁可保持现状，也不要把请求头写坏）。
func SyncOfficialHeaders() (*headerSyncResult, error) {
	cli, err := fetchOfficialVersion("apps/cli/package.json")
	if err != nil {
		return nil, err
	}
	core, err := fetchOfficialVersion("sdk/packages/core/package.json")
	if err != nil {
		return nil, err
	}
	want := buildOfficialHeaders(cli, core)

	proxyConfigMu.Lock()
	cur := cloneProxyConfig(proxyConfig)
	proxyConfigMu.Unlock()

	changed := false
	for k, v := range want {
		if cur.Headers[k] != v {
			changed = true
			break
		}
	}
	res := &headerSyncResult{Changed: changed, CLI: cli, Core: core, Before: cur.Headers, Headers: want}
	if !changed {
		return res, nil
	}

	updated, err := mutateProxyConfig(func(cfg *proxyConfigData) error {
		if cfg.Headers == nil {
			cfg.Headers = map[string]string{}
		}
		for k, v := range want {
			cfg.Headers[k] = v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Headers = updated.Headers
	log.Printf("  headers sync: cli %s->%s core %s->%s platform=cli",
		cur.Headers["X-CLIENT-VERSION"], cli, cur.Headers["X-CORE-VERSION"], core)
	return res, nil
}

// POST /admin/api/headers/sync
func handleAdminSyncHeaders(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	res, err := SyncOfficialHeaders()
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: err.Error()})
		return
	}
	msg := fmt.Sprintf("已是最新：CLI %s · core %s", res.CLI, res.Core)
	if res.Changed {
		msg = fmt.Sprintf("已更新：CLI %s→%s · core %s→%s · platform=cli",
			res.Before["X-CLIENT-VERSION"], res.CLI, res.Before["X-CORE-VERSION"], res.Core)
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: msg, Data: res})
}
