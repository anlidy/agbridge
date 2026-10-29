package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"agbridge/internal/config"
	"agbridge/internal/gem"
)

// Fields of GenerateContentRequest accepted by the public Gemini API. Cloud Code
// adds envelope-only fields (sessionId, labels, ...) that the public API rejects.
var geminiRequestFields = map[string]bool{
	"contents": true, "systemInstruction": true, "tools": true, "toolConfig": true,
	"generationConfig": true, "safetySettings": true, "cachedContent": true,
}

// DummyThoughtSignature is accepted by Gemini in place of a real signature for
// function calls that the current upstream did not generate itself.
const DummyThoughtSignature = "skip_thought_signature_validator"

var versionSuffix = regexp.MustCompile(`/v1(alpha|beta[0-9]*)?$`)

// geminiBase returns the API root including its version, e.g. https://host/v1beta.
func geminiBase(raw string) string {
	base := strings.TrimRight(raw, "/")
	base = strings.TrimSuffix(base, "/models")
	if versionSuffix.MatchString(base) {
		return base
	}
	return base + "/v1beta"
}

func geminiRequest(ctx context.Context, p *config.Provider, method, u string, body []byte) (*http.Request, error) {
	if p.AuthMode == "query" && p.APIKey != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		q := parsed.Query()
		q.Set("key", p.APIKey)
		parsed.RawQuery = q.Encode()
		u = parsed.String()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.APIKey != "" {
		switch p.AuthMode {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		case "query":
		default:
			req.Header.Set("x-goog-api-key", p.APIKey)
		}
	}
	applyHeaders(req, p)
	return req, nil
}

// prepareGeminiBody keeps public API fields and applies the signature policy.
func prepareGeminiBody(body map[string]any, sigMode string) map[string]any {
	out := make(map[string]any, len(body))
	for k, v := range body {
		if geminiRequestFields[k] {
			out[k] = v
		}
	}
	if sigMode == "keep" || sigMode == "" {
		return out
	}
	contents, _ := out["contents"].([]any)
	for _, c := range contents {
		cm, _ := c.(map[string]any)
		parts, _ := cm["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			_, isCall := pm["functionCall"]
			switch {
			case sigMode == "dummy" && isCall:
				pm["thoughtSignature"] = DummyThoughtSignature
			default:
				delete(pm, "thoughtSignature")
			}
		}
	}
	return out
}

// thinkStyle is how an upstream takes thinking depth: Gemini 3+ wants a
// thinkingLevel, 2.5 a numeric thinkingBudget, and each rejects the other.
type thinkStyle int

const (
	styleLevel thinkStyle = iota
	styleBudget
)

// learnedStyle remembers, per provider and upstream model, the style that
// worked after the first one was rejected.
var learnedStyle sync.Map

var (
	budgetRejected = regexp.MustCompile(`thinking[ _]?budget (is )?not supported|unknown name "thinkingbudget"`)
	levelRejected  = regexp.MustCompile(`thinking[ _]?level (is )?not supported|unknown name "thinkinglevel"`)
)

// geminiAttempt is the request shape of one try.
type geminiAttempt struct {
	sig        string // thought signature policy: keep, dummy or strip
	style      thinkStyle
	noThoughts bool // drop includeThoughts
}

func generateGemini(ctx context.Context, c *Call, emit gem.Emitter) (Result, error) {
	sig := c.Model.ThoughtSignature
	if sig == "" {
		sig = "auto"
	}
	key := c.Provider.ID + "\x00" + c.Model.UpstreamModel
	a := geminiAttempt{sig: sig}
	if sig == "auto" {
		a.sig = "keep"
	}
	if v, ok := learnedStyle.Load(key); ok {
		a.style = v.(thinkStyle)
	}
	for tries := 0; ; tries++ {
		res, emitted, err := geminiOnce(ctx, c, emit, a)
		if err == nil || emitted || tries == 3 {
			return res, err
		}
		next, ok := a.adjust(err, sig == "auto")
		if !ok {
			return res, err
		}
		if next.style != a.style {
			learnedStyle.Store(key, next.style)
		}
		a = next
	}
}

// adjust picks the next attempt after a 400 caused by the request's shape.
func (a geminiAttempt) adjust(err error, autoSig bool) (geminiAttempt, bool) {
	e, ok := err.(*Error)
	if !ok || e.Status != http.StatusBadRequest {
		return a, false
	}
	m := strings.ToLower(e.Message)
	switch {
	// Signatures from another backend (e.g. a native Antigravity turn earlier
	// in the conversation) can be rejected; dummy ones are always accepted.
	case autoSig && a.sig == "keep" && (strings.Contains(m, "thought_signature") || strings.Contains(m, "thought signature") || strings.Contains(m, "thoughtsignature")):
		a.sig = "dummy"
	case a.style == styleBudget && budgetRejected.MatchString(m):
		a.style = styleLevel
	case a.style == styleLevel && levelRejected.MatchString(m):
		a.style = styleBudget
	case !a.noThoughts && strings.Contains(m, "include_thoughts"):
		a.noThoughts = true
	default:
		return a, false
	}
	return a, true
}

// applyThinking rewrites generationConfig.thinkingConfig for the upstream.
// Antigravity derives it from catalog fields custom models do not have, so it
// asks for thinkingBudget 0; the depth comes from the model's reasoning effort
// instead, else from a level or nonzero budget the request carries.
func applyThinking(body map[string]any, m *config.Model, a geminiAttempt) {
	gc, _ := body["generationConfig"].(map[string]any)
	if !boolOr(m.SupportsThink, true) {
		if gc != nil {
			delete(gc, "thinkingConfig")
		}
		return
	}
	if gc == nil {
		gc = map[string]any{}
		body["generationConfig"] = gc
	}
	in, _ := gc["thinkingConfig"].(map[string]any)
	level := strings.ToLower(m.ReasoningEffort)
	if s, ok := in["thinkingLevel"].(string); ok && level == "" {
		level = strings.ToLower(s)
	}
	budget, hasBudget := 0, false
	if f, ok := in["thinkingBudget"].(float64); ok && f != 0 {
		budget, hasBudget = int(f), true
	}
	tc := map[string]any{}
	off := level == "minimal"
	if a.style == styleBudget {
		if level != "" {
			budget, hasBudget = effortBudget[level], true
		}
		if hasBudget {
			tc["thinkingBudget"] = budget
			off = budget == 0
		}
	} else {
		if level == "" && hasBudget {
			level = budgetLevel(budget)
		}
		if level != "" {
			tc["thinkingLevel"] = strings.ToUpper(level)
		}
	}
	if !off && !a.noThoughts {
		tc["includeThoughts"] = true
	}
	if len(tc) == 0 {
		delete(gc, "thinkingConfig")
	} else {
		gc["thinkingConfig"] = tc
	}
}

func budgetLevel(b int) string {
	switch {
	case b < 0: // dynamic
		return "high"
	case b <= 4096:
		return "low"
	case b <= 16384:
		return "medium"
	}
	return "high"
}

func geminiOnce(ctx context.Context, c *Call, emit gem.Emitter, a geminiAttempt) (Result, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(c.Provider))
	defer cancel()
	prepared := prepareGeminiBody(cloneBody(c.Body), a.sig)
	applyThinking(prepared, c.Model, a)
	body, err := json.Marshal(prepared)
	if err != nil {
		return Result{}, false, err
	}
	model := url.PathEscape(strings.TrimPrefix(c.Model.UpstreamModel, "models/"))
	u := geminiBase(c.Provider.BaseURL) + "/models/" + model
	if c.Stream {
		u += ":streamGenerateContent?alt=sse"
	} else {
		u += ":generateContent"
	}
	req, err := geminiRequest(ctx, c.Provider, http.MethodPost, u, body)
	if err != nil {
		return Result{}, false, err
	}
	resp, err := do(ctx, client(c.Provider, c.GlobalProxy), req)
	if err != nil {
		return Result{}, false, err
	}
	defer resp.Body.Close()

	var res Result
	emitted := false
	handle := func(data []byte) error {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || string(data) == "[DONE]" {
			return nil
		}
		var probe struct {
			Response      json.RawMessage `json:"response"`
			Error         json.RawMessage `json:"error"`
			UsageMetadata *gem.Usage      `json:"usageMetadata"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			return &Error{Status: http.StatusBadGateway, Message: "上游返回了无法解析的数据"}
		}
		if len(probe.Error) > 0 {
			return &Error{Status: http.StatusBadGateway, Message: upstreamMessage(data)}
		}
		// Some relays speak the Cloud Code envelope; unwrap it.
		if len(probe.Response) > 0 {
			data = probe.Response
			probe.UsageMetadata = nil
			var inner struct {
				UsageMetadata *gem.Usage `json:"usageMetadata"`
			}
			_ = json.Unmarshal(data, &inner)
			probe.UsageMetadata = inner.UsageMetadata
		}
		if u := probe.UsageMetadata; u != nil {
			res.InTokens, res.OutTokens = u.PromptTokenCount, u.CandidatesTokenCount+u.ThoughtsTokenCount
		}
		emitted = true
		return emit(json.RawMessage(data))
	}
	if c.Stream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = gem.ReadSSE(resp.Body, handle)
	} else {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err == nil {
			trimmed := bytes.TrimSpace(b)
			if len(trimmed) > 0 && trimmed[0] == '[' {
				// streamGenerateContent without alt=sse returns a JSON array.
				var arr []json.RawMessage
				if err = json.Unmarshal(trimmed, &arr); err == nil {
					for _, item := range arr {
						if err = handle(item); err != nil {
							break
						}
					}
				}
			} else {
				err = handle(trimmed)
			}
		}
	}
	return res, emitted, err
}

func cloneBody(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func listGemini(ctx context.Context, p *config.Provider, global string) ([]string, error) {
	req, err := geminiRequest(ctx, p, http.MethodGet, geminiBase(p.BaseURL)+"/models?pageSize=1000", nil)
	if err != nil {
		return nil, err
	}
	return listJSON(ctx, client(p, global), req, func(b []byte) ([]string, error) {
		var v struct {
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		var out []string
		for _, m := range v.Models {
			if len(m.Methods) > 0 && !contains(m.Methods, "generateContent") {
				continue
			}
			out = append(out, strings.TrimPrefix(m.Name, "models/"))
		}
		for _, m := range v.Data { // OpenAI-style listing from some relays
			out = append(out, m.ID)
		}
		return out, nil
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
