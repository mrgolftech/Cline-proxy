package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 请求头组装口径：模拟官方 CLI，所以 User-Agent / X-CLIENT-VERSION /
// X-PLATFORM-VERSION 必须是 **CLI 的版本**，X-PLATFORM 固定 "cli"
// （不是 VS Code 扩展的 4.x，也不是 "terminal"）。
func TestBuildOfficialHeaders(t *testing.T) {
	const cli = "3.0.62"
	const core = "0.0.83"
	h := buildOfficialHeaders(cli, core)

	want := map[string]string{
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
	if len(h) != len(want) {
		t.Fatalf("请求头数量 = %d，期望 %d（多了或少了都说明口径漂了）", len(h), len(want))
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("%s = %q，期望 %q", k, h[k], v)
		}
	}
	// 明确钉死两个曾经配错的点
	if h["X-PLATFORM"] != "cli" {
		t.Errorf("X-PLATFORM = %q，必须是 cli（terminal 是 RuntimeEnv 的值）", h["X-PLATFORM"])
	}
	if h["X-CLIENT-VERSION"] == "4.1.19" || h["User-Agent"] == "Cline/4.1.19" {
		t.Error("请求头里出现了 VS Code 扩展版本号，应为 CLI 版本")
	}
}

// URL 拼接必须补斜杠：曾写成 base+path → ".../mainapps/cli/package.json" → 上游 404。
func TestOfficialVersionURL(t *testing.T) {
	want := "https://raw.githubusercontent.com/cline/cline/main/apps/cli/package.json"
	if got := officialVersionURL("apps/cli/package.json"); got != want {
		t.Fatalf("URL = %q，期望 %q", got, want)
	}
	if got := officialVersionURL("/apps/cli/package.json"); got != want {
		t.Errorf("带前导斜杠时 = %q，期望 %q", got, want)
	}
	if strings.Contains(officialVersionURL("apps/cli/package.json"), "mainapps") {
		t.Fatal("URL 少了斜杠，会 404")
	}
}

// 拉取逻辑用 httptest 覆盖（不依赖外网）：正常 / 404 / 坏 JSON / 缺字段。
func TestFetchOfficialVersion(t *testing.T) {
	orig := clineRawBase
	defer func() { clineRawBase = orig }()

	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
		wantErr bool
	}{
		{"正常", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/apps/cli/package.json") {
				t.Errorf("请求路径 = %q（斜杠拼接有问题）", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"name":"@cline/cli","version":"3.0.62"}`))
		}, "3.0.62", false},
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }, "", true},
		{"坏 JSON", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }, "", true},
		{"缺 version", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"name":"x"}`)) }, "", true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.handler)
		clineRawBase = srv.URL
		got, err := fetchOfficialVersion("apps/cli/package.json")
		srv.Close()
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 期望报错，实际拿到 %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 意外报错 %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: version = %q，期望 %q", c.name, got, c.want)
		}
	}
}
