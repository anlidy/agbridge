package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"agbridge/internal/applog"
	"agbridge/internal/config"
	"agbridge/internal/netx"
)

// NativeModel summarises an entry of Google's catalog for the console.
type NativeModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Model       string `json:"model"`
}

var errNoChange = errors.New("no change")

var placeholderRe = regexp.MustCompile(`^MODEL_PLACEHOLDER_M(\d+)$`)

var catalogKeys = []string{"models", "availableModels", "available_models"}

// handleCatalog forwards fetchAvailableModels and appends the custom models
// so they show up in Antigravity's native model picker.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, origin string) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+r.URL.Path+query(r), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "accept-encoding", "content-length", "connection", "host":
			continue
		}
		req.Header[k] = v
	}
	resp, err := netx.Client(s.store.Get().Proxy).Do(req)
	if err != nil {
		log.Printf("[catalog] upstream error: %v", err)
		writeError(w, http.StatusBadGateway, "agbridge 获取模型列表失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	var catalog map[string]any
	if resp.StatusCode/100 != 2 || json.Unmarshal(raw, &catalog) != nil {
		// Keep native error handling (e.g. token refresh on 401) intact.
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	added := s.injectCatalog(catalog)
	out, _ := json.Marshal(catalog)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.ring.Add(applog.Record{Time: start, Kind: "catalog", Model: fmt.Sprintf("+%d 个自定义模型", added),
		Status: 200, DurationMs: time.Since(start).Milliseconds()})
	log.Printf("[catalog] native=%d custom=%d", len(s.NativeModels()), added)
}

func query(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		switch strings.ToLower(k) {
		case "content-length", "content-encoding", "transfer-encoding", "connection":
			continue
		}
		dst[k] = v
	}
}

// injectCatalog mutates catalog in place and returns how many models it added.
func (s *Server) injectCatalog(catalog map[string]any) int {
	natives, used := scanCatalog(catalog)
	s.mu.Lock()
	s.nativeModel, s.catalogAt = natives, time.Now()
	s.mu.Unlock()

	_ = s.store.Update(func(c *config.Config) error {
		if !config.AssignPlaceholders(c, used) {
			return errNoChange
		}
		return nil
	})
	cfg := s.store.Get()

	usedLabels := map[string]bool{}
	for _, n := range natives {
		usedLabels[n.DisplayName] = true
	}
	type entry struct {
		slug string
		m    map[string]any
		list map[string]any
	}
	var entries []entry
	for i := range cfg.Models {
		m := &cfg.Models[i]
		p := cfg.Provider(m.ProviderID)
		if !m.Enabled || p == nil {
			continue
		}
		var items []config.Entry
		for _, e := range m.Entries() {
			if e.Placeholder != nil {
				items = append(items, e)
			}
		}
		if len(items) == 0 {
			continue
		}
		suffixes := make([]string, len(items))
		for i, e := range items {
			suffixes[i] = e.Label
		}
		label := uniqueLabel(m.DisplayName, p.Name, suffixes, usedLabels)
		maxTok, maxOut := limits(m, p)
		images := m.SupportsImages == nil || *m.SupportsImages
		think := m.SupportsThink == nil || *m.SupportsThink
		mimes := map[string]any{
			"text/plain": true, "text/markdown": true, "text/html": true, "text/css": true, "text/csv": true,
			"text/xml": true, "application/json": true, "application/x-javascript": true,
			"application/x-typescript": true, "application/x-python-code": true,
		}
		if images {
			for _, t := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
				mimes[t] = true
			}
		}
		if p.Type == config.TypeGemini || p.Type == config.TypeAnthropic {
			mimes["application/pdf"] = true
		}
		for _, e := range items {
			entries = append(entries, entry{
				slug: e.Slug,
				m: map[string]any{
					"displayName":        label + e.Label,
					"supportsImages":     images,
					"supportsThinking":   think,
					"supportsVideo":      false,
					"supportedMimeTypes": mimes,
					"recommended":        true,
					"maxTokens":          maxTok,
					"maxOutputTokens":    maxOut,
					"tokenizerType":      "LLAMA_WITH_SPECIAL",
					"model":              config.PlaceholderName(*e.Placeholder),
					"apiProvider":        "API_PROVIDER_GOOGLE_GEMINI",
					"modelProvider":      "MODEL_PROVIDER_GOOGLE",
				},
				list: map[string]any{
					"name":                       "models/" + e.Slug,
					"version":                    "1.0",
					"displayName":                label + e.Label,
					"inputTokenLimit":            maxTok,
					"outputTokenLimit":           maxOut,
					"supportedGenerationMethods": []string{"generateContent", "countTokens"},
				},
			})
		}
	}
	slugs := make([]string, len(entries))
	for i, e := range entries {
		slugs[i] = e.slug
	}
	s.mu.Lock()
	s.injected = slugs
	s.mu.Unlock()
	if len(entries) == 0 {
		return 0
	}
	merged := false
	for _, key := range catalogKeys {
		switch coll := catalog[key].(type) {
		case map[string]any:
			for _, e := range entries {
				coll[e.slug] = e.m
			}
			merged = true
		case []any:
			for _, e := range entries {
				coll = append(coll, e.list)
			}
			catalog[key] = coll
			merged = true
		}
	}
	if !merged {
		coll := map[string]any{}
		for _, e := range entries {
			coll[e.slug] = e.m
		}
		catalog["models"] = coll
	}
	ids := make([]any, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.slug)
	}
	// The picker only lists models that appear in a sort group.
	sorts, _ := catalog["agentModelSorts"].([]any)
	attached := false
	for _, so := range sorts {
		som, _ := so.(map[string]any)
		groups, _ := som["groups"].([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			list, ok := gm["modelIds"].([]any)
			if !ok {
				continue
			}
			gm["modelIds"] = appendUnique(list, ids)
			attached = true
		}
	}
	if !attached {
		catalog["agentModelSorts"] = append(sorts, map[string]any{
			"displayName": "第三方模型",
			"groups":      []any{map[string]any{"modelIds": ids}},
		})
	}
	return len(entries)
}

func appendUnique(list, add []any) []any {
	seen := map[any]bool{}
	for _, v := range list {
		seen[v] = true
	}
	for _, v := range add {
		if !seen[v] {
			list = append(list, v)
		}
	}
	return list
}

// scanCatalog lists native models and the placeholder indexes they occupy.
func scanCatalog(catalog map[string]any) ([]NativeModel, []int) {
	var natives []NativeModel
	var used []int
	visit := func(id string, v any) {
		m, _ := v.(map[string]any)
		if m == nil || strings.HasPrefix(id, "agb-") {
			return
		}
		n := NativeModel{ID: id}
		n.DisplayName, _ = m["displayName"].(string)
		switch enum := m["model"].(type) {
		case string:
			n.Model = enum
			if sub := placeholderRe.FindStringSubmatch(enum); sub != nil {
				idx, _ := strconv.Atoi(sub[1])
				used = append(used, idx)
			}
		case float64:
			n.Model = strconv.Itoa(int(enum))
		}
		natives = append(natives, n)
	}
	for _, key := range catalogKeys {
		switch coll := catalog[key].(type) {
		case map[string]any:
			for id, v := range coll {
				visit(id, v)
			}
		case []any:
			for _, v := range coll {
				m, _ := v.(map[string]any)
				name, _ := m["name"].(string)
				visit(strings.TrimPrefix(name, "models/"), v)
			}
		}
	}
	sort.Slice(natives, func(i, j int) bool { return natives[i].ID < natives[j].ID })
	return natives, used
}

// uniqueLabel picks a display name whose entries (name + each suffix) do not
// clash with native or earlier custom ones, and reserves them.
func uniqueLabel(name, provider string, suffixes []string, used map[string]bool) string {
	free := func(label string) bool {
		for _, sfx := range suffixes {
			if used[label+sfx] {
				return false
			}
		}
		return true
	}
	label := name
	if !free(label) {
		label = name + " · " + provider
	}
	for i := 2; !free(label); i++ {
		label = fmt.Sprintf("%s · %s #%d", name, provider, i)
	}
	for _, sfx := range suffixes {
		used[label+sfx] = true
	}
	return label
}

func limits(m *config.Model, p *config.Provider) (int, int) {
	maxTok, maxOut := m.MaxTokens, m.MaxOutputTokens
	if maxTok == 0 {
		maxTok = 128000
		if p.Type == config.TypeGemini {
			maxTok = 1048576
		} else if p.Type == config.TypeAnthropic {
			maxTok = 200000
		}
	}
	if maxOut == 0 {
		maxOut = 8192
		if p.Type == config.TypeGemini {
			maxOut = 65536
		} else if p.Type == config.TypeAnthropic {
			maxOut = 32000
		}
	}
	return maxTok, maxOut
}

func (s *Server) NativeModels() []NativeModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeModel
}

// CatalogStale reports whether the custom models changed since the language
// server last fetched the catalog, so the picker does not show them yet.
func (s *Server) CatalogStale() bool {
	s.mu.Lock()
	served, injected := !s.catalogAt.IsZero(), s.injected
	s.mu.Unlock()
	if !served {
		return false
	}
	cfg := s.store.Get()
	var want []string
	for i := range cfg.Models {
		if m := &cfg.Models[i]; m.Enabled && cfg.Provider(m.ProviderID) != nil {
			for _, e := range m.Entries() {
				want = append(want, e.Slug)
			}
		}
	}
	return !slices.Equal(sortedCopy(want), sortedCopy(injected))
}

func sortedCopy(v []string) []string {
	v = slices.Clone(v)
	slices.Sort(v)
	return v
}
