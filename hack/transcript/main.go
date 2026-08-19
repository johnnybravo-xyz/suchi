// hack/transcript is a recording reverse proxy: point a client at it
// with --target set to a live upstream, and every request/response
// pair lands in --out as a golden fixture. The primary use case is
// replaying the compatibility surface as contract tests —
// but the tool is target-agnostic.
//
// Fixtures land in --out with names like:
//   0001-GET-api-documents.json
//   0002-POST-api-documents.json
// with sequence numbers so replay preserves interaction order.
//
// The captured JSON records enough for a contract-style replay: request
// method + path + query + headers (sanitized) + body, response status +
// headers + body. Auth headers get redacted; binary bodies (uploads,
// downloads) are stored by content hash under blobs/ next to the JSON.
//
// Not for CI. Not part of the shipped binary. Run manually while you
// drive whichever client you want to characterize through its full
// interaction set (login, list, upload, tag, delete...). Later, replay
// the resulting fixture set as contract tests.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		listen    = flag.String("listen", ":8443", "listen address")
		target    = flag.String("target", "", "target URL (required)")
		outDir    = flag.String("out", "testdata/transcripts", "where to write fixtures")
		blobLimit = flag.Int("blob-min", 4096, "bodies larger than N bytes are stored as blobs; smaller are inlined")
		insecure  = flag.Bool("insecure", false, "log unredacted (debug only; don't commit fixtures made this way)")
	)
	flag.Parse()

	if *target == "" {
		log.Fatalf("--target is required (the real URL)")
	}
	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("bad --target: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(*outDir, "blobs"), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	r := &recorder{
		target:    targetURL,
		outDir:    *outDir,
		blobLimit: int64(*blobLimit),
		insecure:  *insecure,
		proxy:     httputil.NewSingleHostReverseProxy(targetURL),
	}
	// Wrap the reverse proxy's Director so we can also strip the Host
	// header rewrite quirk when tunneling to Cloudflare-fronted.
	origDirector := r.proxy.Director
	r.proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = targetURL.Host
	}

	log.Printf("legacy-recorder listening on %s → %s (fixtures → %s)",
		*listen, targetURL, *outDir)
	if err := http.ListenAndServe(*listen, r); err != nil {
		log.Fatal(err)
	}
}

// recorder is the reverse proxy + fixture writer.
type recorder struct {
	target    *url.URL
	outDir    string
	blobLimit int64
	insecure  bool
	proxy     *httputil.ReverseProxy
	seq       atomic.Int64
	mu        sync.Mutex // guards fixture writes; interactive rate is trivial
}

// fixture is one recorded request/response pair. Kept flat + declarative
// so the replay harness can json.Unmarshal it straight into a
// struct and drive httptest.NewRequest / Server.ServeHTTP.
type fixture struct {
	Seq        int64             `json:"seq"`
	RecordedAt string            `json:"recorded_at"`
	DurationMS int64             `json:"duration_ms"`
	Request    fixtureRequest    `json:"request"`
	Response   fixtureResponse   `json:"response"`
	Notes      map[string]string `json:"notes,omitempty"`
}

type fixtureRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   map[string][]string `json:"query,omitempty"`
	Headers map[string][]string `json:"headers"`
	Body    fixtureBody         `json:"body,omitempty"`
}

type fixtureResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    fixtureBody         `json:"body,omitempty"`
}

// fixtureBody either inlines a small body as text or references a blob
// on disk by SHA-256. Never both. Kind is "" for empty.
type fixtureBody struct {
	Kind        string `json:"kind,omitempty"` // "inline" | "blob" | ""
	Text        string `json:"text,omitempty"` // present when Kind=inline
	Blob        string `json:"blob,omitempty"` // hex sha256 filename under blobs/
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	seq := r.seq.Add(1)
	start := time.Now()

	// Buffer the request body — we need to read it AND forward it.
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read req body: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(reqBody))
	req.ContentLength = int64(len(reqBody))

	// Capture the response body via a tee writer, then forward it to
	// the real client. Simpler than intercepting the reverse proxy's
	// ModifyResponse hook and letting go's stdlib copy the body twice.
	respBuf := &bytes.Buffer{}
	tw := &teeWriter{ResponseWriter: w, buf: respBuf}
	r.proxy.ServeHTTP(tw, req)

	f := fixture{
		Seq:        seq,
		RecordedAt: start.UTC().Format(time.RFC3339Nano),
		DurationMS: time.Since(start).Milliseconds(),
		Request: fixtureRequest{
			Method:  req.Method,
			Path:    req.URL.Path,
			Query:   req.URL.Query(),
			Headers: r.sanitizeHeaders(req.Header),
			Body:    r.storeBody(reqBody, req.Header.Get("Content-Type")),
		},
		Response: fixtureResponse{
			Status:  tw.status,
			Headers: r.sanitizeHeaders(tw.Header()),
			Body:    r.storeBody(respBuf.Bytes(), tw.Header().Get("Content-Type")),
		},
	}
	r.writeFixture(f, req)
}

// storeBody either inlines a small body as text (best-effort UTF-8) or
// spills a large / binary body to blobs/<sha256>. Content-type stays on
// the fixture so replay can reconstruct the right Content-Type header
// without sniffing.
func (r *recorder) storeBody(b []byte, contentType string) fixtureBody {
	if len(b) == 0 {
		return fixtureBody{}
	}
	body := fixtureBody{
		SizeBytes:   int64(len(b)),
		ContentType: contentType,
	}
	if int64(len(b)) <= r.blobLimit && looksTextual(contentType, b) {
		body.Kind = "inline"
		body.Text = string(b)
		return body
	}
	sum := sha256.Sum256(b)
	body.Kind = "blob"
	body.Blob = hex.EncodeToString(sum[:])
	path := filepath.Join(r.outDir, "blobs", body.Blob)
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			log.Printf("warn: blob write %s: %v", path, err)
		}
	}
	return body
}

// sanitizeHeaders redacts auth-bearing headers. Kept as a strict
// allow-list-adjacent denylist because legacy / mobile apps put
// creds into surprising places (X-Api-Auth, Cookie, plus custom
// per-fork headers).
func (r *recorder) sanitizeHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		low := strings.ToLower(k)
		if r.insecure {
			out[k] = v
			continue
		}
		switch {
		case low == "authorization",
			low == "cookie",
			low == "set-cookie",
			low == "x-csrftoken",
			strings.HasPrefix(low, "x-api-"):
			out[k] = []string{"REDACTED"}
		default:
			out[k] = v
		}
	}
	return out
}

// writeFixture serializes the pair to <seq>-<METHOD>-<sanitized-path>.json.
// The path is sanitized so the filename can be committed on every
// platform (drop the query, replace / with _, cap length).
func (r *recorder) writeFixture(f fixture, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := fmt.Sprintf("%04d-%s-%s.json", f.Seq, f.Request.Method,
		fixturePathSlug(req.URL.Path))
	path := filepath.Join(r.outDir, name)
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("warn: marshal fixture %d: %v", f.Seq, err)
		return
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		log.Printf("warn: write fixture %s: %v", path, err)
		return
	}
	log.Printf("rec %04d %s %s -> %d (%dms, %s)",
		f.Seq, f.Request.Method, req.URL.Path, f.Response.Status,
		f.DurationMS, name)
}

// teeWriter is an http.ResponseWriter that mirrors the body to an
// internal buffer while also writing to the real client. Only tracks
// status + body — the headers stay on the underlying ResponseWriter.
type teeWriter struct {
	http.ResponseWriter
	status  int
	buf     *bytes.Buffer
	written bool
}

func (t *teeWriter) WriteHeader(code int) {
	if t.written {
		return
	}
	t.status = code
	t.written = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeWriter) Write(b []byte) (int, error) {
	if !t.written {
		t.status = http.StatusOK
		t.written = true
	}
	t.buf.Write(b)
	return t.ResponseWriter.Write(b)
}

func fixturePathSlug(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "root"
	}
	// Replace path separators + risky chars.
	repl := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "?", "-", "&", "-",
		"=", "-", "#", "-", " ", "-", "..", "-",
	)
	p = repl.Replace(p)
	if len(p) > 80 {
		p = p[:80]
	}
	return p
}

// looksTextual is heuristic: content-type sniff plus a NUL check. JSON,
// text/*, and multipart/form-data ARE textual (multipart holds binary
// parts inside a textual envelope; we inline it up to blob-limit so the
// upload boundary is captured verbatim).
func looksTextual(contentType string, b []byte) bool {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "text/"),
		strings.HasPrefix(ct, "application/x-www-form-urlencoded"),
		strings.HasPrefix(ct, "application/xml"),
		strings.HasPrefix(ct, "multipart/form-data"):
		return !bytes.Contains(b, []byte{0})
	}
	// Small unknown-type bodies: inline if they're mostly ASCII.
	if len(b) > 1024 {
		return false
	}
	if bytes.Contains(b, []byte{0}) {
		return false
	}
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f || c == '\n' || c == '\r' || c == '\t' {
			printable++
		}
	}
	return printable*10 > len(b)*9
}
