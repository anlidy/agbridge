// Package gem holds the Gemini wire types that Antigravity speaks, plus
// helpers shared by the provider translators.
package gem

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

type Blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type FileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type FunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type Part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FileData         *FileData         `json:"fileData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

type FunctionDeclaration struct {
	Name                 string `json:"name"`
	Description          string `json:"description,omitempty"`
	Parameters           any    `json:"parameters,omitempty"`
	ParametersJSONSchema any    `json:"parametersJsonSchema,omitempty"`
}

type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type FunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"` // AUTO, ANY, NONE, VALIDATED
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type ToolConfig struct {
	FunctionCallingConfig *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type ThinkingConfig struct {
	IncludeThoughts *bool  `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type GenerationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *float64        `json:"topK,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ThinkingConfig   *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

// Request is the typed view used by non-Gemini translators.
type Request struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolConfig        *ToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int     `json:"index"`
}

type Usage struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

type Response struct {
	Candidates    []Candidate `json:"candidates,omitempty"`
	UsageMetadata *Usage      `json:"usageMetadata,omitempty"`
	ModelVersion  string      `json:"modelVersion,omitempty"`
	ResponseID    string      `json:"responseId,omitempty"`
}

// Emitter receives GenerateContentResponse chunks (typed Response or raw JSON).
type Emitter func(chunk any) error

// SystemText flattens a system instruction into plain text.
func SystemText(c *Content) string {
	if c == nil {
		return ""
	}
	var parts []string
	for _, p := range c.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ReadSSE calls fn with the data payload of each server-sent event.
func ReadSSE(r io.Reader, fn func(data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		b := bytes.Clone(data.Bytes())
		data.Reset()
		return fn(b)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// Merger folds streamed chunks into a single non-streaming response.
type Merger struct {
	parts  []Part
	finish string
	usage  *Usage
	model  string
	id     string
}

func (m *Merger) Add(chunk any) error {
	var r Response
	switch v := chunk.(type) {
	case *Response:
		r = *v
	case Response:
		r = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
	}
	if r.UsageMetadata != nil {
		m.usage = r.UsageMetadata
	}
	if r.ModelVersion != "" {
		m.model = r.ModelVersion
	}
	if r.ResponseID != "" {
		m.id = r.ResponseID
	}
	for _, c := range r.Candidates {
		if c.FinishReason != "" {
			m.finish = c.FinishReason
		}
		for _, p := range c.Content.Parts {
			n := len(m.parts)
			if n > 0 && p.Text != "" && p.FunctionCall == nil && p.InlineData == nil &&
				m.parts[n-1].FunctionCall == nil && m.parts[n-1].InlineData == nil &&
				m.parts[n-1].Text != "" && m.parts[n-1].Thought == p.Thought {
				m.parts[n-1].Text += p.Text
				if p.ThoughtSignature != "" {
					m.parts[n-1].ThoughtSignature = p.ThoughtSignature
				}
				continue
			}
			m.parts = append(m.parts, p)
		}
	}
	return nil
}

func (m *Merger) Result() *Response {
	finish := m.finish
	if finish == "" {
		finish = "STOP"
	}
	parts := m.parts
	if parts == nil {
		parts = []Part{}
	}
	return &Response{
		Candidates:    []Candidate{{Content: Content{Role: "model", Parts: parts}, FinishReason: finish}},
		UsageMetadata: m.usage,
		ModelVersion:  m.model,
		ResponseID:    m.id,
	}
}

// EstimateTokens is a rough count used for countTokens on custom models.
func EstimateTokens(b []byte) int { return len(b)/4 + 1 }
