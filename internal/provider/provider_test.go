package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agbridge/internal/config"
	"agbridge/internal/gem"
)

func body(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const toolHistory = `{
  "systemInstruction": {"parts": [{"text": "You are helpful."}]},
  "contents": [
    {"role": "user", "parts": [{"text": "read a.txt"}]},
    {"role": "model", "parts": [{"text": "thinking", "thought": true}, {"functionCall": {"name": "view_file", "args": {"path": "a.txt"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"name": "view_file", "response": {"output": "hello"}}}]},
    {"role": "user", "parts": [{"text": "thanks"}]}
  ],
  "tools": [{"functionDeclarations": [{"name": "view_file", "description": "Read a file",
    "parameters": {"type": "OBJECT", "properties": {"path": {"type": "STRING"}}, "required": ["path"]}}]}],
  "generationConfig": {"maxOutputTokens": 1000, "temperature": 0.2}
}`

func TestBuildOpenAIPairsToolCalls(t *testing.T) {
	r, err := typedRequest(body(t, toolHistory))
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{Provider: &config.Provider{BaseURL: "https://api.deepseek.com/v1"}, Model: &config.Model{UpstreamModel: "deepseek-chat"}}
	out := buildOpenAI(c, r)
	msgs := out["messages"].([]oaMessage)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %s", got)
	}
	call := msgs[2].ToolCalls[0]
	if call.Function.Name != "view_file" || msgs[3].ToolCallID != call.ID || msgs[3].Content != "hello" {
		t.Fatalf("tool pairing broken: %+v / %+v", call, msgs[3])
	}
	if msgs[2].ReasoningContent != "thinking" {
		t.Fatalf("reasoning not replayed: %q", msgs[2].ReasoningContent)
	}
	params := out["tools"].([]map[string]any)[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"].(map[string]any)["path"].(map[string]any)["type"] != "string" {
		t.Fatalf("schema not converted: %v", params)
	}
	if out["max_tokens"] != 1000 {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
}

func TestOpenAIStreamToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, `{"error":{"message":"bad key"}}`, 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`,
			`{"choices":[{"delta":{"content":"Let me look."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"view_file","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`[DONE]`,
		} {
			io.WriteString(w, "data: "+ev+"\n\n")
		}
	}))
	defer srv.Close()
	c := &Call{
		Provider: &config.Provider{Type: config.TypeOpenAI, BaseURL: srv.URL + "/v1", APIKey: "sk-test", Proxy: "direct"},
		Model:    &config.Model{UpstreamModel: "x"},
		Body:     body(t, toolHistory),
		Stream:   true,
	}
	var m gem.Merger
	res, err := Generate(context.Background(), c, m.Add)
	if err != nil {
		t.Fatal(err)
	}
	parts := m.Result().Candidates[0].Content.Parts
	if len(parts) != 3 || !parts[0].Thought || parts[1].Text != "Let me look." || parts[2].FunctionCall == nil {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[2].FunctionCall.Args["path"] != "a.txt" || parts[2].FunctionCall.ID != "call_1" {
		t.Fatalf("call = %+v", parts[2].FunctionCall)
	}
	if res.InTokens != 10 || res.OutTokens != 5 {
		t.Fatalf("usage = %+v", res)
	}

	c.Provider.APIKey = "wrong"
	_, err = Generate(context.Background(), c, m.Add)
	if e, ok := err.(*Error); !ok || e.Status != 401 || e.Message != "bad key" {
		t.Fatalf("err = %v", err)
	}
}

func TestGeminiPassthroughAndSignatureRetry(t *testing.T) {
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-x:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("x-goog-api-key") != "AIza-test" {
			t.Errorf("missing api key header")
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, b)
		sig := b["contents"].([]any)[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["thoughtSignature"]
		if sig == "native-sig" {
			http.Error(w, `{"error":{"code":400,"message":"Thought signature is not valid."}}`, 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hi\"}]}}]}\r\n\r\n")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":2}}\r\n\r\n")
	}))
	defer srv.Close()
	c := &Call{
		Provider: &config.Provider{Type: config.TypeGemini, BaseURL: srv.URL, APIKey: "AIza-test", Proxy: "direct"},
		Model:    &config.Model{UpstreamModel: "models/gemini-x"},
		Body: body(t, `{"sessionId":"s1","labels":{"a":"b"},"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"native-sig"}]},
			{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`),
		Stream: true,
	}
	var chunks []string
	res, err := Generate(context.Background(), c, func(ch any) error {
		b, _ := json.Marshal(ch)
		chunks = append(chunks, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected a retry, got %d calls", len(calls))
	}
	if _, ok := calls[0]["sessionId"]; ok {
		t.Fatal("cloud code field leaked to public API")
	}
	if len(chunks) != 2 || !strings.Contains(chunks[0], `"Hi"`) || res.InTokens != 7 {
		t.Fatalf("chunks = %v res = %+v", chunks, res)
	}
}

func TestBuildAnthropicMergesRolesAndFillsResults(t *testing.T) {
	r, _ := typedRequest(body(t, `{"contents":[
		{"role":"model","parts":[{"functionCall":{"id":"t1","name":"a","args":{}}},{"functionCall":{"id":"t2","name":"b","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"t1","name":"a","response":{"output":"ok"}}}]},
		{"role":"user","parts":[{"text":"go on"}]}]}`))
	out := buildAnthropic(&Call{Provider: &config.Provider{}, Model: &config.Model{UpstreamModel: "claude"}}, r, false)
	msgs := out["messages"].([]anMsg)
	if len(msgs) != 3 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[2].Role != "user" {
		t.Fatalf("msgs = %+v", msgs)
	}
	user := msgs[2].Content
	if len(user) != 3 || user[0]["tool_use_id"] != "t1" || user[1]["tool_use_id"] != "t2" || user[2]["text"] != "go on" {
		t.Fatalf("user blocks = %+v", user)
	}
}

func TestGeminiBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://generativelanguage.googleapis.com":      "https://generativelanguage.googleapis.com/v1beta",
		"https://relay.example.com/v1beta/":              "https://relay.example.com/v1beta",
		"https://relay.example.com/gemini/v1beta/models": "https://relay.example.com/gemini/v1beta",
		"https://relay.example.com/v1":                   "https://relay.example.com/v1",
	} {
		if got := geminiBase(in); got != want {
			t.Errorf("geminiBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGeminiThinkingTranslation(t *testing.T) {
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		tc, _ := b["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
		calls = append(calls, tc)
		if _, ok := tc["thinkingLevel"]; ok { // a 2.5-style model
			http.Error(w, `{"error":{"code":400,"message":"Thinking level is not supported for this model."}}`, 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer srv.Close()
	call := func(effort string) {
		t.Helper()
		c := &Call{
			Provider: &config.Provider{ID: "p", Type: config.TypeGemini, BaseURL: srv.URL, Proxy: "direct"},
			Model:    &config.Model{UpstreamModel: "gemini-2.5-x", ReasoningEffort: effort},
			Body: body(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
				"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`),
			Stream: true,
		}
		if _, err := Generate(context.Background(), c, func(any) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	call("medium")
	if len(calls) != 2 || calls[0]["thinkingLevel"] != "MEDIUM" || calls[1]["thinkingBudget"] != 8192.0 || calls[1]["includeThoughts"] != true {
		t.Fatalf("calls = %v", calls)
	}
	// The budget style is remembered for this model.
	calls = nil
	call("high")
	if len(calls) != 1 || calls[0]["thinkingBudget"] != 24576.0 {
		t.Fatalf("calls = %v", calls)
	}

	// Antigravity's budget 0 for custom models means "unspecified", not "off".
	b := map[string]any{"generationConfig": map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true, "thinkingBudget": 0.0}}}
	applyThinking(b, &config.Model{}, geminiAttempt{})
	if tc := b["generationConfig"].(map[string]any)["thinkingConfig"]; !reflect.DeepEqual(tc, map[string]any{"includeThoughts": true}) {
		t.Fatalf("thinkingConfig = %v", tc)
	}
	a, ok := geminiAttempt{}.adjust(&Error{Status: 400, Message: "Thinking_config.include_thoughts is only enabled when thinking is enabled."}, true)
	if !ok || !a.noThoughts {
		t.Fatal("include_thoughts rejection not handled")
	}
}
