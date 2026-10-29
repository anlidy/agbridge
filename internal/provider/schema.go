package provider

import (
	"strings"

	"agbridge/internal/gem"
)

// Keys of a Gemini (OpenAPI-subset) schema that carry over to JSON Schema.
var schemaKeys = map[string]bool{
	"type": true, "description": true, "enum": true, "items": true, "properties": true,
	"required": true, "anyOf": true, "oneOf": true, "allOf": true, "format": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true,
	"minLength": true, "maxLength": true, "pattern": true, "default": true, "title": true,
	"additionalProperties": true, "const": true, "$ref": true, "$defs": true, "definitions": true,
}

// jsonSchema converts a Gemini schema (uppercase types, nullable, ...) to
// plain JSON Schema accepted by OpenAI-compatible and Anthropic APIs.
func jsonSchema(v any) any {
	switch s := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range s {
			if !schemaKeys[k] {
				continue
			}
			switch k {
			case "type":
				if t, ok := val.(string); ok {
					out[k] = strings.ToLower(t)
					continue
				}
				out[k] = val
			case "properties", "$defs", "definitions":
				props := map[string]any{}
				if m, ok := val.(map[string]any); ok {
					for name, sub := range m {
						props[name] = jsonSchema(sub)
					}
				}
				out[k] = props
			case "items", "additionalProperties":
				if _, ok := val.(bool); ok {
					out[k] = val
				} else {
					out[k] = jsonSchema(val)
				}
			case "anyOf", "oneOf", "allOf":
				if arr, ok := val.([]any); ok {
					conv := make([]any, len(arr))
					for i, sub := range arr {
						conv[i] = jsonSchema(sub)
					}
					out[k] = conv
				}
			default:
				out[k] = val
			}
		}
		if t, _ := out["type"].(string); t == "type_unspecified" {
			delete(out, "type")
		}
		return out
	case []any:
		conv := make([]any, len(s))
		for i, sub := range s {
			conv[i] = jsonSchema(sub)
		}
		return conv
	}
	return v
}

// toolParameters returns an object schema for a function declaration.
func toolParameters(fd *gem.FunctionDeclaration) map[string]any {
	var schema map[string]any
	if m, ok := fd.ParametersJSONSchema.(map[string]any); ok {
		schema = m
	} else if fd.Parameters != nil {
		schema, _ = jsonSchema(fd.Parameters).(map[string]any)
	}
	if schema == nil {
		schema = map[string]any{}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if schema["type"] == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	return schema
}
