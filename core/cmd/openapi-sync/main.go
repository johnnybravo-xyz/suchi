package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var methods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
}

func main() {
	check := flag.Bool("check", false, "fail when schema.json is stale")
	flag.Parse()

	routes, err := registeredRoutes("..")
	if err != nil {
		fatal(err)
	}
	schemaPath := filepath.Join("api", "schema.json")
	original, err := os.ReadFile(schemaPath)
	if err != nil {
		fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(original, &schema); err != nil {
		fatal(err)
	}
	syncSchema(schema, routes)

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		fatal(err)
	}
	if *check {
		if !bytes.Equal(original, out.Bytes()) {
			fatal(fmt.Errorf("%s is stale; run `make schema`", schemaPath))
		}
		return
	}
	if err := os.WriteFile(schemaPath, out.Bytes(), 0o644); err != nil {
		fatal(err)
	}
}

func registeredRoutes(dir string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			method, path, ok := strings.Cut(pattern, " ")
			method = strings.ToLower(method)
			path = canonicalPath(path)
			if !ok || !methods[method] || (!strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/s/")) {
				return true
			}
			if out[path] == nil {
				out[path] = map[string]bool{}
			}
			out[path][method] = true
			return true
		})
		return nil
	})
	return out, err
}

func syncSchema(schema map[string]any, routes map[string]map[string]bool) {
	info, _ := schema["info"].(map[string]any)
	if info != nil {
		info["license"] = map[string]any{
			"name":       "AGPL-3.0-only",
			"identifier": "AGPL-3.0-only",
		}
	}

	rawPaths, _ := schema["paths"].(map[string]any)
	paths := map[string]any{}
	for rawPath, raw := range rawPaths {
		path := canonicalPath(rawPath)
		routeMethods, registered := routes[path]
		if !registered {
			continue
		}
		item, _ := raw.(map[string]any)
		filtered := map[string]any{}
		for key, value := range item {
			if methods[key] && !routeMethods[key] {
				continue
			}
			filtered[key] = value
		}
		if existing, ok := paths[path].(map[string]any); ok {
			for key, value := range filtered {
				existing[key] = value
			}
		} else {
			paths[path] = filtered
		}
	}
	for path, routeMethods := range routes {
		item, _ := paths[path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[path] = item
		}
		for method := range routeMethods {
			if _, ok := item[method]; !ok {
				item[method] = map[string]any{"summary": strings.ToUpper(method) + " " + path}
			}
		}
	}

	for path, raw := range paths {
		item := raw.(map[string]any)
		params := pathParameters(path)
		if len(params) > 0 {
			item["parameters"] = params
		}
		for method, rawOperation := range item {
			if !methods[method] {
				continue
			}
			operation := rawOperation.(map[string]any)
			delete(operation, "parameters")
			if _, ok := operation["operationId"]; !ok {
				operation["operationId"] = operationID(method, path)
			}
			responses, _ := operation["responses"].(map[string]any)
			if responses == nil {
				responses = map[string]any{
					"200": map[string]any{"description": "Success"},
				}
				operation["responses"] = responses
			}
			if _, ok := responses["4XX"]; !ok {
				responses["4XX"] = map[string]any{
					"description": "Client error",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/Error"},
						},
					},
				}
			}
		}
	}
	schema["paths"] = paths
	pruneUnusedSchemas(schema)
}

func pruneUnusedSchemas(schema map[string]any) {
	components, _ := schema["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if schemas == nil {
		return
	}

	used := map[string]bool{}
	var scan func(any)
	scan = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for _, child := range value {
				scan(child)
			}
		case []any:
			for _, child := range value {
				scan(child)
			}
		case string:
			const prefix = "#/components/schemas/"
			name, ok := strings.CutPrefix(value, prefix)
			if !ok || used[name] {
				return
			}
			definition, ok := schemas[name]
			if !ok {
				return
			}
			used[name] = true
			scan(definition)
		}
	}
	scan(schema["paths"])
	for name := range schemas {
		if !used[name] {
			delete(schemas, name)
		}
	}
}

func canonicalPath(path string) string {
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}
	return path
}

var parameterPattern = regexp.MustCompile(`\{([^}]+)\}`)

func pathParameters(path string) []any {
	matches := parameterPattern.FindAllStringSubmatch(path, -1)
	out := make([]any, 0, len(matches))
	for _, match := range matches {
		name := match[1]
		typeName := "string"
		if name == "id" || strings.HasSuffix(name, "_id") || name == "uid" || name == "cid" {
			typeName = "integer"
		}
		out = append(out, map[string]any{
			"in": "path", "name": name, "required": true,
			"schema": map[string]any{"type": typeName},
		})
	}
	return out
}

func operationID(method, path string) string {
	parts := []string{method}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		parts = append(parts, strings.ToUpper(part[:1])+part[1:])
	}
	return strings.Join(parts, "")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "openapi-sync:", err)
	os.Exit(1)
}
