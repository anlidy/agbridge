package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agbridge/internal/applog"
	"agbridge/internal/config"
)

type env struct {
	t      *testing.T
	store  *config.Store
	srv    *Server
	proxy  *httptest.Server
	google *httptest.Server
	native []string // bodies the fake Google received for generation
}

func newEnv(t *testing.T, third http.Handler) *env {
	e := &env{t: t}
	e.google = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer google-token" {
			http.Error(w, `{"error":{"code":401}}`, 401)
			return
		}
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			io.WriteString(w, `{"models":{"gemini-3-pro":{"displayName":"Gemini 3 Pro","model":"MODEL_PLACEHOLDER_M1650"},
				"claude":{"displayName":"Claude","model":"MODEL_CLAUDE_4_5_SONNET"}},
				"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-3-pro","claude"]}]}]}`)
		case "/v1internal:streamGenerateContent":
			b, _ := io.ReadAll(r.Body)
			e.native = append(e.native, string(b))
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"native\"}]}}]}}\n\n")
		case "/v1internal:loadCodeAssist":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"endpoint":"https://cloudcode-pa.googleapis.com/x","paidTier":{"id":"g1-ultra","usesGcpTos":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	third3 := httptest.NewServer(third)
	t.Cleanup(func() { e.google.Close(); third3.Close() })

	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(c *config.Config) error {
		c.Proxy = "direct"
		c.Upstreams.CloudCode = e.google.URL
		c.Providers = []config.Provider{{ID: "p1", Name: "Relay", Type: config.TypeGemini, BaseURL: third3.URL, APIKey: "k"}}
		c.Models = []config.Model{{ID: "m1", ProviderID: "p1", UpstreamModel: "gemini-2.5-pro", DisplayName: "Gemini 3 Pro", Enabled: true}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.store = store
	e.srv = New(store, applog.NewRing(10), Hooks{}, "test")
	e.proxy = httptest.NewServer(e.srv)
	t.Cleanup(e.proxy.Close)
	return e
}

func (e *env) post(path, body string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, e.proxy.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer google-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func fakeGemini(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := strings.HasSuffix(r.URL.Path, "/models/gemini-2.5-pro:streamGenerateContent")
		if !stream && !strings.HasSuffix(r.URL.Path, "/models/gemini-2.5-pro:generateContent") {
			t.Errorf("third-party path %s", r.URL.Path)
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		for k := range b {
			if k != "contents" && k != "generationConfig" && k != "tools" {
				t.Errorf("unexpected field %q sent to third party", k)
			}
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello world"}]},"finishReason":"STOP"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}]}\n\n")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" world\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	})
}

func TestCatalogInjection(t *testing.T) {
	e := newEnv(t, fakeGemini(t))
	resp := e.post("/v1internal:fetchAvailableModels", `{"project":"p"}`)
	defer resp.Body.Close()
	var cat map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	models := cat["models"].(map[string]any)
	entry, ok := models["agb-m1"].(map[string]any)
	if !ok {
		t.Fatalf("custom model missing: %v", models)
	}
	if entry["model"] == "MODEL_PLACEHOLDER_M1650" {
		t.Fatal("custom model reused a native placeholder")
	}
	if entry["displayName"] != "Gemini 3 Pro · Relay" {
		t.Fatalf("label should be made unique, got %v", entry["displayName"])
	}
	ids := cat["agentModelSorts"].([]any)[0].(map[string]any)["groups"].([]any)[0].(map[string]any)["modelIds"].([]any)
	if ids[len(ids)-1] != "agb-m1" {
		t.Fatalf("not in picker group: %v", ids)
	}
	if m := e.store.Get().Model("m1"); m.Placeholder == nil || *m.Placeholder == 1650 {
		t.Fatalf("placeholder not persisted correctly: %v", m.Placeholder)
	}
	if e.srv.CatalogStale() {
		t.Fatal("catalog reported stale right after injection")
	}
	if err := e.store.Update(func(c *config.Config) error {
		c.Models = append(c.Models, config.Model{ID: "m2", ProviderID: c.Models[0].ProviderID, UpstreamModel: "x", Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !e.srv.CatalogStale() {
		t.Fatal("new model should mark the served catalog stale")
	}

	// Upstream auth errors pass through untouched so the language server refreshes its token.
	req, _ := http.NewRequest(http.MethodPost, e.proxy.URL+"/v1internal:fetchAvailableModels", strings.NewReader("{}"))
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 401 {
		t.Fatalf("status = %d", r2.StatusCode)
	}
}

func TestCustomGenerationIsWrapped(t *testing.T) {
	e := newEnv(t, fakeGemini(t))
	resp := e.post("/v1internal:streamGenerateContent?alt=sse", `{"model":"agb-m1","project":"p","requestId":"r",
		"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"sessionId":"s","generationConfig":{"maxOutputTokens":10}}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Response struct {
				Candidates []struct {
					Content struct {
						Parts []struct{ Text string } `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			t.Fatal(err)
		}
		for _, p := range ev.Response.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
	}
	if text.String() != "Hello world" {
		t.Fatalf("text = %q", text.String())
	}
	if len(e.native) != 0 {
		t.Fatal("custom request leaked to Google")
	}
}

func TestPlaceholderSelectorAndNativeForward(t *testing.T) {
	e := newEnv(t, fakeGemini(t))
	e.post("/v1internal:fetchAvailableModels", `{}`).Body.Close() // allocates placeholders
	ph := config.PlaceholderName(*e.store.Get().Model("m1").Placeholder)

	resp := e.post("/v1internal:generateContent", `{"model":"`+ph+`","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"Hello world"`) || !strings.Contains(string(b), `"response"`) {
		t.Fatalf("non-stream custom response = %s", b)
	}

	native := `{"model":"gemini-3-pro","request":{"contents":[]}}`
	resp = e.post("/v1internal:streamGenerateContent?alt=sse", native)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "native") || len(e.native) != 1 || e.native[0] != native {
		t.Fatalf("native forward broken: %s / %v", b, e.native)
	}
}

func TestResponseRewriteAndHostGuard(t *testing.T) {
	e := newEnv(t, fakeGemini(t))
	resp := e.post("/v1internal:loadCodeAssist", `{}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "http://127.0.0.1:50999/_agbp/x") {
		t.Fatalf("google URL not rewritten: %s", b)
	}
	h := http.Header{"Authorization": {"Bearer google-token"}}
	if got := e.srv.tierOrigin(h); got != "https://cloudcode-pa.googleapis.com" {
		t.Fatalf("GCP-ToS tier should route to production, got %q", got)
	}

	req, _ := http.NewRequest(http.MethodGet, e.proxy.URL+"/health", nil)
	req.Host = "evil.example.com"
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding host accepted: %d", r.StatusCode)
	}
	r, _ = http.Get(e.proxy.URL + "/api/state")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without token: %d", r.StatusCode)
	}
}

func TestThirdPartyAuthErrorIsNotUnauthorized(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":401,"message":"API key not valid"}}`, 401)
	}))
	resp := e.post("/v1internal:streamGenerateContent?alt=sse", `{"model":"agb-m1","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "API key not valid") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

func TestEffortEntries(t *testing.T) {
	var levels []any
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		tc, _ := b["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
		levels = append(levels, tc["thinkingLevel"])
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	e.post("/v1internal:fetchAvailableModels", `{}`).Body.Close() // m1 gets a plain placeholder first
	if err := e.store.Update(func(c *config.Config) error {
		c.Models[0].Efforts = []string{"high", "low"}
		config.AssignPlaceholders(c, nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := e.store.Get().Model("m1")
	if m.Placeholder != nil || len(m.EffortPlaceholders) != 2 {
		t.Fatalf("placeholders = %v %v", m.Placeholder, m.EffortPlaceholders)
	}

	resp := e.post("/v1internal:fetchAvailableModels", `{}`)
	var cat map[string]any
	json.NewDecoder(resp.Body).Decode(&cat)
	resp.Body.Close()
	models := cat["models"].(map[string]any)
	if _, ok := models["agb-m1"]; ok {
		t.Fatal("plain entry still listed next to effort entries")
	}
	low, _ := models["agb-m1-low"].(map[string]any)
	high, _ := models["agb-m1-high"].(map[string]any)
	if low == nil || high == nil || !strings.HasSuffix(low["displayName"].(string), " (Low)") || !strings.HasSuffix(high["displayName"].(string), " (High)") {
		t.Fatalf("effort entries = %v / %v", low, high)
	}
	ids := cat["agentModelSorts"].([]any)[0].(map[string]any)["groups"].([]any)[0].(map[string]any)["modelIds"].([]any)
	if fmt.Sprint(ids[len(ids)-2:]) != "[agb-m1-low agb-m1-high]" {
		t.Fatalf("picker order = %v", ids)
	}

	for _, sel := range []string{high["model"].(string), "agb-m1-low"} {
		resp := e.post("/v1internal:generateContent", `{"model":"`+sel+`","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}}`)
		resp.Body.Close()
	}
	if fmt.Sprint(levels) != "[HIGH LOW]" {
		t.Fatalf("thinking levels sent = %v", levels)
	}
}
