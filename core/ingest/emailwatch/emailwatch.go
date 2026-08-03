// Package emailwatch is the third canonical ingest path (design
// principle 9): IMAP polling for "forward it, forget it" archival.
//
// **Phase-2 status: scaffolding only.**
//
// This package ships the pure-logic pieces that are testable without a
// live mailbox:
//
//   - URL parsing (imaps://user@host/Folder → host+user+folder+TLS)
//   - Attachment MIME allowlist
//   - Sidecar synthesis from a message's From / Subject / Date headers
//   - plus-address routing helper (archive+22@... → JD category 22)
//
// The polling loop itself is a stub. A real implementation lands
// once we can integration-test against a mailbox — go-imap v2's
// client API is close to stable but not fully so; ship the network
// code when there's a way to validate it end-to-end. Design principle:
// don't fake integration coverage with unit tests.
//
// The stub Run() logs "not yet implemented" and exits, so the boot
// path is safe even when INGEST_IMAP_URL is set.
package emailwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/ingest/sidecar"
	"github.com/suchi-dms/suchi/core/jobs"
)

// Defaults.
const (
	DefaultPollInterval = 5 * time.Minute
	DefaultMaxAttach    = 25 * 1024 * 1024 // 25 MiB per attachment
	PluginName          = "email-ingest"   // plugin_kv namespace for msg-id dedup
)

// AllowedMIMEs is the initial attachment-type allowlist. Kept small
// on purpose: an inbox is hostile input, and the ingest pipeline
// only really has qpdf + pdf-inspector + ocrmypdf paths right now.
// Widens as we ship more converters (exotic-file-types tasks).
var AllowedMIMEs = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/tiff":      true,
}

// Config carries the knobs. Zero-value: everything empty → disabled.
type Config struct {
	URL             string // imaps://user@host/FOLDER
	Password        string
	OwnerEmail      string
	PollInterval    time.Duration
	ProcessedFolder string // move-to-folder on success; empty = mark \Seen
	MaxAttachBytes  int64
}

// Watcher is what Run reads. Constructed by New; nil when idle.
type Watcher struct {
	cfg     Config
	db      *db.DB
	cas     *blob.CAS
	log     *slog.Logger
	disp    *jobs.Dispatcher
	ownerID int64

	host      string
	user      string
	folder    string
	useTLS    bool
	interval  time.Duration
	maxAttach int64
}

// New validates cfg + resolves the owner. Returns (nil, nil) when the
// producer is idle (empty URL/OwnerEmail) or the owner isn't
// present yet — matches the fs-watch bootstrap-race behavior.
func New(ctx context.Context, cfg Config, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, log *slog.Logger) (*Watcher, error) {
	if cfg.URL == "" || cfg.OwnerEmail == "" {
		log.Info("emailwatch.disabled", "reason", "INGEST_IMAP_URL or INGEST_IMAP_OWNER_EMAIL not set")
		return nil, nil
	}
	if cfg.Password == "" {
		return nil, errors.New("emailwatch: INGEST_IMAP_PASSWORD is required")
	}
	host, user, folder, useTLS, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("emailwatch: %w", err)
	}
	interval := cfg.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}
	maxAttach := cfg.MaxAttachBytes
	if maxAttach == 0 {
		maxAttach = DefaultMaxAttach
	}

	var ownerID int64
	err = d.Read.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = ? AND disabled = 0`,
		cfg.OwnerEmail).Scan(&ownerID)
	if err != nil {
		log.Warn("emailwatch.disabled",
			"reason", "owner not found or query failed",
			"email", cfg.OwnerEmail, "err", err.Error())
		return nil, nil
	}

	return &Watcher{
		cfg: cfg, db: d, cas: cas, disp: disp, ownerID: ownerID,
		log:  log.With("component", "emailwatch", "host", host, "user", user, "folder", folder),
		host: host, user: user, folder: folder, useTLS: useTLS,
		interval: interval, maxAttach: maxAttach,
	}, nil
}

// Run is the poll loop. **Phase-2 stub**: logs a warning and returns.
// A follow-up wires go-imap/v2 once we have a mailbox to integration-
// test against.
func (w *Watcher) Run(ctx context.Context) {
	w.log.Warn("emailwatch.stub",
		"msg", "IMAP polling loop not yet implemented; add go-imap/v2 wiring in a follow-up commit",
		"interval", w.interval.String())
	<-ctx.Done()
	w.log.Info("emailwatch.stop", "reason", "context")
}

// ParseURL splits an imaps://user@host[:port]/FOLDER URL into pieces.
// Exported so it's cheap to unit-test independent of the client wiring.
func ParseURL(raw string) (host, user, folder string, useTLS bool, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	switch u.Scheme {
	case "imaps":
		useTLS = true
	case "imap":
		useTLS = false
	default:
		err = fmt.Errorf("scheme %q: want imap or imaps", u.Scheme)
		return
	}
	if u.User == nil || u.User.Username() == "" {
		err = errors.New("URL is missing user@host")
		return
	}
	user = u.User.Username()
	host = u.Host
	if host == "" {
		err = errors.New("URL is missing host")
		return
	}
	folder = strings.TrimPrefix(u.Path, "/")
	if folder == "" {
		folder = "INBOX"
	}
	return
}

// RouteFromPlusAddress inspects a delivered-to address like
// "archive+22@example.com" and returns the intended JD category code.
// Returns 0 when the address has no plus-tag or the tag isn't
// numeric. Callers fall through to inbox on 0.
//
// Also recognizes the special tag "flat" → 0 (inbox) so producers
// can send to archive+flat@… for the "no-classification-please" case.
func RouteFromPlusAddress(addr string) int {
	// Take the local-part.
	local := addr
	if at := strings.Index(addr, "@"); at >= 0 {
		local = addr[:at]
	}
	plus := strings.Index(local, "+")
	if plus < 0 {
		return 0
	}
	tag := local[plus+1:]
	if tag == "flat" {
		return 0
	}
	if n, err := strconv.Atoi(tag); err == nil {
		return n
	}
	return 0
}

// SidecarFromMessage synthesizes a V1 sidecar from mail-header
// fields. Producers of full IMAP polling code call this after
// parsing a message.
type MessageHeader struct {
	From        string // "Alice <alice@x>" or "alice@x"
	Subject     string
	Date        time.Time
	DeliveredTo string // for plus-address routing
}

func SidecarFromMessage(h MessageHeader) sidecar.V1 {
	s := sidecar.V1{
		Version: sidecar.Version,
		Title:   h.Subject,
		Notes:   fmt.Sprintf("auto-ingested from email; sender: %s", h.From),
		Tags:    []string{"source:email"},
	}
	if h.From != "" {
		s.Correspondents = []sidecar.Correspondent{
			{Name: h.From, Role: "sender"},
		}
	}
	if !h.Date.IsZero() {
		s.Created = h.Date.UTC().Format("2006-01-02")
	}
	if code := RouteFromPlusAddress(h.DeliveredTo); code != 0 {
		s.JDCategory = code
	}
	return s
}
