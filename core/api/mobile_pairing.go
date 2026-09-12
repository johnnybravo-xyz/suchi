package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

const mobilePairingScopes = auth.ScopeDocumentsRead + "," + auth.ScopeDocumentsWrite

type mobilePairingResponse struct {
	PairingURL string `json:"pairing_url"`
	QRDataURL  string `json:"qr_data_url"`
	ExpiresAt  int64  `json:"expires_at"`
	Name       string `json:"name"`
	Code       string `json:"code"`
}

// CreateMobilePairing replaces the signed-in user's pending pairing. Only the
// code digest reaches disk; the QR is generated locally and returned once.
func (s *Server) CreateMobilePairing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := s.mobilePairingSession(w, r)
	if p == nil {
		return
	}
	if s.TokenIssuer == nil {
		s.writeError(w, http.StatusNotImplemented, "no_issuer", "token issuance is unavailable")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Suchi mobile"
	}
	if utf8.RuneCountInString(name) > 64 {
		s.writeError(w, http.StatusBadRequest, "bad_name", "name must be <= 64 chars")
		return
	}
	origin, err := mobilePairingOrigin(s.PublicURL)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "pairing_unavailable", "configure PUBLIC_URL as an HTTPS origin, or HTTP on localhost or a private literal IP address, before pairing")
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		s.serverErr(w, "mobile_pairing.random", err)
		return
	}
	code := hex.EncodeToString(raw[:])
	link := url.URL{Scheme: "suchi", Host: "pair", RawQuery: url.Values{
		"v": {"1"}, "server": {origin}, "code": {code},
	}.Encode()}
	matrix, err := qrcode.NewQRCodeWriter().Encode(link.String(), gozxing.BarcodeFormat_QR_CODE, 320, 320, nil)
	if err != nil {
		// Encoder errors can contain the input. Never log a pairing secret.
		s.Log.Error("api.mobile_pairing.qr")
		s.writeError(w, http.StatusInternalServerError, "internal", "could not generate pairing QR")
		return
	}
	var qr bytes.Buffer
	if err := png.Encode(&qr, matrix); err != nil {
		s.serverErr(w, "mobile_pairing.png", err)
		return
	}
	expires := time.Now().Add(5 * time.Minute).Unix()
	sum := sha256.Sum256([]byte(code))
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `DELETE FROM mobile_pairings WHERE expires_at <= ?`, time.Now().Unix())
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(r.Context(), `
			INSERT INTO mobile_pairings(user_id, code_hash, name, expires_at)
			SELECT id, ?, ?, ? FROM users WHERE id = ? AND disabled = 0
			ON CONFLICT(user_id) DO UPDATE SET
			  code_hash = excluded.code_hash, name = excluded.name, expires_at = excluded.expires_at
		`, hex.EncodeToString(sum[:]), name, expires, p.UserID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n == 0 {
			return sql.ErrNoRows
		}
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if err != nil {
		s.serverErr(w, "mobile_pairing.create", err)
		return
	}
	s.writeJSON(w, http.StatusCreated, mobilePairingResponse{
		PairingURL: link.String(), QRDataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(qr.Bytes()),
		ExpiresAt: expires, Name: name, Code: code,
	})
}

func (s *Server) DeleteMobilePairing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := s.mobilePairingSession(w, r)
	if p == nil {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &body); err != nil || !validMobilePairingCode(body.Code) {
		s.writeError(w, http.StatusBadRequest, "pairing_invalid", "pairing code is invalid or expired")
		return
	}
	sum := sha256.Sum256([]byte(body.Code))
	if _, err := s.DB.ExecWrite(r.Context(), `DELETE FROM mobile_pairings WHERE user_id = ? AND code_hash = ?`,
		p.UserID, hex.EncodeToString(sum[:])); err != nil {
		s.serverErr(w, "mobile_pairing.delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExchangeMobilePairing consumes the code before issuing the durable token.
// TokenIssuer owns its own write transaction, so it must run after commit.
// An issuance failure deliberately burns the code; start a new pairing.
func (s *Server) ExchangeMobilePairing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &body); err != nil || !validMobilePairingCode(body.Code) || s.demo.Enabled {
		s.writeError(w, http.StatusBadRequest, "pairing_invalid", "pairing code is invalid or expired")
		return
	}
	sum := sha256.Sum256([]byte(body.Code))
	var userID int64
	var name string
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.Context(), `
			DELETE FROM mobile_pairings
			WHERE code_hash = ? AND expires_at > ?
			  AND user_id IN (SELECT id FROM users WHERE disabled = 0)
			RETURNING user_id, name
		`, hex.EncodeToString(sum[:]), time.Now().Unix()).Scan(&userID, &name)
	})
	if errors.Is(err, sql.ErrNoRows) {
		s.writeError(w, http.StatusBadRequest, "pairing_invalid", "pairing code is invalid or expired")
		return
	}
	if err != nil {
		s.serverErr(w, "mobile_pairing.consume", err)
		return
	}
	if s.TokenIssuer == nil {
		s.writeError(w, http.StatusNotImplemented, "no_issuer", "token issuance is unavailable")
		return
	}
	token, err := s.TokenIssuer(r.Context(), userID, name, mobilePairingScopes, auth.TokenSourceMobilePairing)
	if err != nil {
		s.Log.Error("api.mobile_pairing.issue", "user_id", userID)
		s.writeError(w, http.StatusInternalServerError, "internal", "could not issue mobile token; start a new pairing")
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: &pluginapi.Principal{Kind: "user", UserID: userID}, Action: "api_token.create",
		ObjectKind: "api_token", After: map[string]any{"name": name, "scopes": mobilePairingScopes, "method": "mobile_pairing"},
		RequestID: logx.RequestID(r.Context()),
	})
	s.writeJSON(w, http.StatusOK, map[string]string{"token": token, "name": name, "scopes": mobilePairingScopes})
}

func (s *Server) mobilePairingSession(w http.ResponseWriter, r *http.Request) *pluginapi.Principal {
	p := auth.FromContext(r.Context())
	if p == nil || p.UserID <= 0 {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return nil
	}
	if p.Kind != "user" || s.demo.Enabled {
		s.writeError(w, http.StatusForbidden, "forbidden", "pairing requires a browser or OIDC session outside demo mode")
		return nil
	}
	return p
}

func validMobilePairingCode(code string) bool {
	if len(code) != 64 {
		return false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func mobilePairingOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid origin")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", errors.New("invalid origin")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return "", errors.New("invalid origin port")
		}
		port = strconv.FormatUint(number, 10)
	}
	if u.Scheme == "http" && host != "localhost" {
		// Match the mobile ServerOrigin policy, which is narrower than the
		// integration-egress local-host policy: no DNS names or link-local IPs.
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Is4In6() || addr.Zone() != "" ||
			(!addr.IsLoopback() && !addr.IsPrivate()) {
			return "", errors.New("mobile pairing requires HTTPS or approved local HTTP")
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}
