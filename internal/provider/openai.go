package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"agbridge/internal/config"
	"agbridge/internal/gem"
)

type oaMessage struct {
	Role             string       `json:"role"`
	Content          any          `json:"content"`
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func openAIURL(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/chat/completions") {
		return b
	}
	return b + "/chat/completions"
}

func isOfficialOpenAI(base string) bool { return strings.Contains(base, "api.openai.com") }

// buildOpenAI translates a Gemini request into a chat.completions body.
func buildOpenAI(c *Call, r *gem.Request) map[string]any {
	images := boolOr(c.Model.SupportsImages, true)
	sendReasoning := !isOfficialOpenAI(c.Provider.BaseURL)
	var msgs []oaMessage
	if sys := gem.SystemText(r.SystemInstruction); sys != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: sys})
	}
	ids := newCallIDs()
	for _, content := range r.Contents {
		if content.Role == "model" {
			m := oaMessage{Role: "assistant"}
			var text, reasoning strings.Builder
			for _, p := range content.Parts {
				switch {
				case p.FunctionCall != nil:
					args, _ := json.Marshal(p.FunctionCall.Args)
					if p.FunctionCall.Args == nil {
						args = []byte("{}")
					}
					tc := oaToolCall{ID: ids.call(p.FunctionCall), Type: "function"}
					tc.Function.Name = p.FunctionCall.Name
					tc.Function.Arguments = string(args)
					m.ToolCalls = append(m.ToolCalls, tc)
				case p.Thought:
					reasoning.WriteString(p.Text)
				case p.Text != "":
					text.WriteString(p.Text)
				}
			}
			if text.Len() > 0 {
				m.Content = text.String()
			}
			if sendReasoning {
				m.ReasoningContent = reasoning.String()
			}
			if m.Content == nil && len(m.ToolCalls) == 0 {
				continue
			}
			msgs = append(msgs, m)
			continue
		}
		// Tool replies must directly follow the assistant turn, so they are
		// emitted first and the user's own text/images after them.
		var blocks []map[string]any
		var texts []string
		hasImage := false
		text := func(s string) {
			texts = append(texts, s)
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
		}
		for _, p := range content.Parts {
			switch {
			case p.FunctionResponse != nil:
				id := ids.response(p.FunctionResponse)
				result := toolResultText(p.FunctionResponse.Response)
				if id == "" {
					text(fmt.Sprintf("[tool %s result]\n%s", p.FunctionResponse.Name, result))
					continue
				}
				msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: id, Content: result})
			case p.InlineData != nil && images && strings.HasPrefix(p.InlineData.MimeType, "image/"):
				hasImage = true
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:" + p.InlineData.MimeType + ";base64," + p.InlineData.Data}})
			case p.InlineData != nil:
				text(fmt.Sprintf("[附件 %s 已省略：该模型不支持此类型]", p.InlineData.MimeType))
			case p.FileData != nil:
				text("[文件 " + p.FileData.FileURI + "]")
			case p.Text != "" && !p.Thought:
				text(p.Text)
			}
		}
		switch {
		case hasImage:
			msgs = append(msgs, oaMessage{Role: "user", Content: blocks})
		case len(texts) > 0:
			msgs = append(msgs, oaMessage{Role: "user", Content: strings.Join(texts, "\n")})
		}
	}
	msgs = repairToolPairs(msgs)

	body := map[string]any{
		"model":    c.Model.UpstreamModel,
		"messages": msgs,
		"stream":   true,
	}
	body["stream_options"] = map[string]any{"include_usage": true}
	if g := r.GenerationConfig; g != nil {
		if g.Temperature != nil {
			body["temperature"] = *g.Temperature
		}
		if g.TopP != nil {
			body["top_p"] = *g.TopP
		}
		if len(g.StopSequences) > 0 {
			body["stop"] = g.StopSequences
		}
	}
	if n := maxOut(c, r, 0); n > 0 {
		if isOfficialOpenAI(c.Provider.BaseURL) {
			body["max_completion_tokens"] = n
		} else {
			body["max_tokens"] = n
		}
	}
	if c.Model.ReasoningEffort != "" {
		body["reasoning_effort"] = c.Model.ReasoningEffort
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		for i := range t.FunctionDeclarations {
			fd := &t.FunctionDeclarations[i]
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": fd.Name, "description": fd.Description, "parameters": toolParameters(fd)}})
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
		if tc := r.ToolConfig; tc != nil && tc.FunctionCallingConfig != nil {
			switch tc.FunctionCallingConfig.Mode {
			case "ANY":
				if names := tc.FunctionCallingConfig.AllowedFunctionNames; len(names) == 1 {
					body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": names[0]}}
				} else {
					body["tool_choice"] = "required"
				}
			case "NONE":
				body["tool_choice"] = "none"
			}
		}
	}
	return body
}

// repairToolPairs makes the transcript valid for chat APIs: every assistant
// tool call gets a tool reply directly after it, and stray replies become text.
func repairToolPairs(in []oaMessage) []oaMessage {
	var out []oaMessage
	for i := 0; i < len(in); i++ {
		m := in[i]
		if m.Role == "tool" {
			// A tool reply not directly following its call: keep as user text.
			out = append(out, oaMessage{Role: "user", Content: "[tool result]\n" + fmt.Sprint(m.Content)})
			continue
		}
		out = append(out, m)
		if len(m.ToolCalls) == 0 {
			continue
		}
		replies := map[string]oaMessage{}
		j := i + 1
		var extra []oaMessage
		for ; j < len(in) && in[j].Role == "tool"; j++ {
			replies[in[j].ToolCallID] = in[j]
		}
		for _, tc := range m.ToolCalls {
			if r, ok := replies[tc.ID]; ok {
				out = append(out, r)
				delete(replies, tc.ID)
			} else {
				out = append(out, oaMessage{Role: "tool", ToolCallID: tc.ID, Content: "(no result: the call was interrupted)"})
			}
		}
		for _, r := range replies {
			extra = append(extra, oaMessage{Role: "user", Content: "[tool result]\n" + fmt.Sprint(r.Content)})
		}
		out = append(out, extra...)
		i = j - 1
	}
	return out
}

func generateOpenAI(ctx context.Context, c *Call, emit gem.Emitter) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(c.Provider))
	defer cancel()
	r, err := typedRequest(c.Body)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(buildOpenAI(c, r))
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIURL(c.Provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.Provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.Provider.APIKey)
	}
	applyHeaders(req, c.Provider)
	resp, err := do(ctx, client(c.Provider, c.GlobalProxy), req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	st := &oaStream{emit: emit, calls: map[int]*oaToolCall{}}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = gem.ReadSSE(resp.Body, st.event)
	} else {
		// Some endpoints ignore stream=true and answer with one JSON object.
		var b []byte
		if b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err == nil {
			err = st.whole(b)
		}
	}
	if err == nil {
		err = st.finish()
	}
	return st.res, err
}

type oaStream struct {
	emit     gem.Emitter
	calls    map[int]*oaToolCall
	finished string
	usage    *gem.Usage
	res      Result
	any      bool
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string      `json:"content"`
			ReasoningContent *string      `json:"reasoning_content"`
			Reasoning        *string      `json:"reasoning"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message *struct {
			Content          *string      `json:"content"`
			ReasoningContent *string      `json:"reasoning_content"`
			ToolCalls        []oaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens      int `json:"prompt_tokens"`
		CompletionTokens  int `json:"completion_tokens"`
		TotalTokens       int `json:"total_tokens"`
		CompletionDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		PromptDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func (s *oaStream) event(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[DONE]" {
		return nil
	}
	var ch oaChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了无法解析的数据"}
	}
	if len(ch.Error) > 0 && string(ch.Error) != "null" {
		return &Error{Status: http.StatusBadGateway, Message: upstreamMessage(data)}
	}
	s.takeUsage(&ch)
	for _, choice := range ch.Choices {
		d := choice.Delta
		var parts []gem.Part
		if r := firstNonEmpty(d.ReasoningContent, d.Reasoning); r != "" {
			parts = append(parts, gem.Part{Text: r, Thought: true})
		}
		if d.Content != nil && *d.Content != "" {
			parts = append(parts, gem.Part{Text: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			cur, ok := s.calls[idx]
			if !ok {
				cur = &oaToolCall{}
				s.calls[idx] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Function.Name += tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
		if len(parts) > 0 {
			s.any = true
			if err := s.emit(chunk(parts, "", nil)); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finished = *choice.FinishReason
		}
	}
	return nil
}

func (s *oaStream) whole(b []byte) error {
	var ch oaChunk
	if err := json.Unmarshal(b, &ch); err != nil {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了无法解析的数据"}
	}
	if len(ch.Error) > 0 && string(ch.Error) != "null" {
		return &Error{Status: http.StatusBadGateway, Message: upstreamMessage(b)}
	}
	s.takeUsage(&ch)
	for _, choice := range ch.Choices {
		if choice.Message == nil {
			continue
		}
		var parts []gem.Part
		if choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
			parts = append(parts, gem.Part{Text: *choice.Message.ReasoningContent, Thought: true})
		}
		if choice.Message.Content != nil && *choice.Message.Content != "" {
			parts = append(parts, gem.Part{Text: *choice.Message.Content})
		}
		for i, tc := range choice.Message.ToolCalls {
			cp := tc
			s.calls[i] = &cp
		}
		if len(parts) > 0 {
			s.any = true
			if err := s.emit(chunk(parts, "", nil)); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			s.finished = *choice.FinishReason
		}
	}
	return nil
}

func (s *oaStream) takeUsage(ch *oaChunk) {
	if ch.Usage == nil {
		return
	}
	u := &gem.Usage{
		PromptTokenCount:     ch.Usage.PromptTokens,
		CandidatesTokenCount: ch.Usage.CompletionTokens,
		TotalTokenCount:      ch.Usage.TotalTokens,
	}
	if d := ch.Usage.CompletionDetails; d != nil && d.ReasoningTokens > 0 {
		u.ThoughtsTokenCount = d.ReasoningTokens
		u.CandidatesTokenCount -= d.ReasoningTokens
	}
	if d := ch.Usage.PromptDetails; d != nil {
		u.CachedContentTokenCount = d.CachedTokens
	}
	s.usage = u
	s.res = Result{InTokens: ch.Usage.PromptTokens, OutTokens: ch.Usage.CompletionTokens}
}

// finish emits accumulated tool calls and the closing chunk.
func (s *oaStream) finish() error {
	var parts []gem.Part
	idx := make([]int, 0, len(s.calls))
	for i := range s.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		tc := s.calls[i]
		if tc.Function.Name == "" {
			continue
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_agb_r%d", i)
		}
		parts = append(parts, gem.Part{FunctionCall: &gem.FunctionCall{ID: id, Name: tc.Function.Name, Args: parseArgs(tc.Function.Arguments)}})
	}
	if !s.any && len(parts) == 0 {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了空响应"}
	}
	return s.emit(chunk(parts, finishFromOpenAI(s.finished), s.usage))
}

func firstNonEmpty(vals ...*string) string {
	for _, v := range vals {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

func listOpenAI(ctx context.Context, p *config.Provider, global string) ([]string, error) {
	base := strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	applyHeaders(req, p)
	return listJSON(ctx, client(p, global), req, func(b []byte) ([]string, error) {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		var out []string
		for _, m := range v.Data {
			out = append(out, m.ID)
		}
		for _, m := range v.Models { // Ollama native style
			out = append(out, m.Name)
		}
		return out, nil
	})
}
