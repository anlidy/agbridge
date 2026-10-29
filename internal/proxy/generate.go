package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agbridge/internal/applog"
	"agbridge/internal/config"
	"agbridge/internal/gem"
	"agbridge/internal/provider"
)

// Envelope fields of a Cloud Code request that are not part of the Gemini request.
var envelopeOnly = map[string]bool{
	"model": true, "project": true, "requestId": true, "userAgent": true, "requestType": true,
	"modelId": true, "model_id": true, "sessionId": true, "conversationId": true, "labels": true,
}

// matchModel finds the custom model a selector refers to: an entry's catalog
// slug, "models/<slug>", the placeholder enum name or its numeric value. It
// also returns the effort of the matched entry ("" for a plain one).
func matchModel(cfg *config.Config, selectors ...any) (*config.Model, string) {
	for _, sel := range selectors {
		var s string
		switch v := sel.(type) {
		case string:
			s = strings.TrimPrefix(v, "models/")
		case json.Number:
			s = v.String()
		case float64:
			s = strconv.Itoa(int(v))
		default:
			continue
		}
		if s == "" {
			continue
		}
		for i := range cfg.Models {
			m := &cfg.Models[i]
			if m.Slug() == s {
				return m, ""
			}
			for _, e := range m.Entries() {
				if e.Slug == s || (e.Placeholder != nil && (s == config.PlaceholderName(*e.Placeholder) || s == strconv.Itoa(1000+*e.Placeholder))) {
					return m, e.Effort
				}
			}
		}
	}
	return nil, ""
}

// handleCustom answers a generation for a custom model, or forwards it to
// upstream unchanged when it targets a native model.
func (s *Server) handleCustom(w http.ResponseWriter, r *http.Request, envelope bool, urlModel, method, upstream string) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		s.forwardBuffered(w, r, body, upstream)
		return
	}
	cfg := s.store.Get()
	var model *config.Model
	var effort string
	if envelope {
		inner, _ := req["request"].(map[string]any)
		var innerModel any
		if inner != nil {
			innerModel = inner["model"]
		}
		model, effort = matchModel(cfg, req["model"], req["modelId"], req["model_id"], innerModel)
	} else {
		model, effort = matchModel(cfg, urlModel)
	}
	if model == nil {
		s.forwardBuffered(w, r, body, upstream)
		return
	}
	prov := cfg.Provider(model.ProviderID)
	if prov == nil || !model.Enabled {
		writeError(w, http.StatusBadRequest, "agbridge: 模型 "+model.DisplayName+" 已停用或提供方不存在")
		return
	}
	label := model.DisplayName
	if effort != "" {
		m := *model // cfg is shared; adjust a copy
		m.ReasoningEffort = effort
		model = &m
		label += " (" + effort + ")"
	}

	inner := req
	if envelope {
		if m, ok := req["request"].(map[string]any); ok {
			inner = m
		} else {
			inner = map[string]any{}
			for k, v := range req {
				if !envelopeOnly[k] {
					inner[k] = v
				}
			}
		}
	}

	if method == "countTokens" {
		writeJSON(w, http.StatusOK, map[string]any{"totalTokens": gem.EstimateTokens(body)})
		return
	}

	stream := method == "streamGenerateContent"
	start := time.Now()
	rec := applog.Record{Time: start, Kind: "custom", Model: label, Provider: prov.Name}
	log.Printf("[custom] %s -> %s (%s %s) stream=%v", label, prov.Name, prov.Type, model.UpstreamModel, stream)

	wrap := func(chunk any) any {
		if !envelope {
			return chunk
		}
		return map[string]any{"response": chunk, "traceId": "", "metadata": map[string]any{}}
	}
	call := &provider.Call{Provider: prov, Model: model, Body: inner, Stream: stream, GlobalProxy: cfg.Proxy}

	var res provider.Result
	var err error
	if stream {
		flusher, _ := w.(http.Flusher)
		started := false
		res, err = provider.Generate(r.Context(), call, func(chunk any) error {
			b, err := json.Marshal(wrap(chunk))
			if err != nil {
				return err
			}
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		if err != nil && started {
			s.finish(rec, res, err)
			// Headers are gone; abort so the language server sees a broken
			// stream instead of a silently truncated answer.
			panic(http.ErrAbortHandler)
		}
	} else {
		var merger gem.Merger
		res, err = provider.Generate(r.Context(), call, merger.Add)
		if err == nil {
			writeJSON(w, http.StatusOK, wrap(merger.Result()))
		}
	}
	if err != nil {
		status := http.StatusBadGateway
		msg := err.Error()
		var pe *provider.Error
		if errors.As(err, &pe) {
			status, msg = pe.Status, pe.Message
		}
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			// Never let a third-party auth failure look like the user's Google
			// session expiring: that would make Antigravity re-authenticate.
			msg = "HTTP " + strconv.Itoa(status) + ": " + msg
			status = http.StatusBadRequest
		}
		if r.Context().Err() != nil {
			status = 499
		}
		writeError(w, status, "agbridge → "+prov.Name+": "+msg)
	}
	s.finish(rec, res, err)
}

func (s *Server) finish(rec applog.Record, res provider.Result, err error) {
	rec.DurationMs = time.Since(rec.Time).Milliseconds()
	rec.InTokens, rec.OutTokens = res.InTokens, res.OutTokens
	rec.Status = http.StatusOK
	if err != nil {
		rec.Status = http.StatusBadGateway
		var pe *provider.Error
		if errors.As(err, &pe) {
			rec.Status = pe.Status
		}
		rec.Error = err.Error()
		log.Printf("[custom] %s failed after %dms: %v", rec.Model, rec.DurationMs, err)
	}
	s.ring.Add(rec)
}
