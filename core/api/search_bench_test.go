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
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Run with: go test ./core/api -run '^$' -bench 'IndexCorpus|SearchLanguages' -benchtime 1x
// The corpus size is fixed so runs compare.
const benchDocs = 20000

// benchScopes cover a large system, a 200-document system beside it, and a
// member whose visibility comes from ownership and a group ACL.
var benchScopes = []struct {
	name      string
	principal *pluginapi.Principal
	systemID  int64
}{
	{"admin_large", adminPrincipal(1), 1},
	{"admin_small", adminPrincipal(1), 2},
	{"member_large", memberPrincipal(2), 1},
}

var (
	benchEnglish  = strings.Fields("invoice electricity bill payment account statement insurance policy renewal tax return receipt warranty contract tenant landlord deposit salary pension medical prescription")
	benchGerman   = strings.Fields("Rechnung Krankenversicherung Haftpflichtversicherung Kontoauszug Überweisung Steuerbescheid Mietvertrag Nebenkostenabrechnung Gehaltsabrechnung Rentenversicherung Kündigung Beitrag")
	benchJapanese = []string{"請求書", "領収書", "東京都", "保険証券", "契約書", "銀行明細", "税金", "給与明細", "住民票", "電気料金", "を送付します", "のお知らせ"}
)

// benchCorpus writes deterministic ~2 KB documents: 60% English, 25% German,
// 15% Japanese (no spaces between words). Every 100th document belongs to
// system 2, every 4th is owned by the member, and every 5th is shared with
// the member's group.
func benchCorpus(b *testing.B, d *db.DB, n int) {
	b.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO documents(system_id, id, owner_id, original_blob, original_size, title, content,
			jd_category_id, sensitivity, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, 'public', ?, 0)`)
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
			systemID, owner := 1, 1
			if i%100 == 0 {
				systemID = 2
			}
			if i%4 == 0 {
				owner = 2
			}
			if _, err := stmt.Exec(systemID, i, owner, fmt.Sprintf("bench-%d", i), title, content.String(), systemID, i); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO object_acls(object_kind, object_id, principal_kind, principal_id, perm_bits, created_at)
			SELECT 'document', id, 'group', 1, 1, 0 FROM documents WHERE id % 5 = 0`)
		return err
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
		INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES
			(1, 'bench@example.test', 'bench', 'admin', 0, 0), (2, 'member@example.test', 'member', 'member', 0, 0);
		INSERT INTO groups(id, name, created_at, updated_at) VALUES (1, 'Bench readers', 0, 0);
		INSERT INTO group_members(group_id, user_id, created_at) VALUES (1, 2, 0);
		INSERT INTO jd_systems(id, code, name, taxonomy, created_at, updated_at) VALUES (2, 'S02', 'Small', 'jd', 0, 0);
		INSERT INTO jd_system_members(system_id, user_id, created_at) VALUES (2, 2, 0);
		INSERT INTO jd_areas(system_id, code_start, code_end, name, position) VALUES (1, 10, 19, 'Bench', 0), (2, 10, 19, 'Small', 0);
		INSERT INTO jd_categories(system_id, id, area_start, code, name, system) VALUES (1, 1, 10, 10, 'Inbox', 1), (2, 2, 10, 10, 'Inbox', 1);`
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

func benchContext(principal *pluginapi.Principal, systemID int64) context.Context {
	return context.WithValue(auth.WithPrincipal(context.Background(), principal), systemContextKey{}, systemID)
}

func BenchmarkSearchLanguages(b *testing.B) {
	s := openBenchServer(b, true)
	benchCorpus(b, s.DB, benchDocs)
	for _, scope := range benchScopes {
		ctx := benchContext(scope.principal, scope.systemID)
		for _, tc := range []struct{ name, query string }{
			{"english_word", "insurance"},
			{"english_rare", "warranty landlord"},
			{"no_hit", "quixotic"},
			{"german_infix", "versicherung"},
			{"japanese", "保険証券"},
			{"japanese_two_chars", "税金"},
			{"negated", "insurance -policy"},
			{"negated_two_chars", "insurance -税金"},
		} {
			b.Run(scope.name+"/"+tc.name, func(b *testing.B) {
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
}

// Run with: go test ./core/api -run '^$' -bench ListDocumentsQuery -benchtime 5x
// Saved views and filtered lists page through ListDocuments; plain lists stay
// in the set so a text-query change cannot slow the index-ordered path.
func BenchmarkListDocumentsQuery(b *testing.B) {
	s := openBenchServer(b, true)
	benchCorpus(b, s.DB, benchDocs)
	for _, scope := range benchScopes {
		ctx := benchContext(scope.principal, scope.systemID)
		for _, tc := range []struct{ name, query string }{
			{"plain", ""},
			{"filters_only", "version:all"},
			{"english_word", "insurance"},
			{"english_rare", "warranty landlord"},
			{"japanese_two_chars", "税金"},
			{"negated_two_chars", "insurance -税金"},
		} {
			b.Run(scope.name+"/"+tc.name, func(b *testing.B) {
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
}

// Run with: go test ./core/api -run '^$' -bench RewriteText -benchtime 20x
// Pipelines often write back the text a document already has; each op
// rewrites 100 documents. Each case gets its own corpus because index merge
// work left by one case would bill the next.
func BenchmarkRewriteText(b *testing.B) {
	for _, tc := range []struct{ name, set string }{
		{"unchanged", "title = title, content = content"},
		{"changed", "content = content || ' x'"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer()
			s := openBenchServer(b, true)
			benchCorpus(b, s.DB, benchDocs)
			b.StartTimer()
			for i := range b.N {
				first := (i*100)%(benchDocs-100) + 1
				err := s.DB.WriteTx(context.Background(), func(tx *sql.Tx) error {
					_, err := tx.Exec(`UPDATE documents SET `+tc.set+` WHERE id BETWEEN ? AND ?`, first, first+99)
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
