package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"agbridge/internal/config"
	"agbridge/internal/gem"
	"agbridge/internal/netx"
	"agbridge/internal/provider"
)

// providerView hides the API key from the console.
type providerView struct {
	config.Provider
	HasKey  bool   `json:"hasKey"`
	KeyHint string `json:"keyHint,omitempty"`
}

type providerInput struct {
	config.Provider
	APIKey *string `json:"apiKey"`
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	tok := s.store.Get().APIToken
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-AGB-Token")), []byte(tok)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/state" && r.Method == http.MethodGet:
		s.apiState(w)
	case path == "/settings" && r.Method == http.MethodPost:
		s.apiSettings(w, r)
	case path == "/providers" && r.Method == http.MethodPost:
		s.apiSaveProvider(w, r)
	case len(parts) == 2 && parts[0] == "providers" && r.Method == http.MethodDelete:
		s.apiDeleteProvider(w, parts[1])
	case len(parts) == 3 && parts[0] == "providers" && parts[2] == "models" && r.Method == http.MethodPost:
		s.apiListUpstream(w, r, parts[1])
	case path == "/models" && r.Method == http.MethodPost:
		s.apiSaveModel(w, r)
	case len(parts) == 2 && parts[0] == "models" && r.Method == http.MethodDelete:
		s.apiDeleteModel(w, parts[1])
	case len(parts) == 3 && parts[0] == "models" && parts[2] == "test" && r.Method == http.MethodPost:
		s.apiTestModel(w, r, parts[1])
	case path == "/install" && r.Method == http.MethodPost:
		s.apiHook(w, s.hooks.Install)
	case path == "/uninstall" && r.Method == http.MethodPost:
		s.apiHook(w, s.hooks.Uninstall)
	case path == "/logs" && r.Method == http.MethodGet:
		s.apiLogs(w)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (s *Server) apiState(w http.ResponseWriter) {
	cfg := s.store.Get()
	views := make([]providerView, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		v := providerView{Provider: p, HasKey: p.APIKey != ""}
		if len(p.APIKey) > 8 {
			v.KeyHint = p.APIKey[:3] + "…" + p.APIKey[len(p.APIKey)-4:]
		}
		v.APIKey = ""
		for k := range v.Headers {
			v.Headers[k] = "••••"
		}
		views = append(views, v)
	}
	var status any
	if s.hooks.Status != nil {
		status = s.hooks.Status()
	}
	s.mu.Lock()
	catalogAt := s.catalogAt
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      s.Version,
		"port":         cfg.Port,
		"proxy":        cfg.Proxy,
		"upstreams":    cfg.Upstreams,
		"antigravity":  cfg.AntigravityDir,
		"patchBinary":  cfg.PatchEnabled(),
		"configPath":   s.store.Path(),
		"logPath":      s.hooks.LogFile,
		"providers":    views,
		"models":       cfg.Models,
		"install":      status,
		"nativeModels": s.NativeModels(),
		"catalogAt":    catalogAt,
		"catalogStale": s.CatalogStale(),
		"records":      s.ring.List(),
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) respond(w http.ResponseWriter, err error, v any) {
	if err != nil && !errors.Is(err, errNoChange) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Port           *int    `json:"port"`
		Proxy          *string `json:"proxy"`
		AntigravityDir *string `json:"antigravity"`
		PatchBinary    *bool   `json:"patchBinary"`
	}
	if !decode(w, r, &in) {
		return
	}
	err := s.store.Update(func(c *config.Config) error {
		if in.Port != nil {
			c.Port = *in.Port
		}
		if in.Proxy != nil {
			c.Proxy = strings.TrimSpace(*in.Proxy)
		}
		if in.AntigravityDir != nil {
			c.AntigravityDir = strings.TrimSpace(*in.AntigravityDir)
		}
		if in.PatchBinary != nil {
			c.PatchBinary = in.PatchBinary
		}
		return nil
	})
	netx.Reset()
	s.respond(w, err, map[string]any{"ok": true})
}

func (s *Server) apiSaveProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decode(w, r, &in) {
		return
	}
	p := in.Provider
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	err := s.store.Update(func(c *config.Config) error {
		if p.ID == "" {
			p.ID = config.NewID()
			if in.APIKey != nil {
				p.APIKey = strings.TrimSpace(*in.APIKey)
			}
			c.Providers = append(c.Providers, p)
			return nil
		}
		cur := c.Provider(p.ID)
		if cur == nil {
			return errors.New("提供方不存在")
		}
		key, headers := cur.APIKey, cur.Headers
		*cur = p
		cur.APIKey = key
		if in.APIKey != nil {
			cur.APIKey = strings.TrimSpace(*in.APIKey)
		}
		// Masked header values come back as "••••": keep the stored secret.
		for k, v := range cur.Headers {
			if v == "••••" {
				cur.Headers[k] = headers[k]
			}
		}
		return nil
	})
	netx.Reset()
	s.respond(w, err, map[string]any{"id": p.ID})
}

func (s *Server) apiDeleteProvider(w http.ResponseWriter, id string) {
	err := s.store.Update(func(c *config.Config) error {
		ps := c.Providers[:0]
		for _, p := range c.Providers {
			if p.ID != id {
				ps = append(ps, p)
			}
		}
		c.Providers = ps
		ms := c.Models[:0]
		for _, m := range c.Models {
			if m.ProviderID != id {
				ms = append(ms, m)
			}
		}
		c.Models = ms
		return nil
	})
	s.respond(w, err, map[string]any{"ok": true})
}

func (s *Server) apiListUpstream(w http.ResponseWriter, r *http.Request, id string) {
	cfg := s.store.Get()
	p := cfg.Provider(id)
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "提供方不存在"})
		return
	}
	models, err := provider.ListModels(r.Context(), p, cfg.Proxy)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) apiSaveModel(w http.ResponseWriter, r *http.Request) {
	var m config.Model
	if !decode(w, r, &m) {
		return
	}
	m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	if m.DisplayName == "" {
		m.DisplayName = m.UpstreamModel
	}
	err := s.store.Update(func(c *config.Config) error {
		if m.ID == "" {
			m.ID = config.NewID()
			m.Placeholder, m.EffortPlaceholders = nil, nil
			c.Models = append(c.Models, m)
		} else {
			cur := c.Model(m.ID)
			if cur == nil {
				return errors.New("模型不存在")
			}
			// Identities are owned by agbridge.
			m.Placeholder, m.EffortPlaceholders = cur.Placeholder, cur.EffortPlaceholders
			*cur = m
		}
		config.AssignPlaceholders(c, nil)
		return nil
	})
	s.respond(w, err, map[string]any{"id": m.ID})
}

func (s *Server) apiDeleteModel(w http.ResponseWriter, id string) {
	err := s.store.Update(func(c *config.Config) error {
		ms := c.Models[:0]
		for _, m := range c.Models {
			if m.ID != id {
				ms = append(ms, m)
			}
		}
		c.Models = ms
		return nil
	})
	s.respond(w, err, map[string]any{"ok": true})
}

func (s *Server) apiTestModel(w http.ResponseWriter, r *http.Request, id string) {
	cfg := s.store.Get()
	m := cfg.Model(id)
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "模型不存在"})
		return
	}
	p := cfg.Provider(m.ProviderID)
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "提供方不存在"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	body := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Reply with exactly: AGBRIDGE OK"}}}},
		"generationConfig": map[string]any{"maxOutputTokens": 1024},
	}
	start := time.Now()
	var merger gem.Merger
	_, err := provider.Generate(ctx, &provider.Call{Provider: p, Model: m, Body: body, Stream: true, GlobalProxy: cfg.Proxy}, merger.Add)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "ms": time.Since(start).Milliseconds()})
		return
	}
	var text strings.Builder
	for _, part := range merger.Result().Candidates[0].Content.Parts {
		if !part.Thought {
			text.WriteString(part.Text)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text.String(), "ms": time.Since(start).Milliseconds()})
}

func (s *Server) apiHook(w http.ResponseWriter, fn func() error) {
	if fn == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "当前平台不支持"})
		return
	}
	if err := fn(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiLogs(w http.ResponseWriter) {
	f, err := os.Open(s.hooks.LogFile)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"text": ""})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	const tail = 64 << 10
	if st != nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	writeJSON(w, http.StatusOK, map[string]any{"text": string(b)})
}
