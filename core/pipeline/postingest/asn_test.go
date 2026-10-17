// SPDX-License-Identifier: AGPL-3.0-or-later

package postingest

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	ingestmeta "github.com/johnnybravo-xyz/suchi/core/ingest"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/docsplit"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func TestASNValidationPrecedesPreconsumeMutation(t *testing.T) {
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("%PDF malformed ASN fixture"))
	binDir := t.TempDir()
	qrPath := filepath.Join(binDir, "page.png")
	writeQRPNG(t, qrPath, "ASN+42")
	writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\n/bin/cat in.pdf\n")
	writeExecutable(t, filepath.Join(binDir, "pdftoppm"), fmt.Sprintf("#!/bin/sh\n/bin/cp %q \"$5-1.png\"\n", qrPath))
	preconsumeMarker := filepath.Join(binDir, "preconsume-ran")
	preconsume := filepath.Join(binDir, "preconsume")
	writeExecutable(t, preconsume, fmt.Sprintf("#!/bin/sh\n: > %q\nprintf '{}\\n'\n", preconsumeMarker))
	t.Setenv("PATH", binDir)

	h := New(d, cas, testActions(t), slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithScanSplit(ScanSplit{ASNMode: docsplit.ASNModeSplit, ASNPrefix: "ASN", DPI: 300}),
		WithPreConsume(preconsume),
	)
	err := h.Handle(t.Context(), pluginapi.Event{DocID: parentID, Payload: map[string]any{"raw": `{}`}})
	if !errors.Is(err, jobs.ErrTerminal) || !strings.Contains(err.Error(), docsplit.CodeMalformedASN) {
		t.Fatalf("Handle error = %v, want terminal malformed ASN", err)
	}
	if _, err := os.Stat(preconsumeMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-consume ran before validation: %v", err)
	}
	var (
		archiveNumber sql.NullInt64
		trashed       sql.NullInt64
		children      int
	)
	if err := d.Read.QueryRowContext(t.Context(), `
		SELECT archive_serial_number, trashed_at,
		       (SELECT COUNT(*) FROM documents WHERE split_origin_id = d.id)
		FROM documents d WHERE d.id = ?
	`, parentID).Scan(&archiveNumber, &trashed, &children); err != nil {
		t.Fatal(err)
	}
	if archiveNumber.Valid || trashed.Valid || children != 0 {
		t.Fatalf("terminal validation mutated staging row: asn=%v trash=%v children=%d", archiveNumber, trashed, children)
	}
}

func TestASNFanoutIsAtomicAndPreservesContext(t *testing.T) {
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
	if _, err := d.ExecWrite(t.Context(), `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(2,'viewer@example.test','Viewer','member',1,1);
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at,created_by)
		VALUES('document',?,'user',2,1,1,1)
	`, parentID); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteTx(t.Context(), func(tx *sql.Tx) error {
		return ingestmeta.RecordSource(t.Context(), tx, parentID, ingestmeta.SourceWatchedFolder, "scanner", "batch.pdf", 1)
	}); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\nprintf 'selected-%s' \"$4\"\n")
	t.Setenv("PATH", binDir)
	h := New(d, cas, testActions(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	plan := &docsplit.Result{Mode: docsplit.ModeASNSplit, Segments: []docsplit.Segment{
		{Start: 1, End: 2, ASN: 42, MarkerPage: 1},
		{Start: 3, End: 4, ASN: 43, MarkerPage: 3},
	}}
	fanOut, err := h.fanOutPlan(t.Context(), h.log, parentID, "", []byte("source"), plan,
		automations.Context{Filename: "batch.pdf", SourcePath: "/scanner/batch.pdf", MailRuleID: 7},
		"application/pdf", nil)
	if err != nil || !fanOut {
		t.Fatalf("fanout=%v err=%v", fanOut, err)
	}

	rows, err := d.Read.QueryContext(t.Context(), `
		SELECT d.archive_serial_number, d.split_index, j.payload,
		       (SELECT COUNT(*) FROM object_acls a WHERE a.object_kind='document' AND a.object_id=d.id),
		       (SELECT COUNT(*) FROM document_sources s WHERE s.document_id=d.id)
		FROM documents d JOIN jobs j ON j.doc_id=d.id AND j.kind=?
		WHERE d.split_origin_id=? ORDER BY d.split_index
	`, Kind, parentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantASN := []int64{42, 43}
	index := 0
	for rows.Next() {
		var asnValue int64
		var splitIndex, aclCount, sourceCount int
		var payload string
		if err := rows.Scan(&asnValue, &splitIndex, &payload, &aclCount, &sourceCount); err != nil {
			t.Fatal(err)
		}
		if index >= len(wantASN) || asnValue != wantASN[index] || splitIndex != index+1 || aclCount != 1 || sourceCount != 1 {
			t.Fatalf("child %d state: asn=%d split=%d acl=%d source=%d", index, asnValue, splitIndex, aclCount, sourceCount)
		}
		if !strings.Contains(payload, `"filename":"batch.pdf"`) ||
			!strings.Contains(payload, `"source_path":"/scanner/batch.pdf"`) ||
			!strings.Contains(payload, `"mail_rule_id":7`) ||
			!strings.Contains(payload, `"skip_marker_planning":true`) ||
			!strings.Contains(payload, `"protect_first_page":true`) {
			t.Fatalf("child payload = %s", payload)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != 2 {
		t.Fatalf("children = %d, want 2", index)
	}
	complete, err := h.completedFanout(t.Context(), parentID)
	if err != nil || !complete {
		t.Fatalf("completed=%v err=%v", complete, err)
	}
}

func TestASNFanoutConflictIsTerminalAndAtomic(t *testing.T) {
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
	inserted, err := d.ExecWrite(t.Context(), `
		INSERT INTO documents(system_id,owner_id,original_blob,original_size,title,mime_type,
			jd_category_id,archive_serial_number,created_at,updated_at)
		SELECT system_id,owner_id,'conflict',1,'Existing','application/pdf',jd_category_id,42,1,1
		FROM documents WHERE id=?
	`, parentID)
	if err != nil {
		t.Fatal(err)
	}
	holderID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	h := New(d, cas, testActions(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	plan := &docsplit.Result{Mode: docsplit.ModeASNSplit, Segments: []docsplit.Segment{
		{Start: 1, End: 1, ASN: 42, MarkerPage: 1},
		{Start: 2, End: 2, ASN: 43, MarkerPage: 2},
	}}
	fanOut, err := h.fanOutPlan(t.Context(), h.log, parentID, "", []byte("source"), plan,
		automations.Context{}, "application/pdf", nil)
	wantDetail := fmt.Sprintf("archive_serial_number_conflict page=1 asn=42 doc=%d", holderID)
	if fanOut || !errors.Is(err, jobs.ErrTerminal) || !strings.HasSuffix(err.Error(), wantDetail) {
		t.Fatalf("fanout=%v err=%v", fanOut, err)
	}
	var children int
	var trashed sql.NullInt64
	if err := d.Read.QueryRowContext(t.Context(), `
		SELECT (SELECT COUNT(*) FROM documents WHERE split_origin_id=?), trashed_at
		FROM documents WHERE id=?
	`, parentID, parentID).Scan(&children, &trashed); err != nil {
		t.Fatal(err)
	}
	if children != 0 || trashed.Valid {
		t.Fatalf("conflict leaked state: children=%d trashed=%v", children, trashed)
	}
}

func writeQRPNG(t *testing.T, path, text string) {
	t.Helper()
	matrix, err := qrcode.NewQRCodeWriter().Encode(text, gozxing.BarcodeFormat_QR_CODE, 240, 240, nil)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewGray(image.Rect(0, 0, matrix.GetWidth(), matrix.GetHeight()))
	for y := 0; y < matrix.GetHeight(); y++ {
		for x := 0; x < matrix.GetWidth(); x++ {
			value := uint8(255)
			if matrix.Get(x, y) {
				value = 0
			}
			img.SetGray(x, y, color.Gray{Y: value})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
