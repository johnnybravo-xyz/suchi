// SPDX-License-Identifier: AGPL-3.0-or-later

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/johnnybravo-xyz/suchi/core/customfield"
)

const (
	// MaxEnabledAsks is the per-system limit for classifier-backed automations.
	MaxEnabledAsks = 5
	MaxAskChoices  = 10
)

var askActionKinds = map[string]bool{
	"assign_tags":          true,
	"assign_correspondent": true,
	"assign_jd_category":   true,
	"assign_custom_field":  true,
}

// ModelQuestion is the only automation configuration sent to the classifier.
type ModelQuestion struct {
	AutomationID   int64    `json:"automation_id"`
	Question       string   `json:"question"`
	AllowedAnswers []string `json:"allowed_answers"`
}

// MatchedQuestion keeps the local action mapping beside its model-safe question.
type MatchedQuestion struct {
	Automation Automation
	Question   ModelQuestion
}

// MatchingQuestions returns enabled ask automations matching the current
// persisted document state, in automation order.
func (s *Store) MatchingQuestions(ctx context.Context, systemID, docID int64) ([]MatchedQuestion, error) {
	tx, err := s.DB.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return MatchingQuestionsTx(ctx, tx, systemID, docID)
}

// MatchingQuestionsTx is the transaction-scoped form used to reject classifier
// answers when the matching automation definitions changed during a request.
func MatchingQuestionsTx(ctx context.Context, tx *sql.Tx, systemID, docID int64) ([]MatchedQuestion, error) {
	automations, err := ListTx(ctx, tx, systemID)
	if err != nil {
		return nil, err
	}
	snapshot, err := loadSnapshotFrom(ctx, tx, docID)
	if err != nil {
		return nil, err
	}
	event := Context{DocID: docID}
	matches := make([]MatchedQuestion, 0, MaxEnabledAsks)
	for _, automation := range automations {
		if !automation.Enabled || automation.Suspended || automation.Ask == nil {
			continue
		}
		for _, trigger := range automation.Triggers {
			if trigger.Type != TriggerDocumentAdded || !matchesTrigger(trigger, snapshot, event) {
				continue
			}
			matches = append(matches, MatchedQuestion{
				Automation: automation,
				Question: ModelQuestion{
					AutomationID:   automation.ID,
					Question:       automation.Ask.Question,
					AllowedAnswers: allowedAnswers(automation.Ask.Answer),
				},
			})
			break
		}
	}
	if len(matches) > MaxEnabledAsks {
		return nil, fmt.Errorf("automations: %d matching asks exceed limit %d", len(matches), MaxEnabledAsks)
	}
	return matches, nil
}

// SameQuestionMatches reports whether classifier answers still refer to the
// same ordered questions, conditions, and local action mappings.
func SameQuestionMatches(before, after []MatchedQuestion) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].Automation.ID != after[i].Automation.ID ||
			before[i].Automation.OrderIndex != after[i].Automation.OrderIndex {
			return false
		}
		left, leftErr := signatureOf(&before[i].Automation)
		right, rightErr := signatureOf(&after[i].Automation)
		if leftErr != nil || rightErr != nil || left != right {
			return false
		}
	}
	return true
}

func allowedAnswers(answer AskAnswer) []string {
	if answer.Type == "yes_no" {
		return []string{"yes", "no", "unknown"}
	}
	out := append([]string(nil), answer.Choices...)
	return append(out, "unknown")
}

func normalizeAsk(ctx context.Context, tx *sql.Tx, systemID int64, a *Automation) error {
	if a.Ask == nil {
		for i := range a.Actions {
			if a.Actions[i].When != "" {
				return fmt.Errorf("automations: action %d: when requires ask", i+1)
			}
		}
		return nil
	}
	if len(a.Triggers) == 0 {
		return errors.New("automations: ask requires a document_added trigger")
	}
	for i, trigger := range a.Triggers {
		if normalizedTriggerType(trigger) != TriggerDocumentAdded ||
			trigger.FilterPath != "" || trigger.FilterFilename != "" ||
			trigger.FilterMailRuleID != 0 || trigger.FilterEmailFrom != "" ||
			trigger.FilterEmailSubject != "" || trigger.FilterEmailFolder != "" ||
			trigger.FilterEmailHasAttachment != nil {
			return fmt.Errorf("automations: ask trigger %d only supports persisted document_added filters", i+1)
		}
	}

	a.Ask.Question = strings.TrimSpace(a.Ask.Question)
	if n := utf8.RuneCountInString(a.Ask.Question); n < 1 || n > 1000 {
		return errors.New("automations: ask question must contain 1 to 1000 characters")
	}
	answers, err := normalizeAskAnswers(&a.Ask.Answer)
	if err != nil {
		return err
	}

	singleValues := make(map[string]bool)
	for i := range a.Actions {
		action := &a.Actions[i]
		if !askActionKinds[action.Kind] {
			return fmt.Errorf("automations: ask action %d: unsupported kind %q", i+1, action.Kind)
		}
		action.When = canonicalAnswer(strings.TrimSpace(action.When), answers)
		if action.When == "" || action.When == "unknown" {
			return fmt.Errorf("automations: ask action %d: when must name a declared non-unknown answer", i+1)
		}

		var destination string
		switch action.Kind {
		case "assign_correspondent":
			destination = "correspondent"
		case "assign_jd_category":
			destination = "jd_category"
		case "assign_custom_field":
			fieldID, err := requiredActionID(action.Params, "field_id")
			if err != nil {
				return fmt.Errorf("automations: ask action %d: %w", i+1, err)
			}
			destination = fmt.Sprintf("custom_field:%d", fieldID)
			if err := normalizeAskCustomField(ctx, tx, systemID, action, fieldID); err != nil {
				return fmt.Errorf("automations: ask action %d: %w", i+1, err)
			}
		}
		if destination != "" {
			key := action.When + "\x00" + destination
			if singleValues[key] {
				return fmt.Errorf("automations: ask answer %q assigns %s more than once", action.When, strings.ReplaceAll(destination, "_", " "))
			}
			singleValues[key] = true
		}
	}
	return nil
}

func normalizeAskAnswers(answer *AskAnswer) ([]string, error) {
	switch answer.Type {
	case "yes_no":
		if len(answer.Choices) != 0 {
			return nil, errors.New("automations: yes_no ask cannot define choices")
		}
		return []string{"yes", "no", "unknown"}, nil
	case "choice":
		if len(answer.Choices) < 2 || len(answer.Choices) > MaxAskChoices {
			return nil, fmt.Errorf("automations: choice ask requires 2 to %d choices", MaxAskChoices)
		}
		seen := make([]string, 0, len(answer.Choices))
		for i, choice := range answer.Choices {
			choice = strings.TrimSpace(choice)
			if n := utf8.RuneCountInString(choice); n < 1 || n > 40 {
				return nil, fmt.Errorf("automations: ask choice %d must contain 1 to 40 characters", i+1)
			}
			if strings.EqualFold(choice, "unknown") || canonicalAnswer(choice, seen) != "" {
				return nil, fmt.Errorf("automations: ask choice %q is reserved or duplicated", choice)
			}
			answer.Choices[i] = choice
			seen = append(seen, choice)
		}
		return append(seen, "unknown"), nil
	default:
		return nil, fmt.Errorf("automations: unsupported ask answer type %q", answer.Type)
	}
}

func canonicalAnswer(value string, allowed []string) string {
	for _, answer := range allowed {
		if strings.EqualFold(value, answer) {
			return answer
		}
	}
	return ""
}

func normalizeAskCustomField(ctx context.Context, tx *sql.Tx, systemID int64, action *Action, fieldID int64) error {
	value, ok := action.Params["value"]
	if !ok || value == nil {
		return errors.New("custom field value required")
	}
	var dataType, extra string
	if err := tx.QueryRowContext(ctx,
		`SELECT data_type, extra_data FROM custom_fields WHERE system_id = ? AND id = ?`, systemID, fieldID).
		Scan(&dataType, &extra); err != nil {
		return err
	}
	if dataType == "documentlink" {
		return errors.New("documentlink custom fields are not supported by ask")
	}
	typed, err := customfield.Lookup(dataType).Validate(json.RawMessage(extra), value)
	if err != nil {
		return fmt.Errorf("custom field value: %w", err)
	}
	switch value := typed.(type) {
	case string:
		if value == "" {
			return errors.New("custom field value cannot be empty")
		}
	case []string:
		if len(value) == 0 {
			return errors.New("custom field value cannot be empty")
		}
	}
	encoded, err := json.Marshal(typed)
	if err != nil {
		return err
	}
	if len(encoded) > 1024 {
		return errors.New("custom field value exceeds 1024 bytes")
	}
	action.Params["value"] = typed
	return nil
}

func decodeAskPatch(raw json.RawMessage) (*Ask, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var ask Ask
	if err := json.Unmarshal(raw, &ask); err != nil {
		return nil, true, fmt.Errorf("automations: invalid ask: %w", err)
	}
	return &ask, true, nil
}
