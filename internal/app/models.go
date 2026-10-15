package app

import (
	"cline-go-proxy/internal/cline"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ModelStatus string

const (
	ModelActive  ModelStatus = "active"
	ModelRemoved ModelStatus = "removed"
	ModelUnknown ModelStatus = "unknown"
)

type ModelInfo struct {
	ID             string      `json:"id"`
	Name           string      `json:"name,omitempty"`
	Source         string      `json:"source"`
	Provider       string      `json:"provider"`
	Cost           string      `json:"cost"`
	Status         ModelStatus `json:"status"`
	RequiresStream bool        `json:"requiresStream,omitempty"`
	SyncedAt       time.Time   `json:"syncedAt,omitempty"`
}

var (
	modelsMu       sync.Mutex
	modelsCache    map[string]*ModelInfo
	modelsSyncing  bool
	modelsLastSync time.Time
	// modelProbed records the last liveness probe per model id. A cached model that
	// dropped out of the official feed is probed; if upstream says "not found" it is
	// marked ModelRemoved so it stops being advertised. Re-probed every
	// modelProbeInterval so a model that comes back is noticed.
	modelProbed = map[string]time.Time{}
)

const modelProbeInterval = 6 * time.Hour

const (
	modelsRefreshInterval = 60 * time.Second
	modelsSyncTimeout     = 25 * time.Second
)

const recommendedModelsURL = cline.ClineAPIBase + "/ai/cline/recommended-models"

func seedModelCandidates() []*ModelInfo {
	return []*ModelInfo{
		{ID: "deepseek/deepseek-v4-flash", Source: "free", Provider: "deepseek", Cost: "free", RequiresStream: true},
		{ID: "poolside/laguna-s-2.1:free", Source: "free", Provider: "poolside", Cost: "free"},
		{ID: "stepfun/step-3.7-flash", Source: "free", Provider: "stepfun", Cost: "free", RequiresStream: true},
	}
}

func initModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsCache != nil {
		return
	}
	modelsCache = make(map[string]*ModelInfo)
	for _, m := range seedModelCandidates() {
		modelsCache[m.ID] = m
	}
}

func getFreeModels() []*ModelInfo {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make([]*ModelInfo, 0, len(modelsCache))
	for _, m := range modelsCache {
		cp := *m
		out = append(out, &cp)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j-1].ID < out[j].ID {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

type recommendedPayload struct {
	Free []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"free"`
}

func syncRecommendedModels() (int, error) {
	initModelsCache()

	acc := pickAccount()
	if acc == nil {
		return 0, fmt.Errorf("no active accounts")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("GET", recommendedModelsURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header = clineHeaders(token, "")
	req.Header.Set("X-Task-ID", fmt.Sprintf("sess_sync_%d", time.Now().UnixMilli()))

	client := &http.Client{Timeout: modelsSyncTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload recommendedPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	modelsMu.Lock()

	added := 0
	inFeed := make(map[string]bool, len(payload.Free))
	for _, m := range payload.Free {
		id := m.ID
		inFeed[id] = true
		provider := id
		if i := indexByte(id, '/'); i >= 0 {
			provider = id[:i]
		}
		if cached, ok := modelsCache[id]; ok {
			cached.Source = "free"
			cached.Cost = "free"
			cached.Provider = provider
			cached.Status = ModelActive
			cached.SyncedAt = time.Now()
			if cached.Name == "" {
				cached.Name = m.Name
			}
			continue
		}
		modelsCache[id] = &ModelInfo{
			ID:             id,
			Name:           m.Name,
			Source:         "free",
			Provider:       provider,
			Cost:           "free",
			Status:         ModelActive,
			RequiresStream: indexByte(id, ':') < 0,
			SyncedAt:       time.Now(),
		}
		added++
	}

	// A cached model that is no longer in the official feed may be retired upstream.
	// Collect the ones due for a probe; the actual network probe runs outside the
	// lock (see reconcileStaleModels) so a slow upstream never blocks /v1/models.
	stale := make([]string, 0)
	now := time.Now()
	for id, m := range modelsCache {
		if inFeed[id] || m.Status == ModelRemoved {
			continue
		}
		if last, ok := modelProbed[id]; ok && now.Sub(last) < modelProbeInterval {
			continue
		}
		stale = append(stale, id)
	}
	modelsMu.Unlock()

	if len(stale) > 0 {
		go reconcileStaleModels(stale)
	}

	modelsMu.Lock()
	modelsLastSync = time.Now()
	modelsMu.Unlock()
	return added, nil
}

// reconcileStaleModels probes cached-but-unlisted models and marks the ones upstream
// answers "not found" for as ModelRemoved. Anything still answering stays listed —
// some free models work after being delisted from the feed (e.g. z-ai/glm-5.3-flash).
func reconcileStaleModels(ids []string) {
	for _, id := range ids {
		removed := probeModelGone(id)
		modelsMu.Lock()
		modelProbed[id] = time.Now()
		if removed {
			if m, ok := modelsCache[id]; ok && m.Status == ModelActive {
				m.Status = ModelRemoved
				log.Printf("  model probe: %q no longer available upstream, marked removed", id)
			}
		} else if m, ok := modelsCache[id]; ok && m.Status == ModelRemoved {
			m.Status = ModelActive
			log.Printf("  model probe: %q is available again, reactivated", id)
		}
		modelsMu.Unlock()
	}
}

// probeModelGone issues one minimal upstream chat call and reports whether the model
// is gone (HTTP 404 / "model not found"). Any other outcome — 200, 429, 5xx, network
// error — is treated as "still there" so the probe itself can never retire a model on
// a transient failure.
func probeModelGone(id string) bool {
	acc := pickAccount()
	if acc == nil {
		return false
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return false
	}
	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`, id))
	req, err := http.NewRequest("POST", clineAPIBaseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	req.Header = clineHeaders(token, "sess_probe")
	client := &http.Client{Timeout: modelsSyncTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return true
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false
	}
	// Non-404 errors: peek at the body for an explicit "not found".
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.Contains(strings.ToLower(string(b)), "model not found")
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func syncModelsOnce() {
	initModelsCache()
	modelsMu.Lock()
	if modelsSyncing {
		modelsMu.Unlock()
		return
	}
	modelsSyncing = true
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsSyncing = false
		modelsMu.Unlock()
	}()

	added, err := syncRecommendedModels()
	if err != nil {
		log.Printf("  model sync: failed (%v), using cached list", err)
		return
	}
	if added > 0 {
		log.Printf("  model sync: %d new free models from official feed", added)
	} else {
		log.Printf("  model sync: %d free models up to date", len(getFreeModels()))
	}
}

func getDefaultModel() string {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if m, ok := modelsCache[defaultModel]; ok && m.Status == ModelActive {
		return defaultModel
	}
	for _, m := range modelsCache {
		if m.Status == ModelActive {
			return m.ID
		}
	}
	return defaultModel
}

func normalizeRequestModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return getDefaultModel()
	}
	initModelsCache()
	modelsMu.Lock()
	m, ok := modelsCache[id]
	modelsMu.Unlock()
	if ok && m.Status != ModelRemoved {
		return id
	}
	if ok {
		log.Printf("  model %q is marked removed, fallback to %q", id, getDefaultModel())
	} else {
		log.Printf("  model %q not in free list, fallback to %q", id, getDefaultModel())
	}
	return getDefaultModel()
}

func apiModelList() []map[string]any {
	out := make([]map[string]any, 0, len(modelsCache))
	for _, m := range getFreeModels() {
		if m.Status == ModelRemoved {
			continue // retired upstream: don't advertise
		}
		out = append(out, map[string]any{
			"id":         m.ID,
			"object":     "model",
			"created":    time.Now().UnixMilli(),
			"owned_by":   m.Provider,
			"source":     m.Source,
			"status":     m.Status,
			"cost":       m.Cost,
			"requiresStream": m.RequiresStream,
			"syncedAt":   m.SyncedAt,
		})
	}
	return out
}

func ensureModelsFresh() {
	initModelsCache()
	modelsMu.Lock()
	needSync := modelsLastSync.IsZero() || time.Since(modelsLastSync) > modelsRefreshInterval
	syncing := modelsSyncing
	modelsMu.Unlock()
	if needSync && !syncing {
		go syncModelsOnce()
	}
}

func startModelsRefresher() {
	go func() {
		syncModelsOnce()
		ticker := time.NewTicker(modelsRefreshInterval)
		for range ticker.C {
			syncModelsOnce()
		}
	}()
}
