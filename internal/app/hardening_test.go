package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdminSecurityRemoteDeniedByDefault(t *testing.T) {
	t.Setenv("CLINE_ADMIN_PASSWORD", "")
	req := httptest.NewRequest(http.MethodGet, "http://example.test/admin/", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()

	securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestAdminSecurityRemoteBasicAuth(t *testing.T) {
	t.Setenv("CLINE_ADMIN_USER", "admin")
	t.Setenv("CLINE_ADMIN_PASSWORD", "secret")
	req := httptest.NewRequest(http.MethodGet, "http://example.test/admin/", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.SetBasicAuth("admin", "secret")
	rr := httptest.NewRecorder()

	securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}

func TestAdminSecurityLoopbackAllowedWithoutPassword(t *testing.T) {
	t.Setenv("CLINE_ADMIN_PASSWORD", "")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()

	securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}


func TestAdminSecurityUsesForwardedIPFromLocalProxy(t *testing.T) {
	t.Setenv("CLINE_ADMIN_PASSWORD", "")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	rr := httptest.NewRecorder()

	securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected forwarded remote client to require auth, got %d", rr.Code)
	}
}

func TestAnthropicToolChoiceTranslation(t *testing.T) {
	tests := []struct {
		raw  string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
	}
	for _, tt := range tests {
		out := map[string]any{}
		applyAnthropicToolChoice(out, []byte(tt.raw))
		if out["tool_choice"] != tt.want {
			t.Fatalf("%s: expected %#v, got %#v", tt.raw, tt.want, out["tool_choice"])
		}
	}

	out := map[string]any{}
	applyAnthropicToolChoice(out, []byte(`{"type":"tool","name":"read_file","disable_parallel_tool_use":true}`))
	choice, ok := out["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected object tool_choice, got %#v", out["tool_choice"])
	}
	fn, _ := choice["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Fatalf("expected read_file, got %#v", fn["name"])
	}
	if out["parallel_tool_calls"] != false {
		t.Fatalf("expected parallel_tool_calls=false, got %#v", out["parallel_tool_calls"])
	}
}

func TestResponsesStreamingKeepsParallelToolCallsSeparate(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"test-model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]}}]}`,
		`data: {"model":"test-model","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"search_code","arguments":"{\"query\":\"needle\"}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	upstream := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	rr := httptest.NewRecorder()
	chatStreamToResponses(rr, upstream, nil)
	body := rr.Body.String()

	for _, needle := range []string{
		`"call_id":"call_a"`,
		`"name":"read_file"`,
		`"call_id":"call_b"`,
		`"name":"search_code"`,
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		`"status":"completed"`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("stream output missing %q:\n%s", needle, body)
		}
	}
}

func TestNewClineRequestRebuildsBody(t *testing.T) {
	oldBase := clineAPIBaseURL
	clineAPIBaseURL = "http://example.test/api/v1"
	t.Cleanup(func() { clineAPIBaseURL = oldBase })

	body := []byte(`{"hello":"world"}`)
	for i := 0; i < 2; i++ {
		req, err := newClineRequest("token", "session", body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(body) {
			t.Fatalf("attempt %d: body mismatch: %s", i, got)
		}
	}
}

func TestCallClineAPIFailsOverOn429(t *testing.T) {
	oldBase := clineAPIBaseURL
	oldPoolPath := poolPath
	oldPool := pool
	oldConfig := proxyConfig
	t.Cleanup(func() {
		clineAPIBaseURL = oldBase
		poolPath = oldPoolPath
		pool = oldPool
		proxyConfig = oldConfig
	})

	poolPath = filepath.Join(t.TempDir(), "pool.json")
	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "a", Email: "a@example.test", AccessToken: "workos:first", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"},
		{AccountID: "b", Email: "b@example.test", AccessToken: "workos:second", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active"},
	}}
	proxyConfig = &proxyConfigData{Strategy: "fill", Headers: map[string]string{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer workos:first":
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
		case "Bearer workos:second":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.Error(w, "unexpected auth", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	clineAPIBaseURL = server.URL

	resp, acc, err := callClineAPI(map[string]any{
		"model": "test",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err != nil {
		t.Fatalf("callClineAPI returned error: %v", err)
	}
	defer resp.Body.Close()
	if acc == nil || acc.AccountID != "b" {
		t.Fatalf("expected failover to account b, got %#v", acc)
	}
	if pool.Accounts[0].Status != "cooldown" {
		t.Fatalf("expected first account cooldown, got %s", pool.Accounts[0].Status)
	}
}

func TestStatusWriterPreservesFlusher(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr}
	var _ http.Flusher = sw

	sw.Flush()
	if !rr.Flushed {
		t.Fatal("expected underlying ResponseWriter to be flushed")
	}
	if sw.status != http.StatusOK {
		t.Fatalf("expected status 200 after flush, got %d", sw.status)
	}
}

func TestEffectiveNodesUnboundAllowsDirect(t *testing.T) {
	old := proxyConfig
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{Strategy: "round_robin", Headers: map[string]string{}, Proxies: []ProxyNode{}}
	proxyConfigMu.Unlock()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = old
		proxyConfigMu.Unlock()
	})

	nodes, err := effectiveNodes(&Account{AccountID: "a", Email: "a@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != "" {
		t.Fatalf("unbound account should use direct only, got %#v", nodes)
	}
}

func TestEffectiveNodesMissingBindingFailsClosed(t *testing.T) {
	old := proxyConfig
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{Strategy: "round_robin", Headers: map[string]string{}, Proxies: []ProxyNode{}}
	proxyConfigMu.Unlock()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = old
		proxyConfigMu.Unlock()
	})

	nodes, err := effectiveNodes(&Account{
		AccountID: "a", Email: "a@example.test", Proxies: []string{"missing-node"},
	})
	if err == nil {
		t.Fatalf("expected fail-closed error, got nodes %#v", nodes)
	}
	if len(nodes) != 0 {
		t.Fatalf("bound account must never fall back to direct, got %#v", nodes)
	}
}

func TestEffectiveNodesPreservesAccountOrder(t *testing.T) {
	old := proxyConfig
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{},
		Proxies: []ProxyNode{
			{Name: "a", URL: "http://127.0.0.1:18081"},
			{Name: "b", URL: "http://127.0.0.1:18082"},
		},
	}
	proxyConfigMu.Unlock()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = old
		proxyConfigMu.Unlock()
		clearProxyNodeHealth("http://127.0.0.1:18081")
		clearProxyNodeHealth("http://127.0.0.1:18082")
	})

	nodes, err := effectiveNodes(&Account{
		AccountID: "a", Email: "a@example.test", Proxies: []string{"b", "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://127.0.0.1:18082", "http://127.0.0.1:18081"}
	if len(nodes) != len(want) || nodes[0] != want[0] || nodes[1] != want[1] {
		t.Fatalf("expected ordered account exits %#v, got %#v", want, nodes)
	}
}

func TestProxyNodeCircuitBreaker(t *testing.T) {
	const node = "http://127.0.0.1:19090"
	clearProxyNodeHealth(node)
	t.Cleanup(func() { clearProxyNodeHealth(node) })

	recordProxyNodeFailure(node, "first")
	if !proxyNodeAvailable(node) {
		t.Fatal("node should remain available after first failure")
	}
	recordProxyNodeFailure(node, "second")
	if proxyNodeAvailable(node) {
		t.Fatal("node should enter cooldown after threshold")
	}
	if proxyNodeCooldownRemaining(node) <= 0 {
		t.Fatal("expected positive cooldown")
	}
	recordProxyNodeSuccess(node)
	if !proxyNodeAvailable(node) {
		t.Fatal("successful probe should clear circuit breaker")
	}
}

func TestGetProxyConfigReturnsSnapshot(t *testing.T) {
	old := proxyConfig
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{
		Strategy: "fill",
		Headers: map[string]string{"X-Test": "original"},
		Proxies: []ProxyNode{{Name: "a", URL: "http://127.0.0.1:18081"}},
	}
	proxyConfigMu.Unlock()
	t.Cleanup(func() {
		proxyConfigMu.Lock()
		proxyConfig = old
		proxyConfigMu.Unlock()
	})

	snapshot := getProxyConfig()
	snapshot.Headers["X-Test"] = "mutated"
	snapshot.Proxies[0].URL = "http://127.0.0.1:9999"

	again := getProxyConfig()
	if again.Headers["X-Test"] != "original" {
		t.Fatalf("header mutation leaked into global config: %#v", again.Headers)
	}
	if again.Proxies[0].URL != "http://127.0.0.1:18081" {
		t.Fatalf("proxy mutation leaked into global config: %#v", again.Proxies)
	}
}

func TestCallClineAPIMissingBoundNodeNeverDialsDirect(t *testing.T) {
	oldBase := clineAPIBaseURL
	oldPoolPath := poolPath
	oldPool := pool
	oldConfig := proxyConfig
	t.Cleanup(func() {
		clineAPIBaseURL = oldBase
		poolPath = oldPoolPath
		pool = oldPool
		proxyConfigMu.Lock()
		proxyConfig = oldConfig
		proxyConfigMu.Unlock()
	})

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	clineAPIBaseURL = server.URL

	poolPath = filepath.Join(t.TempDir(), "pool.json")
	pool = &AccountPool{Accounts: []*Account{
		{
			AccountID: "a", Email: "a@example.test", AccessToken: "workos:first",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Status: "active",
			Proxies: []string{"deleted-node"},
		},
	}}
	proxyConfigMu.Lock()
	proxyConfig = &proxyConfigData{Strategy: "fill", Headers: map[string]string{}, Proxies: []ProxyNode{}}
	proxyConfigMu.Unlock()

	_, _, err := callClineAPI(map[string]any{
		"model": "test",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, false)
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
	if hits != 0 {
		t.Fatalf("bound account unexpectedly dialed direct upstream %d time(s)", hits)
	}
}
