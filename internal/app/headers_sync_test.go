package app

import "testing"

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
