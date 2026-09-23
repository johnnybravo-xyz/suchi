// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestMobileContractFixturesMatchWireTypes keeps the checked-in mobile
// examples honest. Every key must be accepted by the response type used on
// the wire, and decoding then encoding must preserve the JSON value exactly.
// That catches stale examples which claim omitted zero values are present or
// use a field name the handler never emits.
func TestMobileContractFixturesMatchWireTypes(t *testing.T) {
	t.Parallel()

	factories := map[string]func() any{
		"document-detail.json": func() any { return &DocumentDetail{} },
		"documents-page.json":  func() any { return &Envelope[DocumentListRow]{} },
		"error.json":           func() any { return &errBody{} },
		"handshake.json":       func() any { return &handshakeResponse{} },
		"jd-categories.json":   func() any { return &Envelope[JDCategory]{} },
		"search-page.json":     func() any { return &Envelope[SearchHit]{} },
		"tasks-processing.json": func() any {
			return &TasksResponse{}
		},
		"upload-created.json":  func() any { return &UploadResponse{} },
		"upload-replayed.json": func() any { return &UploadResponse{} },
		"whoami.json":          func() any { return &UserSelf{} },
	}

	dir := filepath.Join("testdata", "mobile", "v1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			found = append(found, entry.Name())
		}
	}
	sort.Strings(found)
	if len(found) != len(factories) {
		t.Fatalf("found fixtures %v; registered %d fixture types", found, len(factories))
	}

	for _, name := range found {
		factory, ok := factories[name]
		if !ok {
			t.Errorf("fixture %q has no registered wire type", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}

			value := factory()
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(value); err != nil {
				t.Fatalf("fixture is not a valid wire response: %v", err)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("fixture has trailing JSON: %v", err)
			}

			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fixtureJSON, encodedJSON any
			if err := json.Unmarshal(raw, &fixtureJSON); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &encodedJSON); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fixtureJSON, encodedJSON) {
				t.Fatalf("fixture contains values the wire encoder omits\nfixture: %s\nencoded: %s", raw, encoded)
			}
		})
	}
}
