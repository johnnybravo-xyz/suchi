// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/db/migrations"
)

// Run with: go test ./core/api -run '^$' -bench 'IndexCorpus|SearchLanguages' -benchtime 1x
// The corpus size is fixed so runs compare.
const benchDocs = 20000

var (
	benchEnglish  = strings.Fields("invoice electricity bill payment account statement insurance policy renewal tax return receipt warranty contract tenant landlord deposit salary pension medical prescription")
	benchGerman   = strings.Fields("Rechnung Krankenversicherung Haftpflichtversicherung Kontoauszug Überweisung Steuerbescheid Mietvertrag Nebenkostenabrechnung Gehaltsabrechnung Rentenversicherung Kündigung Beitrag")
	benchJapanese = []string{"請求書", "領収書", "東京都", "保険証券", "契約書", "銀行明細", "税金", "給与明細", "住民票", "電気料金", "を送付します", "のお知らせ"}
)

// benchCorpus writes deterministic ~2 KB documents: 60% English, 25% German,
// 15% Japanese (no spaces between words).
func benchCorpus(b *testing.B, d *db.DB, n int) {
	b.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title, content,
			jd_category_id, sensitivity, created_at, updated_at) VALUES (1, ?, 1, ?, 1, ?, ?, 1, 'public', ?, 0)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 1; i <= n; i++ {
			vocab, sep := benchEnglish, " "
			switch roll := rng.IntN(100); {
			case roll >= 85:
				vocab, sep = benchJapanese, ""
			case roll >= 60:
				vocab = benchGerman
			}
			var content strings.Builder
			for content.Len() < 2000 {
				content.WriteString(vocab[rng.IntN(len(vocab))])
				content.WriteString(sep)
			}
			title := vocab[rng.IntN(len(vocab))] + sep + vocab[rng.IntN(len(vocab))]
			if _, err := stmt.Exec(i, fmt.Sprintf("bench-%d", i), title, content.String(), i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

func openBenchServer(b *testing.B, trigram bool) *Server {
	b.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		b.Fatal(err)
	}
	setup := `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES (1, 'bench@example.test', 'bench', 'admin', 0, 0);
		INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (1, 10, 19, 'Bench', 0);
		INSERT INTO jd_categories(system_id, id, area_start, code, name, system) VALUES (1, 1, 10, 10, 'Inbox', 1);`
	if !trigram {
		setup += `DROP TRIGGER documents_trigram_ai; DROP TRIGGER documents_trigram_au; DROP TRIGGER documents_trigram_ad;`
	}
	if _, err := d.Write.ExecContext(ctx, setup); err != nil {
		b.Fatal(err)
	}
	return &Server{DB: d, Log: log}
}

func indexBytes(b *testing.B, d *db.DB, prefix string) float64 {
	b.Helper()
	var size float64
	if err := d.Read.QueryRow(`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat WHERE name LIKE ?`, prefix+"%").Scan(&size); err != nil {
		b.Fatal(err)
	}
	return size
}

func BenchmarkIndexCorpus(b *testing.B) {
	for _, trigram := range []bool{false, true} {
		b.Run(map[bool]string{false: "word_only", true: "word_and_trigram"}[trigram], func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				s := openBenchServer(b, trigram)
				b.StartTimer()
				benchCorpus(b, s.DB, benchDocs)
				b.StopTimer()
				b.ReportMetric(indexBytes(b, s.DB, "documents_fts")/(1<<20), "word_MiB")
				b.ReportMetric(indexBytes(b, s.DB, "documents_trigram")/(1<<20), "trigram_MiB")
				var text float64
				if err := s.DB.Read.QueryRow(`SELECT SUM(length(CAST(title AS BLOB)) + length(CAST(content AS BLOB))) FROM documents`).Scan(&text); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(text/(1<<20), "text_MiB")
			}
		})
	}
}

func BenchmarkSearchLanguages(b *testing.B) {
	s := openBenchServer(b, true)
	benchCorpus(b, s.DB, benchDocs)
	ctx := auth.WithPrincipal(context.Background(), adminPrincipal(1))
	for _, tc := range []struct{ name, query string }{
		{"english_word", "insurance"},
		{"english_rare", "warranty landlord"},
		{"german_infix", "versicherung"},
		{"japanese", "保険証券"},
		{"japanese_two_chars", "税金"},
		{"negated", "insurance -policy"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for range b.N {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest("GET", "/api/search/?q="+url.QueryEscape(tc.query), nil).WithContext(ctx)
				s.Search(recorder, request)
				if recorder.Code != 200 {
					b.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}

// Run with: go test ./core/api -run '^$' -bench ListDocumentsQuery -benchtime 5x
// Saved views and filtered lists page through ListDocuments; plain lists stay
// in the set so a text-query change cannot slow the index-ordered path.
func BenchmarkListDocumentsQuery(b *testing.B) {
	s := openBenchServer(b, true)
	benchCorpus(b, s.DB, benchDocs)
	ctx := auth.WithPrincipal(context.Background(), adminPrincipal(1))
	for _, tc := range []struct{ name, query string }{
		{"plain", ""},
		{"filters_only", "version:all"},
		{"english_word", "insurance"},
		{"english_rare", "warranty landlord"},
		{"japanese_two_chars", "税金"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for range b.N {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest("GET", "/api/documents/?q="+url.QueryEscape(tc.query), nil).WithContext(ctx)
				s.ListDocuments(recorder, request)
				if recorder.Code != 200 {
					b.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}
