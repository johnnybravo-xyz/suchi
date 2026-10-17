// SPDX-License-Identifier: AGPL-3.0-or-later

package postingest

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/docsplit"
)

func TestAtomicSplitUsesCurrentParentState(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "edited", true: "trashed"}[retired], func(t *testing.T) {
			d, cas := openPostIngestHarness(t)
			parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
			h := New(d, cas, testActions(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at)
				VALUES(2,'other@example.test','Other','member',1,1);
				INSERT INTO jd_categories(system_id,id,area_start,code,name,system)
				VALUES(1,2,0,2,'Filed',0);
				UPDATE documents SET owner_id=2,jd_category_id=2,title='Edited scan',title_owner='human',
				    sensitivity='restricted',source_mtime=123 WHERE id=?;
			`, parentID); err != nil {
				t.Fatal(err)
			}
			if retired {
				if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET trashed_at=1 WHERE id=?`, parentID); err != nil {
					t.Fatal(err)
				}
			}
			binDir := t.TempDir()
			writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\nprintf 'split page'\n")
			t.Setenv("PATH", binDir)
			fanOut, err := h.fanOutSegments(t.Context(), h.log, parentID, []byte("source"),
				[]docsplit.Segment{{Start: 1, End: 1}})
			if retired {
				if err == nil || fanOut {
					t.Fatal("created live child from trashed parent")
				}
				var count int
				if err := d.Read.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM documents WHERE split_parent_id=?`, parentID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("created %d children after trash", count)
				}
				return
			}
			if err != nil || !fanOut {
				t.Fatalf("fanout=%v err=%v", fanOut, err)
			}
			var ownerID, categoryID, mtime int64
			var title, titleOwner, sensitivity string
			if err := d.Read.QueryRowContext(t.Context(), `SELECT owner_id,jd_category_id,title,title_owner,sensitivity,source_mtime
				FROM documents WHERE split_parent_id=?`, parentID).Scan(&ownerID, &categoryID, &title, &titleOwner, &sensitivity, &mtime); err != nil {
				t.Fatal(err)
			}
			if ownerID != 2 || categoryID != 2 || title != "Edited scan (part 1/1)" || titleOwner != "auto" || sensitivity != "restricted" || mtime != 123 {
				t.Fatalf("stale child: owner=%d category=%d title=%q title_owner=%q sensitivity=%q mtime=%d", ownerID, categoryID, title, titleOwner, sensitivity, mtime)
			}
		})
	}
}

func TestCompletedFanoutReplayKeepsParentRetired(t *testing.T) {
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
	h := New(d, cas, testActions(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "qpdf"), "#!/bin/sh\nprintf 'split page'\n")
	t.Setenv("PATH", binDir)
	if fanOut, err := h.fanOutSegments(t.Context(), h.log, parentID, []byte("source"),
		[]docsplit.Segment{{Start: 1, End: 1}}); err != nil || !fanOut {
		t.Fatalf("fanout=%v err=%v", fanOut, err)
	}
	complete, err := h.completedFanout(t.Context(), parentID)
	if err != nil || !complete {
		t.Fatalf("completed fanout=%v err=%v", complete, err)
	}
	var trashed, updated int64
	if err := d.Read.QueryRowContext(t.Context(), `SELECT trashed_at,updated_at FROM documents WHERE id=?`, parentID).Scan(&trashed, &updated); err != nil {
		t.Fatal(err)
	}
	if trashed == 0 || updated == 0 {
		t.Fatalf("parent retirement = trashed:%d updated:%d", trashed, updated)
	}
}
