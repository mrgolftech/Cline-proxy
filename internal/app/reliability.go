package app

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"cline-go-proxy/internal/cline"
)

var clineAPIBaseURL = cline.ClineAPIBase

const maxClineFailoverAttempts = 3

func pickAccountExcluding(excluded map[string]struct{}) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	active := make([]*Account, 0, len(p.Accounts))
	now := time.Now()
	for _, a := range p.Accounts {
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && now.After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.Status != "active" {
			continue
		}
		if _, seen := excluded[a.AccountID]; seen {
			continue
		}
		active = append(active, a)
	}
	if len(active) == 0 {
		return nil
	}

	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		acc = active[time.Now().UnixNano()%int64(len(active))]
	default:
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}
	savePoolLocked()
	return acc
}

func countEligibleAccounts() int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	now := time.Now()
	n := 0
	for _, a := range p.Accounts {
		if a.Status == "active" || (a.Status == "cooldown" && !a.CooldownUntil.IsZero() && now.After(a.CooldownUntil)) {
			n++
		}
	}
	return n
}

func newClineRequest(token, sessionID string, bodyJSON []byte) (*http.Request, error) {
	req, err := http.NewRequest("POST", clineAPIBaseURL+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header = clineHeaders(token, sessionID)
	return req, nil
}

func markAccountExpired(acc *Account, reason string) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Status = "expired"
	acc.LastReason = reason
	savePoolLocked()
	poolMu.Unlock()
}
