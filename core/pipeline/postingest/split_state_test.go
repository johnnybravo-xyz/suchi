package postingest

import (
	"io"
	"log/slog"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/pipeline/docsplit"
)

func TestSplitChildUsesCurrentParentState(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "edited", true: "trashed"}[retired], func(t *testing.T) {
			d, cas := openPostIngestHarness(t)
			parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
			h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if _, err := d.ExecWrite(t.Context(), `
				INSERT INTO users(id,email,display_name,role,created_at,updated_at)
				VALUES(2,'other@example.test','Other','member',1,1);
				INSERT INTO jd_categories(system_id,id,area_start,code,name,system)
				VALUES(1,2,0,2,'Filed',0);
				UPDATE documents SET owner_id=2,jd_category_id=2,title='Edited scan',
				    sensitivity='restricted',source_mtime=123 WHERE id=?;
			`, parentID); err != nil {
				t.Fatal(err)
			}
			if retired {
				if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET trashed_at=1 WHERE id=?`, parentID); err != nil {
					t.Fatal(err)
				}
			}
			err := h.createSplitChild(t.Context(), h.log, parentID, 1, 1, docsplit.Segment{}, []byte("split page"))
			if retired {
				if err == nil {
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
			if err != nil {
				t.Fatal(err)
			}
			var ownerID, categoryID, mtime int64
			var title, sensitivity string
			if err := d.Read.QueryRowContext(t.Context(), `SELECT owner_id,jd_category_id,title,sensitivity,source_mtime
				FROM documents WHERE split_parent_id=?`, parentID).Scan(&ownerID, &categoryID, &title, &sensitivity, &mtime); err != nil {
				t.Fatal(err)
			}
			if ownerID != 2 || categoryID != 2 || title != "Edited scan (part 1/1)" || sensitivity != "restricted" || mtime != 123 {
				t.Fatalf("stale child: owner=%d category=%d title=%q sensitivity=%q mtime=%d", ownerID, categoryID, title, sensitivity, mtime)
			}
		})
	}
}

func TestSplitRetryPreservesTrashRetention(t *testing.T) {
	d, cas := openPostIngestHarness(t)
	parentID := seedPostIngestDocument(t, d, cas, "application/pdf", []byte("source scan"))
	h := New(d, cas, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := d.ExecWrite(t.Context(), `UPDATE documents SET trashed_at=1,updated_at=2 WHERE id=?`, parentID); err != nil {
		t.Fatal(err)
	}
	if err := h.softDeleteParent(t.Context(), parentID); err != nil {
		t.Fatal(err)
	}
	var trashed, updated int64
	if err := d.Read.QueryRowContext(t.Context(), `SELECT trashed_at,updated_at FROM documents WHERE id=?`, parentID).Scan(&trashed, &updated); err != nil {
		t.Fatal(err)
	}
	if trashed != 1 || updated != 2 {
		t.Fatalf("retry changed trash history: trashed_at=%d updated_at=%d", trashed, updated)
	}
}
