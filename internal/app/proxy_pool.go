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
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var (
	zenHTTPClient  = &http.Client{Transport: buildZenTransport()}
	zenProxyCount  atomic.Uint64
	zenTransportMu sync.Mutex

	zenProxyCooldowns   = map[int]time.Time{} // 代理索引 -> 冷却截止
	zenProxyCooldownsMu sync.Mutex
)

// cooldownZenProxy 标记某出口代理冷却,冷却期内轮询跳过
func cooldownZenProxy(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	if d <= 0 {
		d = 10 * time.Minute
	}
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns[idx] = time.Now().Add(d)
	zenProxyCooldownsMu.Unlock()
}

func zenProxyAvailable(idx int) bool {
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	until, ok := zenProxyCooldowns[idx]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(zenProxyCooldowns, idx)
		return true
	}
	return false
}

func zenProxyCooldownStatus() map[string]string {
	cfg := getZenConfig()
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	out := map[string]string{}
	for idx, until := range zenProxyCooldowns {
		if idx >= 0 && idx < len(cfg.Proxies) {
			if time.Now().Before(until) {
				out[cfg.Proxies[idx]] = until.Format("15:04:05")
			}
		}
	}
	return out
}

// rebuildZenTransport 代理池或配置变化时重建 zen 上游 HTTP 客户端
func rebuildZenTransport() {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	zenHTTPClient = &http.Client{Transport: buildZenTransport()}
}

func getZenHTTPClient() *http.Client {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	return zenHTTPClient
}

// pickZenProxy 按策略选择代理,返回 (代理URL, 索引);无代理返回 ("", -1)。
// 跳过冷却中的代理;全部冷却时返回最早恢复的近似(轮询位)。
// 每次调用递增计数,保证 round_robin 顺序与日志索引一致。
func pickZenProxy() (string, int) {
	cfg := getZenConfig()
	n := len(cfg.Proxies)
	if n == 0 {
		return "", -1
	}
	idx := int(zenProxyCount.Add(1)-1) % n
	switch cfg.ProxyStrategy {
	case "random":
		idx = int(time.Now().UnixNano() % int64(n))
	case "fill":
		idx = 0
	}
	// 冷却跳过:线性探测下一个可用代理
	for i := 0; i < n; i++ {
		if zenProxyAvailable(idx) {
			break
		}
		idx = (idx + 1) % n
	}
	return cfg.Proxies[idx], idx
}

// lastZenProxyIdx 最近一次选择的代理索引(日志用)
func lastZenProxyIdx() int {
	v := int64(zenProxyCount.Load())
	if v <= 0 {
		return -1
	}
	return int((v - 1) % int64(max(1, len(getZenConfig().Proxies))))
}

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

func buildZenTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	t.DialContext = zenDialContext
	// https 走 HTTP/2 + uTLS Chrome 指纹: 完整浏览器指纹(含 h2),避免 Go 原生指纹被 CF 风控
	t.RegisterProtocol("https", zenHTTP2Transport())
	return t
}

func zenHTTP2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, err := zenDialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				raw.Close()
				return nil, err
			}
			uconn := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, utls.HelloChrome_120)
			if err := uconn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
}

func zenDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	p, _ := pickZenProxy()
	if p == "" {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	return dialViaProxy(ctx, p, network, addr)
}

// dialViaProxy 统一拨号:http/https 走 CONNECT,socks5 走 SOCKS5 握手
func dialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h":
		auth := &proxy.Auth{}
		if u.User != nil {
			auth.User = u.User.Username()
			auth.Password, _ = u.User.Password()
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
		// 旧接口无 ctx:包装
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

// clineClientFor 返回走指定出口代理的 HTTP 客户端；proxyURL 为空则用直连的全局客户端。
// 结果按代理 URL 缓存，避免每请求重建连接池。
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

// poolNodeByName 按名称查代理池节点。
func poolNodeByName(name string) (ProxyNode, bool) {
	cfg := getProxyConfig()
	for _, n := range cfg.Proxies {
		if n.Name == name {
			return n, true
		}
	}
	return ProxyNode{}, false
}

// effectiveNodes 返回该账号在指定模型下应依次尝试的出口 URL 列表（有序）。
// 账号绑定的节点名 + (legacy) 单 URL；若模型有出口规则则只允许规则内节点
// （账号未命中规则内节点时，退回规则自身的节点）。空列表 => 返回 [""]（直连）。
func effectiveNodes(acc *Account, model string) []string {
	type ref struct{ name, url string }
	var refs []ref
	if acc != nil {
		for _, nm := range acc.Proxies {
			if n, ok := poolNodeByName(nm); ok {
				refs = append(refs, ref{nm, n.URL})
			}
		}
		if acc.Proxy != "" {
			refs = append(refs, ref{"", acc.Proxy})
		}
	}

	cfg := getProxyConfig()
	if rule := cfg.ModelProxies[model]; len(rule) > 0 {
		allowed := make(map[string]bool, len(rule))
		for _, nm := range rule {
			allowed[nm] = true
		}
		var filtered []ref
		for _, r := range refs {
			if allowed[r.name] {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) == 0 {
			for _, nm := range rule {
				if n, ok := poolNodeByName(nm); ok {
					filtered = append(filtered, ref{nm, n.URL})
				}
			}
		}
		refs = filtered
	}

	urls := make([]string, 0, len(refs))
	for _, r := range refs {
		urls = append(urls, r.url)
	}
	if len(urls) == 0 {
		urls = []string{""}
	}
	return urls
}

// dialHTTPProxy 通过 http(s) 代理建立 CONNECT 隧道
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
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
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
