// Package models handles model discovery, health checks and per-model
// telemetry. Ported from src/models/*.mjs, behavior is the spec.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// staticCreated is the epoch for static models, matching
// src/models/discovery.mjs (1626777600).
const staticCreated int64 = 1626777600

// Model is one entry served by /v1/models and /models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// Discovery holds the current model list and its source (static or dynamic).
// Mirrors src/models/discovery.mjs.
type Discovery struct {
	mu         sync.RWMutex
	list       []Model
	source     string
	staticList []Model // snapshot of the static list, restored on any fetch failure
}

// NewDiscovery seeds Discovery with the static list (object model, created
// 1626777600, owned_by agentrouter).
func NewDiscovery(staticIDs []string) *Discovery {
	static := make([]Model, 0, len(staticIDs))
	for _, id := range staticIDs {
		static = append(static, Model{
			ID:      id,
			Object:  "model",
			Created: staticCreated,
			OwnedBy: "agentrouter",
		})
	}
	list := make([]Model, len(static))
	copy(list, static)
	return &Discovery{list: list, source: "static", staticList: static}
}

// List returns a copy of the current list.
func (d *Discovery) List() []Model {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Model, len(d.list))
	copy(out, d.list)
	return out
}

// Source reports where the list came from, static or dynamic.
func (d *Discovery) Source() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.source
}

// Fetch refreshes the model list from GET /v1/models. Empty apiKey is a
// no-op, mirrors the Node guard on AR_API_KEY.
//
// It builds a fresh 15s client that reuses the caller's Transport. Any
// failure (transport error, non-200, bad JSON, missing data array) restores
// the static list and sets source to static, matching the Node fallback.
// A 200 with a JSON array, even empty, counts as dynamic, like Node's
// Array.isArray(data.data) check.
func (d *Discovery) Fetch(ctx context.Context, client *http.Client, host string, port int, apiKey string) {
	if apiKey == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL(host, port, "/v1/models"), nil)
	if err != nil {
		slog.Warn(fmt.Sprintf("Model discovery failed: %v, using static list", err))
		d.fallbackStatic()
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "agentrouter-spoof-proxy/1.0")
	req.Header.Set("Accept", "application/json")

	c := clientWithTimeout(client, 15*time.Second)
	resp, err := c.Do(req)
	if err != nil {
		slog.Warn(fmt.Sprintf("Model discovery failed: %v, using static list", err))
		d.fallbackStatic()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn(fmt.Sprintf("Model discovery failed: status %d, using static list", resp.StatusCode))
		d.fallbackStatic()
		return
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		slog.Warn(fmt.Sprintf("Model discovery failed: %v, using static list", err))
		d.fallbackStatic()
		return
	}
	if payload.Data == nil {
		// 200 but unexpected shape, keep current list and source (Node no-op).
		return
	}
	list := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		created := m.Created
		if created == 0 {
			created = staticCreated
		}
		ownedBy := m.OwnedBy
		if ownedBy == "" {
			ownedBy = "agentrouter"
		}
		list = append(list, Model{ID: m.ID, Object: "model", Created: created, OwnedBy: ownedBy})
	}
	d.mu.Lock()
	d.list = list
	d.source = "dynamic"
	d.mu.Unlock()
	slog.Info(fmt.Sprintf("DISCOVERED %d models from upstream", len(list)))
}

func (d *Discovery) fallbackStatic() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = make([]Model, len(d.staticList))
	copy(d.list, d.staticList)
	d.source = "static"
}

// upstreamURL builds an upstream URL. Scheme follows the production default:
// port 443 is https, anything else is http. That covers TARGET_PROTOCOL=http
// and plain-HTTP test upstreams.
func upstreamURL(host string, port int, path string) string {
	scheme := "https"
	if port != 443 {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
}

// clientWithTimeout returns a fresh client with the given timeout that
// reuses the caller's Transport when available.
func clientWithTimeout(client *http.Client, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport
	if client != nil && client.Transport != nil {
		tr = client.Transport
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}
