package app

import (
	"crypto/subtle"
	"net"
	"net/http"
	"os"
	"strings"
)

func isLoopbackRequest(r *http.Request) bool {
	ip := requestClientIP(r)
	return ip != nil && ip.IsLoopback()
}

func requestClientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil || !peer.IsLoopback() {
		return peer
	}

	// Only trust forwarding headers when the direct peer is loopback.
	// This covers local Nginx/NPM without allowing remote clients to spoof XFF.
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip := net.ParseIP(first); ip != nil {
			return ip
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		if ip := net.ParseIP(xrip); ip != nil {
			return ip
		}
	}
	return peer
}

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin") {
			if !authorizeAdminRequest(w, r) {
				return
			}
			if r.URL.Path == "/admin/api/accounts/export" && !isLoopbackRequest(r) &&
				!strings.EqualFold(os.Getenv("CLINE_ALLOW_REMOTE_ACCOUNT_EXPORT"), "true") {
				http.Error(w, "remote account export is disabled", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func authorizeAdminRequest(w http.ResponseWriter, r *http.Request) bool {
	if isLoopbackRequest(r) {
		return true
	}

	password := os.Getenv("CLINE_ADMIN_PASSWORD")
	if password == "" {
		http.Error(w, "remote admin access disabled; set CLINE_ADMIN_PASSWORD to enable", http.StatusForbidden)
		return false
	}
	user := os.Getenv("CLINE_ADMIN_USER")
	if user == "" {
		user = "admin"
	}
	gotUser, gotPass, ok := r.BasicAuth()
	if !ok || !constantTimeEqual(gotUser, user) || !constantTimeEqual(gotPass, password) {
		w.Header().Set("WWW-Authenticate", `Basic realm="cline-proxy admin"`)
		http.Error(w, "admin authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	allowed := strings.TrimSpace(os.Getenv("CLINE_CORS_ORIGIN"))
	switch {
	case origin == "":
		return
	case allowed != "" && origin == allowed:
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	case strings.HasPrefix(origin, "http://127.0.0.1:") || strings.HasPrefix(origin, "http://localhost:"):
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}
