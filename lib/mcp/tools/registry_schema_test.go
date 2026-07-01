package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolSchemasAreClientValid guards the whole tool registry against a class
// of output-schema bug that curl-based e2e tests miss but strict MCP clients
// reject at tools/list time: a boolean JSON schema (`true`/`false`) sitting at a
// property value or `items` position.
//
// It bit us twice on the indexer passthrough types. json.RawMessage ([]byte)
// inferred to `{"type":"array"...}` — a valid schema object, so the client
// accepted it, but the server then rejected real object payloads against it. The
// "fix" of using a bare any inferred to the boolean schema `true` at the
// property position, which the go-sdk emits verbatim and a strict client refuses
// ("Invalid input" at outputSchema.properties.<field>). map[string]any infers to
// {"type":"object","additionalProperties":true} — valid at the property position
// and still accepting arbitrary objects.
//
// The test walks each registered tool's input and output schema exactly as a
// client sees them (over an in-memory transport, so OutputSchema is the decoded
// wire map) and fails on any boolean schema found anywhere except in
// additionalProperties, where a boolean is legal and expected.
func TestToolSchemasAreClientValid(t *testing.T) {
	ctx := context.Background()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	Register(srv, &Handlers{ChainID: "test"})

	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools registered")
	}

	for _, tool := range res.Tools {
		assertNoBooleanSchema(t, tool.Name+".inputSchema", tool.InputSchema)
		assertNoBooleanSchema(t, tool.Name+".outputSchema", tool.OutputSchema)
	}
}

// assertNoBooleanSchema recursively descends a decoded JSON schema and fails if
// it finds a boolean at any schema position other than additionalProperties.
// Schema positions checked: every value of a "properties" object and every
// "items" value — the two positions a strict client requires to be objects.
func assertNoBooleanSchema(t *testing.T, path string, node any) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	for key, val := range m {
		switch key {
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				continue
			}
			for name, sub := range props {
				if _, isBool := sub.(bool); isBool {
					t.Errorf("%s.properties.%s is a boolean schema (%v); strict MCP clients reject this — use map[string]any, not any/json.RawMessage", path, name, sub)
					continue
				}
				assertNoBooleanSchema(t, path+".properties."+name, sub)
			}
		case "items":
			if _, isBool := val.(bool); isBool {
				t.Errorf("%s.items is a boolean schema (%v); strict MCP clients reject this — use map[string]any items, not any/json.RawMessage", path, val)
				continue
			}
			assertNoBooleanSchema(t, path+".items", val)
		case "additionalProperties":
			// A boolean here is legal and expected (map[string]any yields
			// additionalProperties:true); only recurse if it's a nested schema.
			assertNoBooleanSchema(t, path+".additionalProperties", val)
		default:
			assertNoBooleanSchema(t, path+"."+key, val)
		}
	}
}
