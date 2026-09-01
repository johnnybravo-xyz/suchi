package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const maxDocumentScopeIDs = 100

type documentScope struct {
	Query            string
	DocumentIDs      []int64
	JDCategoryID     int64
	Sensitivity      string
	DocumentTypeID   int64
	TagIDs           []int64
	CorrespondentIDs []int64
	CreatedAtGTE     *int64
	CreatedAtLTE     *int64
	Language         string
}

type documentScopeError struct {
	Code    string
	Message string
}

func (e *documentScopeError) Error() string { return e.Message }

func documentScopeFromQuery(values url.Values) (documentScope, error) {
	scope := documentScope{Query: strings.TrimSpace(values.Get("q"))}
	var err error
	if scope.DocumentIDs, err = parseBoundedCSVIDs(values.Get("document_ids"), maxDocumentScopeIDs); err != nil {
		return documentScope{}, &documentScopeError{
			Code: "bad_document_ids", Message: fmt.Sprintf("document_ids must contain at most %d positive integers", maxDocumentScopeIDs),
		}
	}
	if scope.JDCategoryID, err = optionalPositiveID(values.Get("jd_category_id")); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_jd_category_id", Message: "jd_category_id must be a positive integer"}
	}
	scope.Sensitivity = values.Get("sensitivity")
	if scope.Sensitivity != "" && !SensitivityLevels[scope.Sensitivity] {
		return documentScope{}, &documentScopeError{Code: "bad_sensitivity", Message: "sensitivity must be one of \"\", public, internal, confidential, restricted"}
	}
	if scope.DocumentTypeID, err = optionalPositiveID(values.Get("document_type__id")); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_document_type_id", Message: "document_type__id must be a positive integer"}
	}
	if scope.TagIDs, err = parseBoundedCSVIDs(values.Get("tags__id__in"), maxDocumentScopeIDs); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_tags", Message: "tags__id__in " + err.Error()}
	}
	if scope.CorrespondentIDs, err = parseBoundedCSVIDs(values.Get("correspondents__id__in"), maxDocumentScopeIDs); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_correspondents", Message: "correspondents__id__in " + err.Error()}
	}
	if scope.CreatedAtGTE, err = optionalNonNegativeInt(values.Get("created_at__gte")); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_created_at_gte", Message: "created_at__gte must be a non-negative unix seconds integer"}
	}
	if scope.CreatedAtLTE, err = optionalNonNegativeInt(values.Get("created_at__lte")); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_created_at_lte", Message: "created_at__lte must be a non-negative unix seconds integer"}
	}
	if scope.Language, err = normalizedScopeLanguage(values.Get("lang")); err != nil {
		return documentScope{}, &documentScopeError{Code: "bad_lang", Message: "lang must be a 2 or 3 letter language code"}
	}
	return scope, nil
}

func documentScopeFromSavedViewJSON(raw string) (documentScope, error) {
	normalized, err := NormalizeSavedViewFilterJSON(raw)
	if err != nil {
		return documentScope{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(normalized))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return documentScope{}, err
	}
	scope := documentScope{}
	if value, ok := values["q"].(string); ok {
		scope.Query = value
	}
	if scope.DocumentIDs, err = scopeIDs(values["document_ids"]); err != nil || len(scope.DocumentIDs) > maxDocumentScopeIDs {
		return documentScope{}, &savedViewFilterError{message: "filter key document_ids must contain 1 to 100 ids"}
	}
	if scope.JDCategoryID, err = scopeID(values["jd_category_id"]); err != nil {
		return documentScope{}, &savedViewFilterError{message: "filter key jd_category_id must be a positive integer"}
	}
	if value, exists := values["sensitivity"]; exists && value != nil {
		scope.Sensitivity, _ = value.(string)
		if !SensitivityLevels[scope.Sensitivity] {
			return documentScope{}, &savedViewFilterError{message: "filter key sensitivity is invalid"}
		}
	}
	if scope.DocumentTypeID, err = scopeID(values["document_type__id"]); err != nil {
		return documentScope{}, &savedViewFilterError{message: "filter key document_type__id must be a positive integer"}
	}
	if scope.TagIDs, err = scopeIDs(values["tags__id__in"]); err != nil || len(scope.TagIDs) > maxDocumentScopeIDs {
		return documentScope{}, &savedViewFilterError{message: "filter key tags__id__in must contain at most 100 unique positive integers"}
	}
	if scope.CorrespondentIDs, err = scopeIDs(values["correspondents__id__in"]); err != nil || len(scope.CorrespondentIDs) > maxDocumentScopeIDs {
		return documentScope{}, &savedViewFilterError{message: "filter key correspondents__id__in must contain at most 100 unique positive integers"}
	}
	return scope, nil
}

func appendDocumentScopePredicates(where []string, args []any, scope documentScope) ([]string, []any) {
	if scope.JDCategoryID > 0 {
		where = append(where, "d.jd_category_id = ?")
		args = append(args, scope.JDCategoryID)
	}
	if len(scope.DocumentIDs) > 0 {
		where = append(where, "d.id IN ("+placeholders(len(scope.DocumentIDs))+")")
		for _, id := range scope.DocumentIDs {
			args = append(args, id)
		}
	}
	if scope.Sensitivity != "" {
		where = append(where, "d.sensitivity = ?")
		args = append(args, scope.Sensitivity)
	}
	if scope.DocumentTypeID > 0 {
		where = append(where, "d.document_type_id = ?")
		args = append(args, scope.DocumentTypeID)
	}
	if len(scope.TagIDs) > 0 {
		where = append(where, `(
			SELECT COUNT(DISTINCT dt.tag_id)
			FROM document_tags dt
			WHERE dt.document_id = d.id AND dt.tag_id IN (`+placeholders(len(scope.TagIDs))+`)
		) = ?`)

		for _, id := range scope.TagIDs {
			args = append(args, id)
		}
		args = append(args, len(scope.TagIDs))
	}
	if len(scope.CorrespondentIDs) > 0 {
		where = append(where, `(
			d.correspondent_id IN (`+placeholders(len(scope.CorrespondentIDs))+`)
			OR EXISTS (
				SELECT 1 FROM document_correspondents dc
				WHERE dc.document_id = d.id AND dc.correspondent_id IN (`+placeholders(len(scope.CorrespondentIDs))+`)
			)
		)`)
		for _, id := range scope.CorrespondentIDs {
			args = append(args, id)
		}
		for _, id := range scope.CorrespondentIDs {
			args = append(args, id)
		}
	}
	if scope.CreatedAtGTE != nil {
		where = append(where, "d.created_at >= ?")
		args = append(args, *scope.CreatedAtGTE)
	}
	if scope.CreatedAtLTE != nil {
		where = append(where, "d.created_at <= ?")
		args = append(args, *scope.CreatedAtLTE)
	}
	if scope.Language != "" {
		where = append(where, "d.languages LIKE ?")
		args = append(args, "%,"+scope.Language+",%")
	}
	return where, args
}

func normalizedScopeLanguage(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) < 2 || len(value) > 3 {
		return "", fmt.Errorf("invalid language code")
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return "", fmt.Errorf("invalid language code")
		}
	}
	return value, nil
}
func (s *Server) loadSavedViewScope(ctx context.Context, principal *pluginapi.Principal, viewID int64) (documentScope, error) {
	var filterJSON string
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT filter_json FROM saved_views
		WHERE id = ? AND (owner_id = ? OR shared = 1)
	`, viewID, principal.UserID).Scan(&filterJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return documentScope{}, errNotFound
		}
		return documentScope{}, err
	}
	return documentScopeFromSavedViewJSON(filterJSON)
}

func optionalPositiveID(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("not a positive integer")
	}
	return id, nil
}

func optionalNonNegativeInt(value string) (*int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return nil, fmt.Errorf("not a non-negative integer")
	}
	return &number, nil
}

func scopeID(value any) (int64, error) {
	if value == nil {
		return 0, nil
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case json.Number:
		raw = typed.String()
	default:
		return 0, fmt.Errorf("not an integer")
	}
	return optionalPositiveID(raw)
}

func scopeIDs(value any) ([]int64, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		if text, ok := value.(string); ok {
			return parseCSVIDs(text)
		}
		items = []any{value}
	}
	ids := make([]int64, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		id, err := scopeID(item)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("not a positive integer list")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}
