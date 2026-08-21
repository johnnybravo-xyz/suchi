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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/crypto"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/emailaccounts"
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch/oauth"
	"github.com/johnnybravo-xyz/suchi/core/ingest/sidecar"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/pipeconfig"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
)

// Defaults. Per-attachment size cap is overridable via
// SUCHI_EMAIL_MAX_ATTACH (byte-suffixed, e.g. "50M").
const (
	PluginName         = "email-ingest" // plugin_kv namespace for msg-id dedup
	imapCommandTimeout = 30 * time.Second
)

var errMessageTooLarge = errors.New("emailwatch: raw message too large")

// DefaultMaxAttach returns the effective per-attachment cap. Read at
// call time so a config file loaded from main.runServe reaches it.
func DefaultMaxAttach() int64 {
	return pipeconfig.Bytes("SUCHI_EMAIL_MAX_ATTACH", 25*1024*1024)
}

// shouldImport is the pre-ingest gate. Splitting it out of importOne
// lets the unit tests exercise the drop paths without spinning up a
// CAS + DB fixture.
//
// Returns:
//   - hasAttachment: cached result of the MIME walk. importOne reuses
//     this for the post-ingest payload so we don't parse twice.
//   - fromHeader:    the address we compared against the allowlist,
//     surfaced so the caller can log it on drop.
//   - drop:          "" means pass the gate. Non-empty is the reason
//     tag: "from_allowlist" or "attachments_only".
func shouldImport(account *emailaccounts.Account, envelope *imap.Envelope, raw []byte) (hasAttachment bool, fromHeader string, drop string) {
	if envelope != nil && len(envelope.From) > 0 && envelope.From[0] != nil {
		fromHeader = envelope.From[0].Address()
	}
	if !MatchFromAllowlist(fromHeader, account.FromAllowlist) {
		return false, fromHeader, "from_allowlist"
	}
	hasAttachment = HasAttachment(raw)
	if account.AttachmentsOnly && !hasAttachment {
		return hasAttachment, fromHeader, "attachments_only"
	}
	return hasAttachment, fromHeader, ""
}

// Config carries process-wide knobs shared by every Watcher. Per-
// account state lives on emailaccounts.Account.
type Config struct {
	MaxAttachBytes int64
}

// Watcher polls one email account. Constructed by New from an
// emailaccounts.Account row + the shared Config.
type Watcher struct {
	account *emailaccounts.Account
	cfg     Config
	db      *db.DB
	cas     *blob.CAS
	disp    *jobs.Dispatcher
	aead    *crypto.AEADKey
	msal    *oauth.Manager
	log     *slog.Logger

	interval  time.Duration
	maxAttach int64
	rootCAs   *x509.CertPool // nil = use system roots only
}

// New builds a Watcher from an account row. Returns (nil, nil) when
// the account is disabled or its owner has been removed — the
// supervisor treats nil as "skip this row this cycle". Returns an
// error only for account configuration the operator must fix;
// password unseal is deferred to connect so a rotated/stale seal
// doesn't block the whole supervisor at build time.
func New(ctx context.Context, account *emailaccounts.Account, cfg Config, d *db.DB, cas *blob.CAS, disp *jobs.Dispatcher, aead *crypto.AEADKey, msal *oauth.Manager, log *slog.Logger) (*Watcher, error) {
	if account == nil || !account.Enabled {
		return nil, nil
	}
	if err := emailaccounts.ValidatePollInterval(account.PollIntervalMin); err != nil {
		return nil, fmt.Errorf("emailwatch: invalid account: %w", err)
	}

	var ownerCheck int64
	err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = ? AND disabled = 0`,
		account.OwnerID).Scan(&ownerCheck)
	if err != nil {
		log.Warn("emailwatch.disabled",
			"reason", "owner not found or query failed",
			"account_id", account.ID, "owner_id", account.OwnerID, "err", err.Error())
		return nil, nil
	}

	interval := time.Duration(account.PollIntervalMin) * time.Minute
	maxAttach := cfg.MaxAttachBytes
	if maxAttach == 0 {
		maxAttach = DefaultMaxAttach()
	}

	// Widen the trust pool with an operator-supplied CA when set —
	// Bridge, self-hosted Dovecot, homelab CAs, etc. System roots stay
	// trusted; hard-fail rather than silently degrade if the file is
	// unreadable or malformed.
	rootCAs, err := emailaccounts.LoadTLSRootCAs(account.TLSCAFile)
	if err != nil {
		return nil, err
	}

	return &Watcher{
		account: account,
		cfg:     cfg,
		db:      d,
		cas:     cas,
		disp:    disp,
		aead:    aead,
		msal:    msal,
		log: log.With(
			"component", "emailwatch",
			"account_id", account.ID,
			"account", account.Name,
			"host", account.Host,
			"user", account.Username,
			"folder", account.Folder,
		),
		interval:  interval,
		maxAttach: maxAttach,
		rootCAs:   rootCAs,
	}, nil
}

// Run is the poll loop. Connects, syncs messages above one folder's durable
// UID cursor into suchi as .eml documents, then sleeps and repeats. Blocks until
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
	w.runCycle(ctx)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("emailwatch.stop", "reason", "context")
			return
		case <-t.C:
			w.runCycle(ctx)
		}
	}
}

// runCycle records the outcome separately from the UID cursor. A failed poll
// keeps the timestamp of the last successful poll while surfacing its error;
// a successful poll clears any prior error even when no new mail was found.
func (w *Watcher) runCycle(ctx context.Context) {
	err := w.cycle(ctx)
	if ctx.Err() != nil {
		return
	}
	syncedAt := w.account.LastSyncAt
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
		w.log.Warn("emailwatch.cycle_failed", "err", errMsg)
	} else {
		syncedAt = time.Now().Unix()
	}
	if markErr := emailaccounts.MarkSync(ctx, w.db, w.account.ID, syncedAt, errMsg); markErr != nil {
		w.log.Warn("emailwatch.sync_status_persist_failed", "err", markErr.Error())
		return
	}
	w.account.LastSyncAt = syncedAt
	w.account.LastError = errMsg
}

// cycle runs one connect → search-by-UID-cursor → ingest → advance-
// cursor pass. All errors are logged; the loop keeps ticking.
//
// Idempotency: LastUIDSeen is the high-water mark for the folder.
// Search is `UID <cursor+1>:*` (plus SINCE <sync_since> for the
// initial-sync horizon). Nothing STOREs \Seen unless the operator
// asked for it via MarkSeen — the operator's mail client keeps its
// own read/unread state.
//
// UIDVALIDITY drift: if the folder's current UIDVALIDITY differs
// from what we last saw, we've been reconnected to a "different"
// folder (recreated / mailbox reset) and old UIDs are meaningless.
// Reset the cursor to 0 and re-sync from the SINCE horizon.
func (w *Watcher) cycle(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ownerEnabled bool
	if err := w.db.Read.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM users WHERE id = ? AND disabled = 0
		)
	`, w.account.OwnerID).Scan(&ownerEnabled); err != nil {
		return fmt.Errorf("check account owner: %w", err)
	}
	if !ownerEnabled {
		return nil
	}
	c, err := w.connect(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = c.Logout() }()

	mbox, err := c.Select(w.account.Folder, false)
	if err != nil {
		return fmt.Errorf("select folder %q: %w", w.account.Folder, err)
	}
	uidValidity := mbox.UidValidity
	lastUID := w.account.LastUIDSeen
	if w.account.UIDValiditySeen != 0 && w.account.UIDValiditySeen != uidValidity {
		w.log.Warn("emailwatch.uidvalidity_reset",
			"was", w.account.UIDValiditySeen, "now", uidValidity)
		lastUID = 0
	}

	criteria := imap.NewSearchCriteria()
	uidSet := new(imap.SeqSet)
	uidSet.AddRange(lastUID+1, 0) // "N:*"
	criteria.Uid = uidSet
	if ts := w.account.SyncSince; ts != nil && *ts > 0 {
		criteria.Since = time.Unix(*ts, 0).UTC()
	}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(uids) == 0 {
		// Even with no messages, persist a UIDVALIDITY stamp on first
		// cycle so a future drift is detectable. Skip when nothing
		// changed to avoid a pointless updated_at bump every poll.
		if w.account.UIDValiditySeen != uidValidity {
			if err := emailaccounts.UpdateUIDCursor(ctx, w.db, w.account.ID, lastUID, uidValidity); err != nil {
				return fmt.Errorf("persist cursor: %w", err)
			} else {
				w.account.LastUIDSeen = lastUID
				w.account.UIDValiditySeen = uidValidity
			}
		}
		return nil
	}
	w.log.Info("emailwatch.new_messages", "count", len(uids), "cursor", lastUID)

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

	var (
		seenUIDs   []uint32
		failedUIDs []uint32
		cycleErrs  []error
		cycleErrN  int
	)
	recordCycleErr := func(err error) {
		cycleErrN++
		if len(cycleErrs) < 5 {
			cycleErrs = append(cycleErrs, err)
		}
	}
	for m := range msgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, msgID, err := w.materialize(m, section)
		if err != nil {
			if errors.Is(err, errMessageTooLarge) {
				limit := w.rawMessageLimit()
				w.log.Warn("emailwatch.message_skipped",
					"uid", m.Uid, "reason", "raw_message_too_large", "err", err.Error())
				audit.Log(ctx, w.db, w.log, audit.Event{
					Action: "document.ingest.skipped", ObjectKind: "ingest",
					After: map[string]any{
						"reason":        "oversized_email",
						"account_id":    w.account.ID,
						"uid":           m.Uid,
						"limit_type":    "raw_message",
						"limit_bytes":   limit,
						"size_at_least": limit + 1,
					},
				})
				seenUIDs = append(seenUIDs, m.Uid)
				continue
			}
			w.log.Warn("emailwatch.materialize_failed",
				"uid", m.Uid, "err", err.Error())
			failedUIDs = append(failedUIDs, m.Uid)
			recordCycleErr(fmt.Errorf("uid %d materialize: %w", m.Uid, err))
			continue
		}
		imported, err := w.importOne(ctx, raw, msgID, m)
		if err != nil {
			w.log.Warn("emailwatch.import_failed",
				"uid", m.Uid, "msg_id", msgID, "err", err.Error())
			failedUIDs = append(failedUIDs, m.Uid)
			recordCycleErr(fmt.Errorf("uid %d import: %w", m.Uid, err))
			continue
		}
		if imported {
			w.log.Info("emailwatch.imported",
				"uid", m.Uid, "msg_id", msgID, "bytes", len(raw))
		}
		seenUIDs = append(seenUIDs, m.Uid)
	}
	fetchErr := <-done
	vanishedUIDs := missingUIDs(uids, seenUIDs, failedUIDs)
	if fetchErr == nil {
		if len(vanishedUIDs) > 0 {
			w.log.Info("emailwatch.messages_disappeared", "count", len(vanishedUIDs))
		}
	} else if isConcurrentDeleteFetchError(fetchErr) {
		remaining, searchErr := c.UidSearch(criteria)
		if searchErr != nil {
			fetchErr = fmt.Errorf("recheck after concurrent delete: %w", searchErr)
			w.log.Warn("emailwatch.fetch_recheck_failed", "err", searchErr.Error())
			recordCycleErr(fetchErr)
		} else {
			var retryUIDs []uint32
			vanishedUIDs, retryUIDs = partitionMissingUIDs(vanishedUIDs, remaining)
			failedUIDs = append(failedUIDs, retryUIDs...)
			w.log.Info("emailwatch.messages_disappeared", "count", len(vanishedUIDs))
			fetchErr = nil
		}
	} else {
		w.log.Warn("emailwatch.fetch_failed", "err", fetchErr.Error())
		recordCycleErr(fmt.Errorf("fetch: %w", fetchErr))
	}

	// Server-side bookkeeping for the processed UIDs.
	//   - ProcessedFolder set   → move messages out (INBOX-tidy path)
	//   - Else MarkSeen         → STORE +\Seen (operator-opt-in legacy
	//                              path; hijacks the client's read state)
	//   - Else                  → do nothing on the server; the local
	//                              cursor below is what makes the poll
	//                              idempotent
	bookkeepingOK := true
	if len(seenUIDs) > 0 {
		markSet := new(imap.SeqSet)
		markSet.AddNum(seenUIDs...)
		switch {
		case w.account.ProcessedFolder != "":
			if err := c.UidMove(markSet, w.account.ProcessedFolder); err != nil {
				w.log.Warn("emailwatch.move_failed",
					"to", w.account.ProcessedFolder, "err", err.Error())
				bookkeepingOK = false
				recordCycleErr(fmt.Errorf("move to %q: %w", w.account.ProcessedFolder, err))
			}
		case w.account.MarkSeen:
			flags := []any{imap.SeenFlag}
			if err := c.UidStore(markSet,
				imap.FormatFlagsOp(imap.AddFlags, true), flags, nil); err != nil {
				w.log.Warn("emailwatch.mark_seen_failed", "err", err.Error())
				bookkeepingOK = false
				recordCycleErr(fmt.Errorf("mark seen: %w", err))
			}
		}
	}

	// A transient failure is a checkpoint barrier: later messages may be
	// imported and moved, but the cursor stops immediately before the first
	// failed UID. Those later messages are harmlessly deduplicated if the
	// server still returns them on the next poll. A fetch-level error keeps the
	// old cursor because the client cannot know which requested UIDs were lost.
	checkpointUIDs := append(seenUIDs, vanishedUIDs...)
	nextUID := nextUIDCheckpoint(lastUID, checkpointUIDs, failedUIDs, fetchErr == nil && bookkeepingOK)
	if nextUID > lastUID || w.account.UIDValiditySeen != uidValidity {
		if err := emailaccounts.UpdateUIDCursor(ctx, w.db, w.account.ID, nextUID, uidValidity); err != nil {
			w.log.Warn("emailwatch.cursor_persist_failed", "err", err.Error())
			recordCycleErr(fmt.Errorf("persist cursor: %w", err))
		} else {
			w.account.LastUIDSeen = nextUID
			w.account.UIDValiditySeen = uidValidity
		}
	}
	if omitted := cycleErrN - len(cycleErrs); omitted > 0 {
		cycleErrs = append(cycleErrs, fmt.Errorf("%d additional errors omitted", omitted))
	}
	return errors.Join(cycleErrs...)
}

// Outlook can return NO when a message disappears between UID SEARCH and UID
// FETCH. That is normal concurrent mailbox activity, not a mailbox failure.
func isConcurrentDeleteFetchError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "requested messages no longer exist") ||
		strings.Contains(message, "requested message no longer exists")
}

func missingUIDs(requested, completed, failed []uint32) []uint32 {
	delivered := make(map[uint32]struct{}, len(completed)+len(failed))
	for _, uid := range completed {
		delivered[uid] = struct{}{}
	}
	for _, uid := range failed {
		delivered[uid] = struct{}{}
	}

	missing := make([]uint32, 0)
	for _, uid := range requested {
		if _, ok := delivered[uid]; !ok {
			missing = append(missing, uid)
		}
	}
	return missing
}

func partitionMissingUIDs(missing, remaining []uint32) (vanished, retry []uint32) {
	present := make(map[uint32]struct{}, len(remaining))
	for _, uid := range remaining {
		present[uid] = struct{}{}
	}
	for _, uid := range missing {
		if _, ok := present[uid]; ok {
			retry = append(retry, uid)
		} else {
			vanished = append(vanished, uid)
		}
	}
	return vanished, retry
}

// nextUIDCheckpoint returns the highest UID that can be safely skipped on the
// next search. Message-level failures form a barrier; an incomplete fetch or
// failed server-side move/flag operation leaves the prior cursor untouched.
func nextUIDCheckpoint(lastUID uint32, completed, failed []uint32, checkpointComplete bool) uint32 {
	if !checkpointComplete {
		return lastUID
	}
	next := lastUID
	for _, uid := range completed {
		if uid > next {
			next = uid
		}
	}
	var firstFailed uint32
	for _, uid := range failed {
		if uid != 0 && (firstFailed == 0 || uid < firstFailed) {
			firstFailed = uid
		}
	}
	if firstFailed != 0 {
		next = firstFailed - 1
	}
	if next < lastUID {
		return lastUID
	}
	return next
}

// DialAccount opens an IMAP connection using the account's TLS mode and
// optional CA file. Authentication remains the caller's responsibility.
func DialAccount(ctx context.Context, account *emailaccounts.Account) (*imapclient.Client, error) {
	if account == nil {
		return nil, errors.New("emailwatch: account required")
	}
	rootCAs, err := emailaccounts.LoadTLSRootCAs(account.TLSCAFile)
	if err != nil {
		return nil, err
	}
	return dialAccount(ctx, account, rootCAs)
}

func dialAccount(ctx context.Context, account *emailaccounts.Account, rootCAs *x509.CertPool) (*imapclient.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, imapCommandTimeout)
	defer cancel()

	addr := net.JoinHostPort(account.Host, strconv.Itoa(account.Port))
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	closeConn := true
	defer func() {
		if closeConn {
			_ = conn.Close()
		}
	}()
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if account.UseTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: account.Host,
			RootCAs:    rootCAs,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			return nil, fmt.Errorf("TLS handshake %s: %w", addr, err)
		}
		conn = tlsConn
	}
	c, err := imapclient.New(conn)
	if err != nil {
		return nil, fmt.Errorf("IMAP handshake %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	c.Timeout = imapCommandTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < c.Timeout {
			c.Timeout = remaining
		}
	}
	closeConn = false
	return c, nil
}

// connect dials with the account transport and logs in.
func (w *Watcher) connect(ctx context.Context) (*imapclient.Client, error) {
	c, err := dialAccount(ctx, w.account, w.rootCAs)
	if err != nil {
		return nil, err
	}

	switch w.account.AuthMethod {
	case emailaccounts.AuthPassword:
		password, unsealErr := emailaccounts.OpenPassword(w.aead, w.account.SealedSecret)
		if unsealErr != nil {
			_ = c.Logout()
			// Scrub the underlying error — it can contain ciphertext
			// bytes in some crypto backends and there's nothing
			// actionable about the specific failure mode.
			w.log.Warn("emailwatch.unseal_failed", "account_id", w.account.ID)
			return nil, errors.New("emailwatch: unseal password failed")
		}
		if err := c.Login(w.account.Username, password); err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("login %s@%s: %w", w.account.Username, w.account.Host, err)
		}
	case emailaccounts.AuthXOAuth2:
		if w.msal == nil {
			_ = c.Logout()
			return nil, errors.New("emailwatch: xoauth2 client not configured")
		}
		credential, err := emailaccounts.OpenMicrosoftOAuthCredential(w.aead, w.account.SealedSecret)
		if err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("emailwatch: unseal token cache: %w", err)
		}
		client, err := w.msal.ClientFor(credential.ClientID)
		if err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("emailwatch: resolve oauth client: %w", err)
		}
		refreshed, err := client.AcquireTokenSilent(ctx, credential.CacheJSON, w.account.OAuthAccountID)
		if err != nil {
			_ = c.Logout()
			if errors.Is(err, oauth.ErrCacheStale) {
				// Surface the stale-token state via last_error so the
				// UI can prompt the operator to re-run device code.
				// MarkSync is the only write path for last_error;
				// reuse it rather than adding a parallel one.
				_ = emailaccounts.MarkSync(ctx, w.db, w.account.ID, w.account.LastSyncAt, err.Error())
			}
			return nil, fmt.Errorf("emailwatch: acquire token: %w", err)
		}
		if refreshed.Rotated {
			sealed, sealErr := emailaccounts.SealMicrosoftOAuthCredential(w.aead,
				emailaccounts.MicrosoftOAuthCredential{ClientID: credential.ClientID, CacheJSON: refreshed.CacheJSON})
			if sealErr != nil {
				_ = c.Logout()
				return nil, fmt.Errorf("emailwatch: seal rotated cache: %w", sealErr)
			}
			if _, patchErr := emailaccounts.Patch(ctx, w.db, w.account.ID, emailaccounts.AccountPatch{SealedSecret: &sealed}); patchErr != nil {
				// Persistence failure isn't fatal for this poll cycle
				// — MSAL will re-rotate on the next AcquireTokenSilent
				// — but log it.
				w.log.Warn("emailwatch.cache_persist_failed", "err", patchErr, "account_id", w.account.ID)
			}
		}
		if err := c.Authenticate(oauth.XOAUTH2Client(w.account.Username, refreshed.AccessToken)); err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("emailwatch: xoauth2 authenticate: %w", err)
		}
	default:
		_ = c.Logout()
		return nil, fmt.Errorf("emailwatch: unknown auth_method %q", string(w.account.AuthMethod))
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
	// Cap the read at maxAttach*2 — a message with 25 MB attachments can
	// easily be 40 MB with encoding overhead. Read one sentinel byte so an
	// oversized message fails visibly instead of entering CAS truncated.
	limit := w.rawMessageLimit()
	raw, err := io.ReadAll(io.LimitReader(lit, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(raw)) > limit {
		return nil, "", fmt.Errorf("%w: exceeds %d-byte ingest limit", errMessageTooLarge, limit)
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

func (w *Watcher) rawMessageLimit() int64 {
	return w.maxAttach*2 + 8*1024
}

// importOne is the write side: pre-ingest gates (from-allowlist,
// attachments-only) then dedup by Message-ID, put the raw bytes into
// CAS, insert a documents row with mime=message/rfc822, and enqueue
// post-ingest (which fans out attachments as children via
// core/pipeline/eml).
//
// Returns (imported, err) where imported=false is either "gate
// dropped it", "already known" (dedup hit), or "size cap tripped".
// err is only for hard failures the outer loop should log.
func (w *Watcher) importOne(ctx context.Context, raw []byte, msgID string, m *imap.Message) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("empty message body")
	}

	// Pre-ingest gates (from-allowlist + attachments-only). Extracted
	// so unit tests can exercise the drop paths without a CAS + DB
	// fixture; whatever `shouldImport` returns is the authoritative
	// decision.
	hasAttachment, fromHeader, drop := shouldImport(w.account, m.Envelope, raw)
	if drop != "" {
		w.log.Debug("emailwatch.gate_drop", "reason", drop, "from", fromHeader)
		return false, nil
	}

	// Dedup: message-ID + owner scope. Same ID under a different
	// owner is fine (household member forwarded it, etc.).
	if msgID != "" {
		var existing int64
		err := w.db.Read.QueryRowContext(ctx, `
			SELECT id FROM documents
			WHERE owner_id = ? AND email_message_id = ?
			LIMIT 1
		`, w.account.OwnerID, msgID).Scan(&existing)
		if err == nil {
			w.log.Debug("emailwatch.dedup", "msg_id", msgID, "existing", existing)
			return false, w.recordMailboxSource(ctx, existing)
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
	`, w.account.OwnerID, ref.SHA256).Scan(&existingID)
	if err == nil {
		w.log.Debug("emailwatch.blob_dedup", "existing", existingID, "sha", ref.SHA256)
		return false, w.recordMailboxSource(ctx, existingID)
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

	payload, err := BuildPostIngestPayload(ref.SHA256, ref.Size, "message/rfc822", title, w.account.Folder, m.Envelope, hasAttachment, w.account.AttachmentsOnly)
	if err != nil {
		return false, fmt.Errorf("marshal post-ingest payload: %w", err)
	}

	if err := w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents(
				owner_id, original_blob, original_size, title, mime_type,
				jd_category_id, added_at, created_at, updated_at,
				email_message_id
			) VALUES (?, ?, ?, ?, 'message/rfc822', ?, ?, ?, ?, ?)
		`, w.account.OwnerID, ref.SHA256, ref.Size, title,
			inbox, now, created, now,
			nullOrString(msgID))
		if err != nil {
			return err
		}
		docID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := ingestmeta.RecordSource(ctx, tx, docID, ingestmeta.SourceMailbox,
			w.account.Name, w.mailboxSourceDetail(), now); err != nil {
			return err
		}
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

func (w *Watcher) mailboxSourceDetail() string {
	if w.account.Username == "" {
		return w.account.Folder
	}
	return w.account.Username + " / " + w.account.Folder
}

func (w *Watcher) recordMailboxSource(ctx context.Context, docID int64) error {
	return w.db.WriteTx(ctx, func(tx *sql.Tx) error {
		return ingestmeta.RecordSource(ctx, tx, docID, ingestmeta.SourceMailbox,
			w.account.Name, w.mailboxSourceDetail(), time.Now().Unix())
	})
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
