package app

import (
	"cline-go-proxy/internal/kit"

	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// dialViaProxy 统一拨号：http/https 走 CONNECT，socks5 走 SOCKS5 握手。
func dialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			a := &proxy.Auth{User: u.User.Username()}
			a.Password, _ = u.User.Password()
			auth = a
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// ---- Cline 账号池：按账号绑定的出口代理构造 HTTP 客户端 ----

var (
	clineClients   = map[string]*http.Client{}
	clineClientsMu sync.Mutex
)

func clineClientFor(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kit.HTTPClient
	}
	clineClientsMu.Lock()
	defer clineClientsMu.Unlock()
	if c, ok := clineClients[proxyURL]; ok {
		return c
	}
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialViaProxy(ctx, proxyURL, network, addr)
	}
	c := &http.Client{Transport: t}
	clineClients[proxyURL] = c
	return c
}

func resetClineClients() {
	clineClientsMu.Lock()
	defer clineClientsMu.Unlock()
	for _, c := range clineClients {
		if t, ok := c.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
	clineClients = map[string]*http.Client{}
}

// ---- 出口节点健康状态 / 熔断 ----

type proxyNodeHealthState struct {
	Failures      int
	CooldownUntil time.Time
	LastError     string
}

var (
	proxyNodeHealth   = map[string]*proxyNodeHealthState{}
	proxyNodeHealthMu sync.Mutex
)

const (
	proxyNodeFailureThreshold = 2
	proxyNodeCooldownBase      = 2 * time.Minute
	proxyNodeCooldownMax       = 10 * time.Minute
)

func proxyNodeAvailable(proxyURL string) bool {
	if proxyURL == "" {
		return true
	}
	proxyNodeHealthMu.Lock()
	defer proxyNodeHealthMu.Unlock()
	st := proxyNodeHealth[proxyURL]
	if st == nil || st.CooldownUntil.IsZero() {
		return true
	}
	if time.Now().After(st.CooldownUntil) {
		delete(proxyNodeHealth, proxyURL)
		return true
	}
	return false
}

func recordProxyNodeSuccess(proxyURL string) {
	if proxyURL == "" {
		return
	}
	proxyNodeHealthMu.Lock()
	delete(proxyNodeHealth, proxyURL)
	proxyNodeHealthMu.Unlock()
}

func recordProxyNodeFailure(proxyURL, reason string) {
	if proxyURL == "" {
		return
	}
	proxyNodeHealthMu.Lock()
	defer proxyNodeHealthMu.Unlock()
	st := proxyNodeHealth[proxyURL]
	if st == nil {
		st = &proxyNodeHealthState{}
		proxyNodeHealth[proxyURL] = st
	}
	st.Failures++
	st.LastError = reason
	if st.Failures >= proxyNodeFailureThreshold {
		multiplier := st.Failures - proxyNodeFailureThreshold + 1
		if multiplier > 5 {
			multiplier = 5
		}
		d := time.Duration(multiplier) * proxyNodeCooldownBase
		if d > proxyNodeCooldownMax {
			d = proxyNodeCooldownMax
		}
		st.CooldownUntil = time.Now().Add(d)
	}
}

func clearProxyNodeHealth(proxyURL string) {
	if proxyURL == "" {
		return
	}
	proxyNodeHealthMu.Lock()
	delete(proxyNodeHealth, proxyURL)
	proxyNodeHealthMu.Unlock()
}

func proxyNodeCooldownRemaining(proxyURL string) time.Duration {
	if proxyURL == "" {
		return 0
	}
	proxyNodeHealthMu.Lock()
	defer proxyNodeHealthMu.Unlock()
	st := proxyNodeHealth[proxyURL]
	if st == nil || st.CooldownUntil.IsZero() {
		return 0
	}
	return time.Until(st.CooldownUntil)
}

func poolNodeByName(name string) (ProxyNode, bool) {
	cfg := getProxyConfig()
	for _, n := range cfg.Proxies {
		if n.Name == name {
			return n, true
		}
	}
	return ProxyNode{}, false
}

// effectiveNodes 只根据账号绑定关系决定出口。
// - 未绑定任何代理的账号：允许直连。
// - 明确绑定了代理的账号：只允许使用绑定出口；节点失效/全部冷却时 fail-closed，绝不回退直连。
func effectiveNodes(acc *Account) ([]string, error) {
	if acc == nil {
		return nil, fmt.Errorf("account is nil")
	}

	hasBinding := len(acc.Proxies) > 0 || strings.TrimSpace(acc.Proxy) != ""
	if !hasBinding {
		return []string{""}, nil
	}

	urls := make([]string, 0, len(acc.Proxies)+1)
	seen := map[string]struct{}{}
	missing := make([]string, 0)

	for _, name := range acc.Proxies {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		node, ok := poolNodeByName(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !proxyNodeAvailable(node.URL) {
			continue
		}
		if _, ok := seen[node.URL]; ok {
			continue
		}
		seen[node.URL] = struct{}{}
		urls = append(urls, node.URL)
	}

	// 兼容旧数据：legacy 单 URL 仍属于账号绑定的一部分，但不再由 UI 维护。
	if legacy := strings.TrimSpace(acc.Proxy); legacy != "" && proxyNodeAvailable(legacy) {
		if _, ok := seen[legacy]; !ok {
			urls = append(urls, legacy)
		}
	}

	if len(urls) > 0 {
		return urls, nil
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("account %s proxy binding invalid: missing node(s): %s", truncateEmail(acc.Email), strings.Join(missing, ", "))
	}
	return nil, fmt.Errorf("account %s has proxy bindings but all bound nodes are in cooldown or unavailable", truncateEmail(acc.Email))
}

// dialHTTPProxy 通过 http(s) 代理建立 CONNECT 隧道。
func dialHTTPProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(rawConn); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawConn.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy CONNECT %s: %s %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return rawConn, nil
}
