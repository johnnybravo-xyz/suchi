// SPDX-License-Identifier: AGPL-3.0-or-later

// Package systems owns filing-system identity and entry, independently of the
// taxonomy tree and document permissions. Callers must check both permissions.
package systems

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"
)

const DefaultID int64 = 1

// Queryer accepts either a read snapshot or a caller-owned transaction.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type System struct {
	ID              int64
	Code            string
	Name            string
	Taxonomy        string
	InboxCategoryID int64
	PresetID        string
	PresetVersion   int
	PresetSHA256    string
	AuthoringJSON   string
	CreatedAt       int64
	UpdatedAt       int64
}

const columns = `id,code,name,taxonomy,COALESCE(inbox_category_id,0),COALESCE(preset_id,''),COALESCE(preset_version,0),COALESCE(preset_sha256,''),COALESCE(authoring_json,''),created_at,updated_at`

func scan(row *sql.Row) (System, error) {
	var s System
	err := row.Scan(&s.ID, &s.Code, &s.Name, &s.Taxonomy, &s.InboxCategoryID, &s.PresetID, &s.PresetVersion, &s.PresetSHA256, &s.AuthoringJSON, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func Get(ctx context.Context, q Queryer, id int64) (System, error) {
	return scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM jd_systems WHERE id=?`, id))
}

func ByCode(ctx context.Context, q Queryer, code string) (System, error) {
	if !ValidCode(code) {
		return System{}, sql.ErrNoRows
	}
	return scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM jd_systems WHERE code=?`, code))
}

// Introduced is derived, never a separately mutable preference.
func Introduced(ctx context.Context, q Queryer) (bool, error) {
	var introduced bool
	err := q.QueryRowContext(ctx, `SELECT code<>'' FROM jd_systems WHERE id=?`, DefaultID).Scan(&introduced)
	return introduced, err
}

func CanEnter(ctx context.Context, q Queryer, userID, systemID int64) (bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM users u JOIN jd_systems s ON s.id=?
		WHERE u.id=? AND u.disabled=0 AND (u.role='admin' OR EXISTS (
			SELECT 1 FROM jd_system_members m WHERE m.system_id=s.id AND m.user_id=u.id
		)))`, systemID, userID).Scan(&allowed)
	return allowed, err
}

func ValidCode(code string) bool {
	return len(code) == 3 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= '0' && code[1] <= '9' && code[2] >= '0' && code[2] <= '9'
}

func validName(name string) bool {
	return utf8.ValidString(name) && strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= 80
}

// Address returns the SYS.AC category address. The original archive has no
// address before system introduction (empty code); callers show the category code.
func Address(systemCode string, categoryCode int) string {
	if !ValidCode(systemCode) || categoryCode < 0 || categoryCode > 99 {
		return ""
	}
	return string([]byte{
		systemCode[0], systemCode[1], systemCode[2], '.',
		byte(categoryCode/10) + '0', byte(categoryCode%10) + '0',
	})
}

type FilingAddress struct {
	SystemCode   string
	CategoryCode int
}

var ErrBadAddress = errors.New("invalid filing address")

func ParseAddress(value string) (FilingAddress, error) {
	if len(value) != 6 || !ValidCode(value[:3]) || value[3] != '.' ||
		value[4] < '0' || value[4] > '9' || value[5] < '0' || value[5] > '9' {
		return FilingAddress{}, ErrBadAddress
	}
	return FilingAddress{
		SystemCode:   value[:3],
		CategoryCode: int(value[4]-'0')*10 + int(value[5]-'0'),
	}, nil
}

// Create leaves the deferred Inbox pointer NULL until the caller has seeded
// this system's protected category in the same transaction.
func Create(ctx context.Context, tx *sql.Tx, code, name, taxonomy string, now int64) (int64, error) {
	if !ValidCode(code) || !validName(name) || (taxonomy != "jd" && taxonomy != "flat") {
		return 0, errors.New("invalid filing system")
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO jd_systems(code,name,taxonomy,created_at,updated_at) VALUES(?,?,?,?,?)`, code, name, taxonomy, now, now)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func SetInbox(ctx context.Context, tx *sql.Tx, systemID, categoryID, now int64) error {
	return update(ctx, tx, `UPDATE jd_systems SET inbox_category_id=?,updated_at=? WHERE id=?`, categoryID, now, systemID)
}

func Rename(ctx context.Context, tx *sql.Tx, systemID int64, name string, now int64) error {
	if !validName(name) {
		return errors.New("system name must contain 1 to 80 characters")
	}
	return update(ctx, tx, `UPDATE jd_systems SET name=?,updated_at=? WHERE id=?`, name, now, systemID)
}

// SetCode can name the original archive exactly once. SQL guards also protect
// callers which write directly inside their import transaction.
func SetCode(ctx context.Context, tx *sql.Tx, systemID int64, code string, now int64) error {
	if !ValidCode(code) {
		return errors.New("invalid system code")
	}
	return update(ctx, tx, `UPDATE jd_systems SET code=?,updated_at=? WHERE id=?`, code, now, systemID)
}

func update(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	r, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
