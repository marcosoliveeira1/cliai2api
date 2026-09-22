package app

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// modelCatalogs holds one raw (unprefixed) catalog per gateway, keyed by
// gateway name, so the two gateways never overwrite each other.
// modelCatalog is the unified view served by GET /v1/models: every entry
// carries its gateway prefix (cmdcode/<id>, opencode/<id>). It is rebuilt
// from the buckets on every update — fetch paths never write it directly.
var (
	modelCatalogMu sync.RWMutex
	modelCatalogs  = map[string][]ModelInfo{}
	modelCatalog   []ModelInfo
)

// SetGatewayCatalog replaces one gateway's raw catalog and rebuilds the
// unified prefixed view. A nil slice clears the bucket (empty contribution)
// without touching the other gateway.
func setGatewayCatalog(gateway string, models []ModelInfo) {
	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()
	if modelCatalogs == nil {
		modelCatalogs = map[string][]ModelInfo{}
	}
	modelCatalogs[gateway] = append([]ModelInfo(nil), models...)
	refreshUnifiedCatalogLocked()
}

// refreshUnifiedCatalog rebuilds modelCatalog as the union of the per-gateway
// buckets with the gateway prefix applied. Gateway order is fixed so the
// listing is deterministic; a gateway without accounts contributes nothing.
func refreshUnifiedCatalogLocked() {
	unified := make([]ModelInfo, 0)
	for _, gateway := range []string{GatewayCmdcode, GatewayOpencode} {
		for _, m := range modelCatalogs[gateway] {
			m.ID = gateway + "/" + m.ID
			unified = append(unified, m)
		}
	}
	modelCatalog = unified
}

func modelCatalogSnapshot() []ModelInfo {
	modelCatalogMu.RLock()
	defer modelCatalogMu.RUnlock()
	return append([]ModelInfo(nil), modelCatalog...)
}

func gatewayCatalogSnapshot(gateway string) []ModelInfo {
	modelCatalogMu.RLock()
	defer modelCatalogMu.RUnlock()
	return append([]ModelInfo(nil), modelCatalogs[gateway]...)
}

// FetchProviderModels 从 CC API 拉取模型列表，填充 cmdcode gateway 的 bucket。
// 拉取失败只记 [WARN] 并保持已有 catalog，其它 gateway 不受影响。
func FetchProviderModels(baseURL, apiKey string) {
	url := baseURL + "/provider/v1/models"

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("[WARN] fetch models: build request failed: %v (using empty catalog)", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] fetch models: request failed: %v (using empty catalog)", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[WARN] fetch models: unexpected status %d (using empty catalog)", resp.StatusCode)
		return
	}

	var list CCProviderModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		log.Printf("[WARN] fetch models: decode response failed: %v (using empty catalog)", err)
		return
	}

	catalog := make([]ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		catalog = append(catalog, ModelInfo{
			ID:            m.ID,
			Object:        "model",
			Created:       1700000000,
			OwnedBy:       "commandcode",
			ContextWindow: m.ContextLength,
		})
	}
	setGatewayCatalog(GatewayCmdcode, catalog)
	log.Printf("models: %d loaded from %s", len(gatewayCatalogSnapshot(GatewayCmdcode)), url)
}

// refreshGatewayCatalog updates one gateway after its account configuration
// changes. Failed fetches keep the last successful catalog; no enabled account
// clears only that gateway's contribution.
func refreshGatewayCatalog(gateway string, cfg *Config, pool *AccountPool) {
	if pool == nil || pool.Primary() == nil {
		catalogGateway := gateway
		if gateway == GatewayZen {
			catalogGateway = GatewayOpencode
		}
		setGatewayCatalog(catalogGateway, nil)
		return
	}

	switch gateway {
	case GatewayCmdcode:
		FetchProviderModels(cfg.UpstreamBaseURL(), pool.Primary().APIKey)
	case GatewayZen:
		models := NewZenClientWithPool(pool, cfg.GatewayBaseURL(GatewayZen)).FetchModels()
		if len(models) == 0 {
			log.Printf("[WARN] fetch zen models failed; keeping existing zen catalog")
			return
		}
		setGatewayCatalog(GatewayOpencode, models)
	}
}

func availableModels() []string {
	models := modelCatalogSnapshot()
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

func isModelExcluded(model string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	candidates := []string{model}
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		candidates = append(candidates, model[idx+1:])
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		for _, e := range excludes {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if strings.HasPrefix(c, e) {
				return true
			}
		}
	}
	return false
}
