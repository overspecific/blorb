package prefactor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/overspecific/blorb/internal/config"
	"github.com/overspecific/blorb/internal/prefactor"
	"github.com/overspecific/blorb/internal/tools"
)

// testRegistry builds a Registry from tool entries, failing the test on
// configuration errors.
func testRegistry(t *testing.T, entries []config.ToolEntry) *tools.Registry {
	t.Helper()
	reg, err := tools.NewRegistry(entries)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func TestDefaultAgentSchemaVersionSpanTypes(t *testing.T) {
	reg := testRegistry(t, []config.ToolEntry{
		{
			Name:        "list_files",
			Description: "List files.",
			Command:     []string{"ls"},
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		{
			Name:        "get_time",
			Description: "Get the time.",
			Command:     []string{"date"},
		},
	})

	schema := prefactor.DefaultAgentSchemaVersion("helper", reg)

	want := map[string]bool{
		prefactor.SchemaAgentTurn:              true,
		prefactor.SchemaUserMessage:            true,
		prefactor.SchemaAssistantMessage:       true,
		prefactor.SchemaLLM:                    true,
		prefactor.ToolSchemaName("list_files"): true,
		prefactor.ToolSchemaName("get_time"):   true,
	}
	got := make(map[string]bool, len(schema.SpanTypeSchemas))
	for _, s := range schema.SpanTypeSchemas {
		got[s.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("schema missing span type %q (have %v)", name, keys(got))
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected span type %q", name)
		}
	}
}

func TestToolSpanParamsMatchArgsSchema(t *testing.T) {
	argsSchema := `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	reg := testRegistry(t, []config.ToolEntry{
		{
			Name:        "list_files",
			Description: "List files.",
			Command:     []string{"ls"},
			ArgsSchema:  json.RawMessage(argsSchema),
		},
	})

	schema := prefactor.DefaultAgentSchemaVersion("helper", reg)

	var wantSchema map[string]any
	if err := json.Unmarshal(json.RawMessage(argsSchema), &wantSchema); err != nil {
		t.Fatalf("unmarshal args schema: %v", err)
	}

	for _, s := range schema.SpanTypeSchemas {
		if s.Name != prefactor.ToolSchemaName("list_files") {
			continue
		}
		got, err := json.Marshal(s.ParamsSchema)
		if err != nil {
			t.Fatalf("marshal params schema: %v", err)
		}
		var gotSchema map[string]any
		if err := json.Unmarshal(got, &gotSchema); err != nil {
			t.Fatalf("unmarshal params schema: %v", err)
		}
		assertJSONEqual(t, "params schema", gotSchema, wantSchema)
		return
	}
	t.Fatal("no span type for tool list_files")
}

func TestToolSpanParamsFallbackOpenObject(t *testing.T) {
	reg := testRegistry(t, []config.ToolEntry{
		{Name: "no_schema", Description: "No schema.", Command: []string{"date"}},
	})

	schema := prefactor.DefaultAgentSchemaVersion("helper", reg)
	for _, s := range schema.SpanTypeSchemas {
		if s.Name != prefactor.ToolSchemaName("no_schema") {
			continue
		}
		if s.ParamsSchema["type"] != "object" || s.ParamsSchema["additionalProperties"] != true {
			t.Errorf("ParamsSchema = %v, want a permissive open object", s.ParamsSchema)
		}
		if s.ResultSchema["properties"] == nil {
			t.Errorf("ResultSchema = %v, want output/failed properties", s.ResultSchema)
		}
		return
	}
	t.Fatal("no span type for tool no_schema")
}

func TestSchemaGoldenMarshal(t *testing.T) {
	reg := testRegistry(t, []config.ToolEntry{
		{
			Name:        "list_files",
			Description: "List files.",
			Command:     []string{"ls"},
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	})

	schema := prefactor.DefaultAgentSchemaVersion("helper", reg)
	got, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}

	golden := filepath.Join("testdata", "agent_schema.json.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to write it): %v", err)
	}
	assertJSONEqualBytes(t, "schema", got, want)
}

func TestSchemaIdentifierStable(t *testing.T) {
	newSchema := func() prefactor.AgentSchemaVersion {
		reg := testRegistry(t, []config.ToolEntry{
			{Name: "t", Description: "A tool.", Command: []string{"echo"}},
		})
		return prefactor.DefaultAgentSchemaVersion("helper", reg)
	}

	first := newSchema()
	second := newSchema()
	if first.ExternalIdentifier == "" {
		t.Fatal("ExternalIdentifier is empty, want a hash")
	}
	if !strings.HasPrefix(first.ExternalIdentifier, "blorb-schema-") {
		t.Errorf("ExternalIdentifier = %q, want blorb-schema-<hash8> prefix", first.ExternalIdentifier)
	}
	if first.ExternalIdentifier != second.ExternalIdentifier {
		t.Errorf("identifier changed across calls: %q vs %q", first.ExternalIdentifier, second.ExternalIdentifier)
	}
}

func TestSchemaIdentifierChangesWhenToolSchemaChanges(t *testing.T) {
	reg1 := testRegistry(t, []config.ToolEntry{
		{Name: "t", Description: "A tool.", Command: []string{"echo"}, ArgsSchema: json.RawMessage(`{"type":"object"}`)},
	})
	reg2 := testRegistry(t, []config.ToolEntry{
		{Name: "t", Description: "A tool.", Command: []string{"echo"}, ArgsSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
	})

	id1 := prefactor.DefaultAgentSchemaVersion("helper", reg1).ExternalIdentifier
	id2 := prefactor.DefaultAgentSchemaVersion("helper", reg2).ExternalIdentifier
	if id1 == id2 {
		t.Errorf("identifiers equal (%q), want them to differ when args_schema changes", id1)
	}
}

func TestSchemaIdentifierChangesWhenToolsChange(t *testing.T) {
	reg1 := testRegistry(t, []config.ToolEntry{
		{Name: "a", Description: "Tool a.", Command: []string{"echo"}},
	})
	reg2 := testRegistry(t, []config.ToolEntry{
		{Name: "a", Description: "Tool a.", Command: []string{"echo"}},
		{Name: "b", Description: "Tool b.", Command: []string{"echo"}},
	})

	id1 := prefactor.DefaultAgentSchemaVersion("helper", reg1).ExternalIdentifier
	id2 := prefactor.DefaultAgentSchemaVersion("helper", reg2).ExternalIdentifier
	if id1 == id2 {
		t.Errorf("identifiers equal (%q), want them to differ when tools are added", id1)
	}
}

func TestSchemaNilRegistry(t *testing.T) {
	schema := prefactor.DefaultAgentSchemaVersion("helper", nil)
	for _, s := range schema.SpanTypeSchemas {
		if strings.HasPrefix(s.Name, prefactor.SchemaToolPrefix) {
			t.Errorf("unexpected tool span type %q with a nil registry", s.Name)
		}
	}
	if len(schema.SpanTypeSchemas) != 4 {
		t.Errorf("span type count = %d, want 4 built-ins", len(schema.SpanTypeSchemas))
	}
}

func assertJSONEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	gotBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	assertJSONEqualBytes(t, what, gotBytes, wantBytes)
}

func assertJSONEqualBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshal got %s: %v", what, err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("unmarshal want %s: %v", what, err)
	}
	gotNorm, _ := json.Marshal(gotVal)
	wantNorm, _ := json.Marshal(wantVal)
	if string(gotNorm) != string(wantNorm) {
		t.Errorf("%s differs from golden:\ngot:  %s\nwant: %s", what, gotNorm, wantNorm)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
