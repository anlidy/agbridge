// Package provider sends Antigravity's Gemini-format requests to third-party
// model APIs and streams the answers back as Gemini response chunks.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agbridge/internal/config"
	"agbridge/internal/gem"
	"agbridge/internal/netx"
)

// Call is one generation routed to a custom model.
type Call struct {
	Provider *config.Provider
	Model    *config.Model
	// Body is the inner Gemini GenerateContentRequest (Cloud Code envelope removed).
	Body        map[string]any
	Stream      bool
	GlobalProxy string
}

// Result carries accounting for the activity log.
type Result struct {
	InTokens  int
	OutTokens int
}

// Error is an upstream failure with an HTTP status suitable for Antigravity.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// Generate dispatches on the provider type.
func Generate(ctx context.Context, c *Call, emit gem.Emitter) (Result, error) {
	switch c.Provider.Type {
	case config.TypeGemini:
		return generateGemini(ctx, c, emit)
	case config.TypeOpenAI:
		return generateOpenAI(ctx, c, emit)
	case config.TypeAnthropic:
		return generateAnthropic(ctx, c, emit)
	}
	return Result{}, &Error{Status: 500, Message: "unknown provider type " + c.Provider.Type}
}

// ListModels queries the provider's model catalog (also used as a connection test).
func ListModels(ctx context.Context, p *config.Provider, globalProxy string) ([]string, error) {
	switch p.Type {
	case config.TypeGemini:
		return listGemini(ctx, p, globalProxy)
	case config.TypeOpenAI:
		return listOpenAI(ctx, p, globalProxy)
	case config.TypeAnthropic:
		return listAnthropic(ctx, p, globalProxy)
	}
	return nil, fmt.Errorf("unknown provider type %s", p.Type)
}

func client(p *config.Provider, global string) *http.Client {
	setting := p.Proxy
	if setting == "" {
		setting = global
	}
	return netx.Client(setting)
}

func timeout(p *config.Provider) time.Duration {
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return 10 * time.Minute
}

func applyHeaders(req *http.Request, p *config.Provider) {
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

// do sends req and converts non-2xx answers into *Error with a short message.
func do(ctx context.Context, c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Status: http.StatusBadGateway, Message: "连接上游失败: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		return nil, &Error{Status: resp.StatusCode, Message: upstreamMessage(b)}
	}
	return resp, nil
}

func upstreamMessage(b []byte) string {
	var v struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil && v.Error != nil {
		switch e := v.Error.(type) {
		case string:
			return e
		case map[string]any:
			if m, ok := e["message"].(string); ok {
				return m
			}
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	if s == "" {
		s = "(empty body)"
	}
	return s
}

func listJSON(ctx context.Context, c *http.Client, req *http.Request, extract func([]byte) ([]string, error)) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := do(ctx, c, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return extract(b)
}

func joinURL(base, suffix string) string {
	return strings.TrimRight(base, "/") + suffix
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// typedRequest converts the raw body into the typed view used by translators.
func typedRequest(body map[string]any) (*gem.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var r gem.Request
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, &Error{Status: 400, Message: "无法解析 Gemini 请求: " + err.Error()}
	}
	return &r, nil
}

func maxOut(c *Call, r *gem.Request, def int) int {
	n := 0
	if r.GenerationConfig != nil && r.GenerationConfig.MaxOutputTokens != nil {
		n = *r.GenerationConfig.MaxOutputTokens
	}
	if c.Model.MaxOutputTokens > 0 && (n == 0 || n > c.Model.MaxOutputTokens) {
		n = c.Model.MaxOutputTokens
	}
	if n == 0 {
		n = def
	}
	return n
}

// toolResultText renders a functionResponse payload for chat-style APIs.
func toolResultText(resp map[string]any) string {
	if len(resp) == 1 {
		for _, k := range []string{"output", "content", "result"} {
			if s, ok := resp[k].(string); ok {
				return s
			}
		}
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// parseArgs decodes streamed tool arguments, tolerating empty input.
func parseArgs(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err == nil && m != nil {
		return m
	}
	return map[string]any{"_raw_arguments": s}
}

// callIDs assigns stable IDs to function calls and pairs responses with them,
// since Gemini history may omit IDs that chat APIs require.
type callIDs struct {
	pending map[string][]string
	known   map[string]bool
	seq     int
}

func newCallIDs() *callIDs {
	return &callIDs{pending: map[string][]string{}, known: map[string]bool{}}
}

func (c *callIDs) call(fc *gem.FunctionCall) string {
	id := fc.ID
	if id == "" || c.known[id] {
		c.seq++
		id = fmt.Sprintf("call_agb_%d", c.seq)
	}
	c.known[id] = true
	c.pending[fc.Name] = append(c.pending[fc.Name], id)
	return id
}

// response returns the call ID for a function response, or "" if unmatched.
func (c *callIDs) response(fr *gem.FunctionResponse) string {
	q := c.pending[fr.Name]
	if fr.ID != "" {
		for i, id := range q {
			if id == fr.ID {
				c.pending[fr.Name] = append(q[:i:i], q[i+1:]...)
				return id
			}
		}
	}
	if len(q) == 0 {
		return ""
	}
	c.pending[fr.Name] = q[1:]
	return q[0]
}

func finishFromOpenAI(r string) string {
	switch r {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func chunk(parts []gem.Part, finish string, usage *gem.Usage) *gem.Response {
	if parts == nil {
		parts = []gem.Part{}
	}
	return &gem.Response{
		Candidates:    []gem.Candidate{{Content: gem.Content{Role: "model", Parts: parts}, FinishReason: finish}},
		UsageMetadata: usage,
	}
}
