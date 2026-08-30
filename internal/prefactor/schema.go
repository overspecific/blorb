package prefactor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/overspecific/blorb/internal/tools"
)

// Span schema names for the span types blorb emits.
const (
	SchemaAgentTurn        = "blorb:agent_turn"
	SchemaUserMessage      = "blorb:user_message"
	SchemaAssistantMessage = "blorb:assistant_message"
	SchemaLLM              = "blorb:llm"
	SchemaToolPrefix       = "blorb:tool:"
)

// ToolSchemaName returns the schema name for a tool span.
func ToolSchemaName(tool string) string {
	return SchemaToolPrefix + tool
}

// openObject returns a permissive JSON-schema object: anything is allowed.
// Following Prefactor's own default-schema pattern, additionalProperties is
// explicitly true so payloads are never rejected by schema validation.
func openObject() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": true,
	}
}

// propertyObject builds an object schema from named properties, permissive
// about additional properties.
func propertyObject(props map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": true,
	}
}

// stringSchema is the schema for a plain string value.
func stringSchema() map[string]any {
	return map[string]any{"type": "string"}
}

// objectSchemaFor parses raw (a JSON schema from tool args_schema) into a
// map schema. When raw is empty or not a non-empty JSON object, a permissive
// open object schema is returned as the fallback.
func objectSchemaFor(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return openObject()
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return openObject()
	}
	if len(m) == 0 {
		return openObject()
	}
	return m
}

// toolSpanSchemas builds the span type schemas for the configured tools,
// derived from each tool's args_schema. Tools are visited in sorted order
// so the schema content is stable.
func toolSpanSchemas(reg *tools.Registry) []SpanTypeSchema {
	if reg == nil {
		return nil
	}
	var out []SpanTypeSchema
	for _, def := range reg.Definitions() { // Definitions is already sorted by name
		out = append(out, SpanTypeSchema{
			Name:         ToolSchemaName(def.Name),
			ParamsSchema: objectSchemaFor(def.Parameters),
			ResultSchema: propertyObject(map[string]any{
				"output": stringSchema(),
				"failed": map[string]any{"type": "boolean"},
			}),
			Template: "{{output}}",
		})
	}
	return out
}

// DefaultAgentSchemaVersion builds the full agent schema version for a
// blorb agent from its name and tool registry. The external identifier is
// a stable SHA-256 hash of the schema content, so unchanged configs
// resolve to the same registered schema version.
func DefaultAgentSchemaVersion(agentName string, reg *tools.Registry) AgentSchemaVersion {
	spanTypes := []SpanTypeSchema{
		{
			Name: SchemaAgentTurn,
			ParamsSchema: propertyObject(map[string]any{
				"user_message": stringSchema(),
			}),
			ResultSchema: propertyObject(map[string]any{
				"assistant_text": stringSchema(),
				"status":         stringSchema(),
			}),
			Template: "{{assistant_text}}",
		},
		{
			Name: SchemaUserMessage,
			ParamsSchema: propertyObject(map[string]any{
				"content": stringSchema(),
			}),
		},
		{
			Name: SchemaAssistantMessage,
			ParamsSchema: propertyObject(map[string]any{
				"content":   stringSchema(),
				"reasoning": stringSchema(),
			}),
			ResultSchema: propertyObject(map[string]any{
				"content": stringSchema(),
			}),
		},
		{
			Name: SchemaLLM,
			ParamsSchema: propertyObject(map[string]any{
				"model": stringSchema(),
				"messages": map[string]any{
					"type": "array",
					"items": propertyObject(map[string]any{
						"role":    stringSchema(),
						"content": stringSchema(),
					}),
				},
			}),
			ResultSchema: propertyObject(map[string]any{
				"content":   stringSchema(),
				"reasoning": stringSchema(),
				"tool_calls": map[string]any{
					"type": "array",
				},
				"usage": propertyObject(map[string]any{
					"prompt_tokens":     map[string]any{"type": "integer"},
					"completion_tokens": map[string]any{"type": "integer"},
					"total_tokens":      map[string]any{"type": "integer"},
				}),
				"finish_reason": stringSchema(),
				"status":        stringSchema(),
			}),
		},
	}
	spanTypes = append(spanTypes, toolSpanSchemas(reg)...)

	schemaVersion := AgentSchemaVersion{
		ExternalIdentifier: schemaIdentifier(agentName, spanTypes),
		SpanTypeSchemas:    spanTypes,
	}
	return schemaVersion
}

// schemaIdentifier hashes the schema content into a stable external
// identifier: blorb-schema-<first 8 hex chars of the SHA-256 of the
// marshalled span types>.
func schemaIdentifier(agentName string, spanTypes []SpanTypeSchema) string {
	// Marshal through sorted-name order to keep the hash stable regardless
	// of any future ordering changes.
	sorted := make([]SpanTypeSchema, len(spanTypes))
	copy(sorted, spanTypes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	data, err := json.Marshal(struct {
		AgentName string           `json:"agent_name"`
		SpanTypes []SpanTypeSchema `json:"span_types"`
	}{AgentName: agentName, SpanTypes: sorted})
	if err != nil {
		// Structs of strings and maps marshal deterministically; this
		// cannot realistically fail, but errors are always handled.
		return "blorb-schema-unavailable"
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("blorb-schema-%s", hex.EncodeToString(sum[:])[:8])
}
