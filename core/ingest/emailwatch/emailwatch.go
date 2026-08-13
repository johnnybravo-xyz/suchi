// Package emailwatch is the third canonical ingest path (design
// principle 9): IMAP polling for "forward it, forget it" archival.
//
// Complements the fs-watch (mbsync + Maildir) and Upload API paths.
// Every message the loop fetches gets stored as a `.eml` blob and
// enqueued through the same post-ingest chain — `core/pipeline/eml`
// then fans out one child document per attachment. Same semantics as
// dropping a .eml file into `INGEST_FS_DIR`, minus the mail-sync
// sidecar.
//
// Dedup: Message-ID is unique per email; documents.email_message_id
// carries it, and the poll loop skips any message whose ID already
// exists in the table. Safe across restarts + folder-moves.
//
// This package still exports the pure-logic helpers (URL parsing,
// SidecarFromMessage, plus-address routing) that landed as
// scaffolding — they're used both here and by potential agent code.
package emailwatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
)

// Defaults.
const (
	DefaultPollInterval = 5 * time.Minute
	DefaultMaxAttach    = 25 * 1024 * 1024 // 25 MiB per attachment
	PluginName          = "email-ingest"   // plugin_kv namespace for msg-id dedup
)

// AllowedMIMEs is the attachment-type allowlist. Kept tight on
// purpose: an inbox is hostile input, and only types the downstream
// pipeline can render + text-extract belong here. Currently: PDFs,
// common raster images, plus the office-doc formats the converter
// chain already handles (docx / xlsx / odt) and plain text.
var AllowedMIMEs = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/tiff":      true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true, // .docx
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true, // .xlsx
	"application/vnd.oasis.opendocument.text":                                 true, // .odt
	"text/plain": true,
}

// Config carries the knobs. Zero-value: everything empty → disabled.
type Config struct {
	URL             string // imaps://user@host/FOLDER
	Password        string
	OwnerEmail      string
	PollInterval    time.Duration
	ProcessedFolder string // move-to-folder on success; empty = mark \Seen
	MaxAttachBytes  int64
	// TLSCAFile is an optional path to a PEM file whose CAs are added
	// to the trust pool used for imaps:// connections. Bridge, self-
	// hosted Dovecot, and homelab CAs live here. System roots stay
	// trusted; this only widens the set.
	TLSCAFile string
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
	rootCAs   *x509.CertPool // nil = use system roots only
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

	// Widen the trust pool with an operator-supplied CA when set —
	// Bridge, self-hosted Dovecot, homelab CAs, etc. System roots stay
	// trusted; hard-fail rather than silently degrade if the file is
	// unreadable or malformed.
	var rootCAs *x509.CertPool
	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("emailwatch: read INGEST_IMAP_TLS_CA_FILE %q: %w", cfg.TLSCAFile, err)
		}
		rootCAs, err = x509.SystemCertPool()
		if err != nil || rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		if !rootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("emailwatch: no valid PEM certs in %q", cfg.TLSCAFile)
		}
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
		interval: interval, maxAttach: maxAttach, rootCAs: rootCAs,
	}, nil
}

// Run is the poll loop. Connects, syncs one folder's UNSEEN messages
// into suchi as .eml documents, then sleeps and repeats. Blocks until
// ctx is cancelled.
//
// A connection error is logged and retried on the next tick — no
// crash-on-mailbox-outage. Dedup via Message-ID means a repeat cycle
// on the same messages is idempotent even if the server never marked
// them \Seen.
func (w *Watcher) Run(ctx context.Context) {
	w.log.Info("emailwatch.start", "interval", w.interval.String())
	// First cycle immediately so the operator's first upload lands
	// without a full poll interval wait.
	w.cycle(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("emailwatch.stop", "reason", "context")
			return
		case <-t.C:
			w.cycle(ctx)
		}
	}
}

// cycle runs one connect → fetch-unseen → ingest → mark-seen pass.
// All errors are logged; the loop keeps ticking.
func (w *Watcher) cycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c, err := w.connect(ctx)
	if err != nil {
		w.log.Warn("emailwatch.connect_failed", "err", err.Error())
		return
	}
	defer func() { _ = c.Logout() }()

	if _, err := c.Select(w.folder, false); err != nil {
		w.log.Warn("emailwatch.select_failed", "folder", w.folder, "err", err.Error())
		return
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		w.log.Warn("emailwatch.search_failed", "err", err.Error())
		return
	}
	if len(uids) == 0 {
		return
	}
	w.log.Info("emailwatch.unseen", "count", len(uids))

	// Fetch bodies + envelopes in one round-trip.
	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	section := &imap.BodySectionName{Peek: true} // don't set \Seen implicitly
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchUid,
		section.FetchItem(),
	}
	msgs := make(chan *imap.Message, 8)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(seqset, items, msgs) }()

	var seenUIDs []uint32
	for m := range msgs {
		if ctx.Err() != nil {
			return
		}
		raw, msgID, err := w.materialize(m, section)
		if err != nil {
			w.log.Warn("emailwatch.materialize_failed",
				"uid", m.Uid, "err", err.Error())
			continue
		}
		imported, err := w.importOne(ctx, raw, msgID, m)
		if err != nil {
			w.log.Warn("emailwatch.import_failed",
				"uid", m.Uid, "msg_id", msgID, "err", err.Error())
			continue
		}
		if imported {
			w.log.Info("emailwatch.imported",
				"uid", m.Uid, "msg_id", msgID, "bytes", len(raw))
		}
		seenUIDs = append(seenUIDs, m.Uid)
	}
	if err := <-done; err != nil {
		w.log.Warn("emailwatch.fetch_failed", "err", err.Error())
		// still fall through and mark whatever we did import
	}
	if len(seenUIDs) == 0 {
		return
	}

	// Mark processed messages. Either move to ProcessedFolder (when
	// configured — the common "archive after ingest" convention) or
	// set \Seen.
	markSet := new(imap.SeqSet)
	markSet.AddNum(seenUIDs...)
	if w.cfg.ProcessedFolder != "" {
		if err := c.UidMove(markSet, w.cfg.ProcessedFolder); err != nil {
			w.log.Warn("emailwatch.move_failed",
				"to", w.cfg.ProcessedFolder, "err", err.Error())
		}
	} else {
		flags := []any{imap.SeenFlag}
		if err := c.UidStore(markSet,
			imap.FormatFlagsOp(imap.AddFlags, true), flags, nil); err != nil {
			w.log.Warn("emailwatch.mark_seen_failed", "err", err.Error())
		}
	}
}

// connect dials, TLS-wraps if useTLS, and logs in. Bounded by ctx.
func (w *Watcher) connect(ctx context.Context) (*imapclient.Client, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	// w.host may embed a :port (url.URL.Host includes it). Split so
	// addr assembly and TLS ServerName each get the piece they need.
	host, port := w.host, ""
	if i := strings.LastIndex(w.host, ":"); i > 0 {
		host, port = w.host[:i], w.host[i+1:]
	}
	if port == "" {
		if w.useTLS {
			port = "993"
		} else {
			port = "143"
		}
	}
	addr := fmt.Sprintf("%s:%s", host, port)
	var (
		c   *imapclient.Client
		err error
	)
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if w.useTLS {
			c, err = imapclient.DialTLS(addr, &tls.Config{
				ServerName: host,
				RootCAs:    w.rootCAs, // nil => system roots only
			})
		} else {
			c, err = imapclient.Dial(addr)
		}
		done <- err
	}()
	select {
	case <-dialCtx.Done():
		return nil, dialCtx.Err()
	case err = <-done:
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if err := c.Login(w.user, w.cfg.Password); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("login %s@%s: %w", w.user, w.host, err)
	}
	return c, nil
}

// materialize reads the whole raw message body out of the imap.Message
// literal reader and extracts the Message-ID header. Both are needed
// downstream — the raw for CAS.Put, the ID for dedup.
func (w *Watcher) materialize(m *imap.Message, section *imap.BodySectionName) ([]byte, string, error) {
	lit := m.GetBody(section)
	if lit == nil {
		return nil, "", errors.New("empty body literal")
	}
	// Cap the read at maxAttach*2 — a message with 25 MB attachments
	// can easily be 40 MB with encoding overhead.
	raw, err := io.ReadAll(io.LimitReader(lit, w.maxAttach*2+8*1024))
	if err != nil {
		return nil, "", err
	}
	msgID := ""
	if m.Envelope != nil {
		msgID = strings.TrimSpace(m.Envelope.MessageId)
	}
	// go-imap's envelope decoding strips the angle brackets, but
	// documents.email_message_id stores the raw form for parity
	// with our own eml.Parse (which returns them included). Re-wrap.
	if msgID != "" && !strings.HasPrefix(msgID, "<") {
		msgID = "<" + msgID + ">"
	}
	return raw, msgID, nil
}

// importOne is the write side: dedup by Message-ID, put the raw
// bytes into CAS, insert a documents row with mime=message/rfc822,
// enqueue post-ingest (which fans out attachments as children via
// core/pipeline/eml).
//
// Returns (imported, err) where imported=false is either "already
// known" (dedup hit) or "size cap tripped". err is only for hard
// failures the outer loop should log.
func (w *Watcher) importOne(ctx context.Context, raw []byte, msgID string, m *imap.Message) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("empty message body")
	}
	// Dedup: message-ID + owner scope. Same ID under a different
	// owner is fine (household member forwarded it, etc.).
	if msgID != "" {
		var existing int64
		err := w.db.Read.QueryRowContext(ctx, `
			SELECT id FROM documents
			WHERE owner_id = ? AND email_message_id = ?
			LIMIT 1
		`, w.ownerID, msgID).Scan(&existing)
		if err == nil {
			w.log.Debug("emailwatch.dedup", "msg_id", msgID, "existing", existing)
			return false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}

	ref, err := w.cas.Put(bytes.NewReader(raw))
	if err != nil {
		return false, fmt.Errorf("cas put: %w", err)
	}

	// Owner-scoped alive-blob dedup — the same .eml bytes might already
	// be in the archive from a prior fs-watch drop. Skip if so.
	var existingID int64
	err = w.db.Read.QueryRowContext(ctx, `
		SELECT id FROM documents
		WHERE owner_id = ? AND original_blob = ? AND trashed_at IS NULL
	`, w.ownerID, ref.SHA256).Scan(&existingID)
	if err == nil {
		w.log.Debug("emailwatch.blob_dedup", "existing", existingID, "sha", ref.SHA256)
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	title := ""
	created := time.Now().Unix()
	if m.Envelope != nil {
		if m.Envelope.Subject != "" {
			title = m.Envelope.Subject
		}
		if !m.Envelope.Date.IsZero() {
			created = m.Envelope.Date.Unix()
		}
	}
	if title == "" {
		title = "email"
	}

	inbox, err := jd.InboxCategoryID(ctx, w.db)
	if err != nil {
		return false, fmt.Errorf("inbox category: %w", err)
	}

	if err := w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				email_message_id
			) VALUES (?, ?, ?, ?, 'message/rfc822', ?, ?, ?, ?, ?)
		`, w.ownerID, ref.SHA256, ref.Size, title,
			inbox, now, created, now,
			nullOrString(msgID))
		if err != nil {
			return err
		}
		docID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// Consumption-trigger context. mail_rule_id stays 0 until a
		// mail-rules feature lands (Phase 6); filename mirrors the
		// subject so filter_filename automations can pattern-match.
		payload, _ := json.Marshal(map[string]any{
			"sha256":    ref.SHA256,
			"size":      ref.Size,
			"mime_type": "message/rfc822",
			"filename":  title,
		})
		return jobs.Enqueue(ctx, tx, postingest.Kind, docID, string(payload))
	}); err != nil {
		return false, fmt.Errorf("db write: %w", err)
	}
	// Nudge the dispatcher so the eml.Parse fanout doesn't wait for
	// the next poll tick.
	if w.disp != nil {
		w.disp.Nudge()
	}
	return true, nil
}

// nullOrString returns nil when s is empty (so INSERT stores NULL
// instead of an empty string in email_message_id) — keeps the unique
// index tidy.
func nullOrString(s string) any {
	if s == "" {
		return nil
	}
	return s
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
