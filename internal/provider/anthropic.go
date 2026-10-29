package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agbridge/internal/config"
	"agbridge/internal/gem"
)

func anthropicURL(base string) string {
	b := strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(b, "/messages"):
		return b
	case strings.HasSuffix(b, "/v1"):
		return b + "/messages"
	}
	return b + "/v1/messages"
}

type anMsg struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

var effortBudget = map[string]int{"low": 2048, "medium": 8192, "high": 24576}

func buildAnthropic(c *Call, r *gem.Request, thinking bool) map[string]any {
	images := boolOr(c.Model.SupportsImages, true)
	var msgs []anMsg
	add := func(role string, blocks ...map[string]any) {
		if len(blocks) == 0 {
			return
		}
		// Anthropic requires alternating roles; merge consecutive turns.
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, anMsg{Role: role, Content: blocks})
	}
	ids := newCallIDs()
	open := map[string]bool{} // tool_use ids awaiting a result
	for _, content := range r.Contents {
		if content.Role == "model" {
			var blocks []map[string]any
			var thought strings.Builder
			for _, p := range content.Parts {
				switch {
				case p.Thought:
					// Thinking text may be split over several parts, with the
					// signature arriving on the last one.
					thought.WriteString(p.Text)
					if p.ThoughtSignature != "" {
						if thinking {
							blocks = append(blocks, map[string]any{"type": "thinking", "thinking": thought.String(), "signature": p.ThoughtSignature})
						}
						thought.Reset()
					}
				case p.FunctionCall != nil:
					args := p.FunctionCall.Args
					if args == nil {
						args = map[string]any{}
					}
					id := ids.call(p.FunctionCall)
					open[id] = true
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": p.FunctionCall.Name, "input": args})
				case p.Text != "":
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				}
			}
			add("assistant", blocks...)
			continue
		}
		var results, rest []map[string]any
		for _, p := range content.Parts {
			switch {
			case p.FunctionResponse != nil:
				if id := ids.response(p.FunctionResponse); id != "" && open[id] {
					delete(open, id)
					results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id,
						"content": toolResultText(p.FunctionResponse.Response)})
				} else {
					rest = append(rest, map[string]any{"type": "text", "text": fmt.Sprintf("[tool %s result]\n%s",
						p.FunctionResponse.Name, toolResultText(p.FunctionResponse.Response))})
				}
			case p.InlineData != nil && images && strings.HasPrefix(p.InlineData.MimeType, "image/"):
				rest = append(rest, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": p.InlineData.MimeType, "data": p.InlineData.Data}})
			case p.InlineData != nil && p.InlineData.MimeType == "application/pdf":
				rest = append(rest, map[string]any{"type": "document", "source": map[string]any{
					"type": "base64", "media_type": p.InlineData.MimeType, "data": p.InlineData.Data}})
			case p.InlineData != nil:
				rest = append(rest, map[string]any{"type": "text", "text": "[附件 " + p.InlineData.MimeType + " 已省略]"})
			case p.FileData != nil:
				rest = append(rest, map[string]any{"type": "text", "text": "[文件 " + p.FileData.FileURI + "]"})
			case p.Text != "" && !p.Thought:
				rest = append(rest, map[string]any{"type": "text", "text": p.Text})
			}
		}
		// Every tool_use in the previous assistant turn needs a result here.
		if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
			for _, b := range msgs[n-1].Content {
				if b["type"] != "tool_use" {
					continue
				}
				id, _ := b["id"].(string)
				if open[id] {
					delete(open, id)
					results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id,
						"content": "(no result: the call was interrupted)", "is_error": true})
				}
			}
		}
		add("user", append(results, rest...)...)
	}
	if len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = append([]anMsg{{Role: "user", Content: []map[string]any{{"type": "text", "text": "(continue)"}}}}, msgs...)
	}

	maxTokens := maxOut(c, r, 8192)
	body := map[string]any{
		"model":      c.Model.UpstreamModel,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if sys := gem.SystemText(r.SystemInstruction); sys != "" {
		body["system"] = sys
	}
	if thinking {
		budget := effortBudget[c.Model.ReasoningEffort]
		if budget >= maxTokens {
			budget = maxTokens / 2
		}
		if budget >= 1024 {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		}
	} else if g := r.GenerationConfig; g != nil {
		// Sampling parameters are rejected together with extended thinking.
		if g.Temperature != nil {
			body["temperature"] = *g.Temperature
		}
		if g.TopP != nil {
			body["top_p"] = *g.TopP
		}
	}
	if g := r.GenerationConfig; g != nil && len(g.StopSequences) > 0 {
		body["stop_sequences"] = g.StopSequences
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		for i := range t.FunctionDeclarations {
			fd := &t.FunctionDeclarations[i]
			tools = append(tools, map[string]any{"name": fd.Name, "description": fd.Description, "input_schema": toolParameters(fd)})
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
		if tc := r.ToolConfig; tc != nil && tc.FunctionCallingConfig != nil {
			switch tc.FunctionCallingConfig.Mode {
			case "ANY":
				if names := tc.FunctionCallingConfig.AllowedFunctionNames; len(names) == 1 && !thinking {
					body["tool_choice"] = map[string]any{"type": "tool", "name": names[0]}
				} else if !thinking {
					body["tool_choice"] = map[string]any{"type": "any"}
				}
			case "NONE":
				body["tool_choice"] = map[string]any{"type": "none"}
			}
		}
	}
	return body
}

func generateAnthropic(ctx context.Context, c *Call, emit gem.Emitter) (Result, error) {
	thinking := c.Model.ReasoningEffort != "" && effortBudget[c.Model.ReasoningEffort] > 0
	res, emitted, err := anthropicOnce(ctx, c, emit, thinking)
	// History without thinking signatures (e.g. switched from another model)
	// is rejected when thinking is on; retry without it.
	if err != nil && !emitted && thinking {
		if e, ok := err.(*Error); ok && e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "thinking") {
			res, _, err = anthropicOnce(ctx, c, emit, false)
		}
	}
	return res, err
}

func anthropicOnce(ctx context.Context, c *Call, emit gem.Emitter, thinking bool) (Result, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout(c.Provider))
	defer cancel()
	r, err := typedRequest(c.Body)
	if err != nil {
		return Result{}, false, err
	}
	body, err := json.Marshal(buildAnthropic(c, r, thinking))
	if err != nil {
		return Result{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL(c.Provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		return Result{}, false, err
	}
	anthropicHeaders(req, c.Provider)
	resp, err := do(ctx, client(c.Provider, c.GlobalProxy), req)
	if err != nil {
		return Result{}, false, err
	}
	defer resp.Body.Close()
	st := &anStream{emit: emit, blocks: map[int]*anBlock{}, usage: &gem.Usage{}}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = gem.ReadSSE(resp.Body, st.event)
	} else {
		var b []byte
		if b, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err == nil {
			err = st.whole(b)
		}
	}
	if err == nil {
		err = st.finish()
	}
	return Result{InTokens: st.usage.PromptTokenCount, OutTokens: st.usage.CandidatesTokenCount}, st.any, err
}

func anthropicHeaders(req *http.Request, p *config.Provider) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if p.APIKey != "" {
		if p.AuthMode == "bearer" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		} else {
			req.Header.Set("x-api-key", p.APIKey)
		}
	}
	applyHeaders(req, p)
}

type anBlock struct {
	kind      string
	id, name  string
	input     strings.Builder
	signature string
	thinking  strings.Builder
}

type anStream struct {
	emit   gem.Emitter
	blocks map[int]*anBlock
	calls  []gem.Part
	stop   string
	usage  *gem.Usage
	any    bool
}

type anEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string         `json:"type"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anUsage        `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type anUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

func (s *anStream) event(data []byte) error {
	var ev anEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了无法解析的数据"}
	}
	switch ev.Type {
	case "error":
		return &Error{Status: http.StatusBadGateway, Message: upstreamMessage(data)}
	case "message_start":
		if ev.Message != nil {
			u := ev.Message.Usage
			s.usage.PromptTokenCount = u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
			s.usage.CachedContentTokenCount = u.CacheReadTokens
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			s.blocks[ev.Index] = &anBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		}
	case "content_block_delta":
		b := s.blocks[ev.Index]
		if b == nil || ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				s.any = true
				return s.emit(chunk([]gem.Part{{Text: ev.Delta.Text}}, "", nil))
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				s.any = true
				b.thinking.WriteString(ev.Delta.Thinking)
				return s.emit(chunk([]gem.Part{{Text: ev.Delta.Thinking, Thought: true}}, "", nil))
			}
		case "signature_delta":
			b.signature += ev.Delta.Signature
		case "input_json_delta":
			b.input.WriteString(ev.Delta.PartialJSON)
		}
	case "content_block_stop":
		b := s.blocks[ev.Index]
		if b == nil {
			return nil
		}
		switch b.kind {
		case "tool_use":
			s.calls = append(s.calls, gem.Part{FunctionCall: &gem.FunctionCall{ID: b.id, Name: b.name, Args: parseArgs(b.input.String())}})
		case "thinking":
			if b.signature != "" {
				// Carry the signature on an empty thought part so it returns in history.
				return s.emit(chunk([]gem.Part{{Text: "", Thought: true, ThoughtSignature: b.signature}}, "", nil))
			}
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.stop = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			s.usage.CandidatesTokenCount = ev.Usage.OutputTokens
		}
	}
	return nil
}

func (s *anStream) whole(b []byte) error {
	var v struct {
		Content []struct {
			Type      string         `json:"type"`
			Text      string         `json:"text"`
			Thinking  string         `json:"thinking"`
			Signature string         `json:"signature"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Input     map[string]any `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      anUsage         `json:"usage"`
		Error      json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了无法解析的数据"}
	}
	if len(v.Error) > 0 && string(v.Error) != "null" {
		return &Error{Status: http.StatusBadGateway, Message: upstreamMessage(b)}
	}
	var parts []gem.Part
	for _, c := range v.Content {
		switch c.Type {
		case "text":
			parts = append(parts, gem.Part{Text: c.Text})
		case "thinking":
			parts = append(parts, gem.Part{Text: c.Thinking, Thought: true, ThoughtSignature: c.Signature})
		case "tool_use":
			args := c.Input
			if args == nil {
				args = map[string]any{}
			}
			s.calls = append(s.calls, gem.Part{FunctionCall: &gem.FunctionCall{ID: c.ID, Name: c.Name, Args: args}})
		}
	}
	s.stop = v.StopReason
	s.usage.PromptTokenCount = v.Usage.InputTokens + v.Usage.CacheReadTokens + v.Usage.CacheCreationTokens
	s.usage.CandidatesTokenCount = v.Usage.OutputTokens
	if len(parts) > 0 {
		s.any = true
		return s.emit(chunk(parts, "", nil))
	}
	return nil
}

func (s *anStream) finish() error {
	if !s.any && len(s.calls) == 0 {
		return &Error{Status: http.StatusBadGateway, Message: "上游返回了空响应"}
	}
	finish := "STOP"
	switch s.stop {
	case "max_tokens":
		finish = "MAX_TOKENS"
	case "refusal":
		finish = "SAFETY"
	}
	s.usage.TotalTokenCount = s.usage.PromptTokenCount + s.usage.CandidatesTokenCount
	return s.emit(chunk(s.calls, finish, s.usage))
}

func listAnthropic(ctx context.Context, p *config.Provider, global string) ([]string, error) {
	u := strings.TrimSuffix(anthropicURL(p.BaseURL), "/messages") + "/models?limit=1000"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	anthropicHeaders(req, p)
	return listJSON(ctx, client(p, global), req, func(b []byte) ([]string, error) {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(v.Data))
		for _, m := range v.Data {
			out = append(out, m.ID)
		}
		return out, nil
	})
}
