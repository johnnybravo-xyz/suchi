// Command suchi mcp — MCP server adapter over suchi's REST API.
//
// This is a thin adapter, not a second API. Every tool maps 1:1 to
// a /api/ call, no business logic that isn't a REST call underneath.
// Agent writes appear in the audit log as any other actor because
// they go through the same auth chain.
//
// Two transports (matches the MCP SDK):
//   - stdio (default) — for local integrations like Claude Desktop
//   - Streamable HTTP — for remote agents, `--http :port` flag
//
// Auth (both transports):
//   SUCHI_URL   base URL of the suchi API (e.g. https://suchi.local:8000)
//   SUCHI_TOKEN scoped API token (from `POST /api/token/`)
//
// The token is forwarded to suchi as `Authorization: Token <hex>`.
// Every operation an agent performs runs as whoever owns that token —
// same permission model as a mobile client. Give the agent its own
// user account with the scopes it needs; don't hand it your admin
// token.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// debugReadBuildInfo aliases the stdlib call so the main package's
// existing buildVersion() sees the same debug.BuildInfo without an
// extra import dance.
func debugReadBuildInfo() (*debug.BuildInfo, bool) { return debug.ReadBuildInfo() }

// runMCP is the `suchi mcp` subcommand entry.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		httpAddr = fs.String("http", "", "if non-empty, listen for MCP over Streamable HTTP on this addr (default: stdio)")
		baseURL  = fs.String("url", "", "suchi API base URL; overrides SUCHI_URL/PUBLIC_URL env")
		token    = fs.String("token", "", "suchi API token; overrides SUCHI_TOKEN env")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Auth + endpoint discovery. flags → env → error out cleanly.
	base := firstNonEmpty(*baseURL, os.Getenv("SUCHI_URL"), os.Getenv("PUBLIC_URL"))
	if base == "" {
		fmt.Fprintln(os.Stderr,
			"mcp: no target URL — set SUCHI_URL, PUBLIC_URL, or pass --url")
		return 2
	}
	tok := firstNonEmpty(*token, os.Getenv("SUCHI_TOKEN"))
	if tok == "" {
		fmt.Fprintln(os.Stderr,
			"mcp: no API token — set SUCHI_TOKEN or pass --token")
		return 2
	}
	target, err := url.ParseRequestURI(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: bad --url %q: %v\n", base, err)
		return 2
	}
	if target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" ||
		(target.EscapedPath() != "" && target.EscapedPath() != "/") {
		fmt.Fprintf(os.Stderr, "mcp: bad --url %q: want an http(s) origin without credentials, path, query, or fragment\n", base)
		return 2
	}
	base = strings.TrimRight(base, "/")

	client := newSuchiClient(base, tok)

	// Sanity — /api/whoami confirms the token works before we advertise
	// tools to an agent. Better UX than a first-tool-call failure.
	if err := client.ping(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: cannot reach %s (%v)\n", base, err)
		return 1
	}

	info, _ := debugReadBuildInfo()
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "suchi",
		Version: buildVersion(info),
	}, nil)
	registerTools(server, client)

	// Transport dispatch.
	ctx := context.Background()
	if *httpAddr != "" {
		// Streamable HTTP — for remote agents.
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
			return server
		}, nil)
		log.Printf("mcp: listening on %s (Streamable HTTP; listener has no separate authentication)", *httpAddr)
		httpServer := &http.Server{
			Addr:              *httpAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		if err := httpServer.ListenAndServe(); err != nil {
			log.Printf("mcp: http listen: %v", err)
			return 1
		}
		return 0
	}
	// stdio — the default. Suitable for `command:"suchi-mcp"` entries
	// in Claude Desktop / Cursor / other local agent configs.
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Printf("mcp: stdio transport: %v", err)
		return 1
	}
	return 0
}

// suchiClient wraps every REST call an MCP tool makes. Tokens go here,
// timeouts go here, retries would go here if we wanted them — the tool
// handlers stay declarative.
type suchiClient struct {
	base  string
	token string
	http  *http.Client
}

const maxMCPResponseBytes = 8 << 20

func newSuchiClient(base, token string) *suchiClient {
	return &suchiClient{
		base:  base,
		token: token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// do issues a request against the Suchi API with Token auth. It accepts only
// bounded, valid JSON on 2xx and reports failures without response text or
// query values that may contain document data.
func (c *suchiClient) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Keep responses bounded even when a configured origin is not Suchi.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMCPResponseBytes {
		return nil, errors.New("suchi response exceeds 8 MiB")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, safeAPIResponseError(method, path, resp.StatusCode, b)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("suchi returned a non-JSON response")
	}
	if !json.Valid(b) {
		return nil, errors.New("suchi returned malformed JSON")
	}
	return b, nil
}

func (c *suchiClient) ping(ctx context.Context) error {
	_, err := c.do(ctx, "GET", "/api/whoami", nil)
	return err
}

func safeAPIResponseError(method, requestPath string, status int, body []byte) error {
	pathOnly := requestPath
	if parsed, err := url.Parse(requestPath); err == nil {
		pathOnly = parsed.EscapedPath()
	} else if before, _, ok := strings.Cut(requestPath, "?"); ok {
		pathOnly = before
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &envelope) == nil && safeErrorCode(envelope.Code) {
		return fmt.Errorf("suchi %s %s: %d (%s)", method, pathOnly, status, envelope.Code)
	}
	return fmt.Errorf("suchi %s %s: %d", method, pathOnly, status)
}

func safeErrorCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

// ---------- tool registration ----------

// registerTools wires every MCP tool to a suchi REST call. Each tool's
// input args are declared as a Go struct with `json`/`jsonschema` tags —
// the SDK derives the JSON schema automatically.
func registerTools(server *mcp.Server, client *suchiClient) {
	registerSearch(server, client)
	registerGetDocument(server, client)
	registerListInbox(server, client)
	registerResolveApprovalTask(server, client)
	registerCreateShareLink(server, client)
}

// --- search_documents ---

type searchArgs struct {
	Query    string `json:"query"    jsonschema:"the full-text search query"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"max results (default 10, max 25)"`
}

func registerSearch(server *mcp.Server, client *suchiClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_documents",
		Description: "Full-text search over suchi's document corpus (title + " +
			"content). Returns ranked hits with a highlighted snippet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		limit := args.PageSize
		if limit <= 0 || limit > 25 {
			limit = 10
		}
		q := url.Values{}
		q.Set("q", args.Query)
		q.Set("page_size", strconv.Itoa(limit))
		b, err := client.do(ctx, "GET", "/api/search/?"+q.Encode(), nil)
		return textResult(b, err)
	})
}

// --- get_document ---

type getDocArgs struct {
	ID int64 `json:"id" jsonschema:"the document id"`
}

func registerGetDocument(server *mcp.Server, client *suchiClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_document",
		Description: "Fetch a single document by id — title, extracted content, tags, correspondents, custom fields.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args getDocArgs) (*mcp.CallToolResult, any, error) {
		if args.ID <= 0 {
			return nil, nil, errors.New("id must be > 0")
		}
		b, err := client.do(ctx, "GET", "/api/documents/"+strconv.FormatInt(args.ID, 10), nil)
		return textResult(b, err)
	})
}

// --- list_inbox ---

type listInboxArgs struct{}

func registerListInbox(server *mcp.Server, client *suchiClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_inbox",
		Description: "List approval tasks assigned to the caller. " +
			"Includes machine-outbox jobs too (for context on what suchi is doing).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listInboxArgs) (*mcp.CallToolResult, any, error) {
		b, err := client.do(ctx, "GET", "/api/tasks/?limit=50", nil)
		return textResult(b, err)
	})
}

// --- resolve_approval_task ---

type resolveTaskArgs struct {
	TaskID int64  `json:"task_id" jsonschema:"the approval task id to resolve"`
	Choice string `json:"choice"  jsonschema:"one of the choices the task declared"`
}

func registerResolveApprovalTask(server *mcp.Server, client *suchiClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "resolve_approval_task",
		Description: "Resolve an approval task with the given choice (e.g. approve, reject).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args resolveTaskArgs) (*mcp.CallToolResult, any, error) {
		if args.TaskID <= 0 || args.Choice == "" {
			return nil, nil, errors.New("task_id must be > 0 and choice non-empty")
		}
		body, _ := json.Marshal(map[string]any{"choice": args.Choice})
		b, err := client.do(ctx, "POST",
			"/api/approvals/tasks/"+strconv.FormatInt(args.TaskID, 10)+"/resolve",
			strings.NewReader(string(body)))
		return textResult(b, err)
	})
}

// --- create_share_link ---

type createShareArgs struct {
	DocIDs       []int64 `json:"doc_ids" jsonschema:"the document ids to include in the share (1..N)"`
	Label        string  `json:"label,omitempty" jsonschema:"human-friendly label (creator sees this in their share list)"`
	ExpiresInSec int64   `json:"expires_in_sec,omitempty" jsonschema:"seconds until the link expires; 0 means never"`
	Password     string  `json:"password,omitempty" jsonschema:"optional password recipients must enter"`
}

func registerCreateShareLink(server *mcp.Server, client *suchiClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "create_share_link",
		Description: "Create an expiring share link for one or more documents. " +
			"Returns the token + public_url the recipient uses.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args createShareArgs) (*mcp.CallToolResult, any, error) {
		if len(args.DocIDs) == 0 {
			return nil, nil, errors.New("doc_ids must be non-empty")
		}
		body, _ := json.Marshal(map[string]any{
			"doc_ids":        args.DocIDs,
			"label":          args.Label,
			"expires_in_sec": args.ExpiresInSec,
			"password":       args.Password,
		})
		b, err := client.do(ctx, "POST", "/api/share_links/",
			strings.NewReader(string(body)))
		return textResult(b, err)
	})
}

// ---------- helpers ----------

// textResult wraps a JSON byte-slice response into a CallToolResult
// with a single text content block. Errors get surfaced verbatim.
// Agents that speak MCP know how to parse JSON inside a text block —
// the alternative (structured content) is fine but adds surface area
// per tool that we don't need yet.
func textResult(b []byte, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
