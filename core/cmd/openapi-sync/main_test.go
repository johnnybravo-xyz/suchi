package main

import "testing"

func TestSyncSchemaPrunesStaleRoutesAndComponents(t *testing.T) {
	schema := map[string]any{
		"components": map[string]any{"schemas": map[string]any{
			"Error":  map[string]any{"type": "object"},
			"Unused": map[string]any{"type": "object"},
		}},
		"paths": map[string]any{
			"/api/live": map[string]any{
				"get":  map[string]any{"summary": "kept"},
				"post": map[string]any{"summary": "stale method"},
			},
			"/api/stale": map[string]any{"get": map[string]any{}},
		},
	}
	routes := map[string]map[string]bool{
		"/api/live": {"get": true},
		"/api/new":  {"post": true},
	}

	syncSchema(schema, routes)

	paths := schema["paths"].(map[string]any)
	if _, ok := paths["/api/stale"]; ok {
		t.Fatal("stale route was retained")
	}
	live := paths["/api/live"].(map[string]any)
	if _, ok := live["post"]; ok {
		t.Fatal("stale method was retained")
	}
	if live["get"].(map[string]any)["summary"] != "kept" {
		t.Fatal("registered operation metadata was not preserved")
	}
	if _, ok := paths["/api/new"].(map[string]any)["post"]; !ok {
		t.Fatal("new route was not added")
	}

	schemas := schema["components"].(map[string]any)["schemas"].(map[string]any)
	if _, ok := schemas["Error"]; !ok {
		t.Fatal("referenced component was pruned")
	}
	if _, ok := schemas["Unused"]; ok {
		t.Fatal("unused component was retained")
	}
}

func TestPruneUnusedSchemasKeepsTransitiveReferences(t *testing.T) {
	schema := map[string]any{
		"components": map[string]any{"schemas": map[string]any{
			"Parent": map[string]any{"properties": map[string]any{
				"child": map[string]any{"$ref": "#/components/schemas/Child"},
			}},
			"Child":  map[string]any{"type": "object"},
			"Unused": map[string]any{"type": "object"},
		}},
		"paths": map[string]any{
			"/api/live": map[string]any{"get": map[string]any{
				"responses": map[string]any{"200": map[string]any{
					"content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"$ref": "#/components/schemas/Parent"},
					}},
				}},
			}},
		},
	}

	pruneUnusedSchemas(schema)

	schemas := schema["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{"Parent", "Child"} {
		if _, ok := schemas[name]; !ok {
			t.Fatalf("referenced component %q was pruned", name)
		}
	}
	if _, ok := schemas["Unused"]; ok {
		t.Fatal("unused component was retained")
	}
}
