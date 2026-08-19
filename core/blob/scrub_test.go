package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/blob"
)

func TestScrubReportsMissingAndQuarantinesCorruptBlobs(t *testing.T) {
	root := t.TempDir()
	cas, err := blob.New(root)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := cas.Put(bytes.NewReader([]byte("healthy")))
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := cas.Put(bytes.NewReader([]byte("original")))
	if err != nil {
		t.Fatal(err)
	}

	badContent := []byte("tampered")
	corruptPath := casPath(root, corrupt.SHA256)
	if err := os.WriteFile(corruptPath, badContent, 0o640); err != nil {
		t.Fatal(err)
	}
	actual := sha256.Sum256(badContent)
	actualHex := hex.EncodeToString(actual[:])
	missing := strings.Repeat("f", 64)

	report, err := cas.Scrub(context.Background(), []string{
		healthy.SHA256,
		corrupt.SHA256,
		missing,
		missing,
		"demo:metadata-only",
	}, blob.ScrubOptions{Quarantine: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Referenced != 3 || report.Checked != 2 || report.BytesChecked != int64(len("healthy")+len(badContent)) {
		t.Fatalf("scrub counts = referenced:%d checked:%d bytes:%d",
			report.Referenced, report.Checked, report.BytesChecked)
	}
	if len(report.Missing) != 1 || report.Missing[0] != missing {
		t.Fatalf("missing = %v", report.Missing)
	}
	if len(report.Corrupt) != 1 {
		t.Fatalf("corrupt = %+v", report.Corrupt)
	}
	issue := report.Corrupt[0]
	if issue.Expected != corrupt.SHA256 || issue.Actual != actualHex || issue.QuarantinedTo == "" {
		t.Fatalf("corrupt issue = %+v", issue)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %+v", report.Failures)
	}
	if _, err := os.Stat(issue.QuarantinedTo); err != nil {
		t.Fatalf("quarantined file: %v", err)
	}
	if _, err := cas.Stat(corrupt.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("corrupt CAS path still exists: %v", err)
	}
	if _, err := cas.Stat(healthy.SHA256); err != nil {
		t.Fatalf("healthy blob moved: %v", err)
	}
}

func casPath(root, sum string) string {
	return filepath.Join(root, "blobs", "sha256", sum[:2], sum[2:4], sum[4:6], sum)
}
