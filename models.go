package main

// Model discovery. The catalog lives behind a credential, so model.for_auth is
// the only authoritative source (same reasoning as workbuddy-cliproxy). The host
// asks per auth, so the result is cached briefly to keep the panel from hammering
// upstream.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type upstreamModel struct {
	ModelName   string `json:"model_name"`
	DisplayName string `json:"display_name"`
	CostRatio   any    `json:"cost_ratio"`
	Description string `json:"description"`
	VipType     any    `json:"vip_type"`
}

type catalog struct {
	models  []upstreamModel
	fetched time.Time
	err     string
	uid     string
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalog{} // key: account identity
)

const catalogTTL = 10 * time.Minute

// fetchCatalog returns the upstream model list for one credential, cached.
func fetchCatalog(ctx context.Context, sa *storedAuth) ([]upstreamModel, error) {
	key := accountIdentity(sa)
	catalogMu.Lock()
	cached, ok := catalogCache[key]
	catalogMu.Unlock()
	if ok && cached.err == "" && time.Since(cached.fetched) < catalogTTL && len(cached.models) > 0 {
		return cached.models, nil
	}

	raw, err := sa.client().modelList(ctx)
	if err != nil {
		catalogMu.Lock()
		catalogCache[key] = catalog{fetched: time.Now(), err: err.Error()}
		catalogMu.Unlock()
		return nil, err
	}
	var models []upstreamModel
	if list, ok := raw["model_list"].([]any); ok {
		for _, item := range list {
			b, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var m upstreamModel
			if json.Unmarshal(b, &m) == nil && strings.TrimSpace(m.ModelName) != "" {
				models = append(models, m)
			}
		}
	}
	catalogMu.Lock()
	catalogCache[key] = catalog{models: models, fetched: time.Now()}
	catalogMu.Unlock()
	return models, nil
}

// handleModelsForAuth resolves the live per-account catalog.
func handleModelsForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, ok := parseStored(req.StorageJSON)
	if !ok {
		// A credential the plugin cannot read is reported as an account with no
		// models rather than an error.
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := fetchCatalog(ctx, sa)
	if err != nil {
		hostLog("warn", "kuku: model catalog fetch failed", map[string]any{"uid": sa.UID, "error": err.Error()})
		// Fall through to configured extra models so a transient upstream
		// failure does not empty the catalog entirely.
		models = nil
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: modelsForAuth(sa, models)})
}

// modelsForAuth applies the publish policy and the model prefix.
func modelsForAuth(sa *storedAuth, upstream []upstreamModel) []pluginapi.ModelInfo {
	prefix := configuredModelPrefix()
	mode, allow := configuredPublish()
	allowSet := map[string]bool{}
	for _, id := range allow {
		allowSet[id] = true
	}
	extra := configuredExtraModels()
	extraSet := map[string]bool{}
	for _, id := range extra {
		extraSet[id] = true
	}

	seen := map[string]bool{}
	out := make([]pluginapi.ModelInfo, 0, len(upstream)+len(extra))
	add := func(id, display, desc string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		published := id
		if prefix != "" {
			published = prefix + "/" + id
		}
		info := pluginapi.ModelInfo{
			ID:          published,
			Object:      "model",
			OwnedBy:     providerName,
			Type:        "chat",
			Name:        id,
			DisplayName: firstNonEmpty(display, id),
			Description: desc,
		}
		out = append(out, info)
	}

	for _, m := range upstream {
		id := strings.TrimSpace(m.ModelName)
		if mode == "allow" && !allowSet[id] && !extraSet[id] {
			continue
		}
		add(id, m.DisplayName, m.Description)
	}
	// Extra models are always published: the operator listed them on purpose.
	for _, id := range extra {
		add(id, "", "")
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
