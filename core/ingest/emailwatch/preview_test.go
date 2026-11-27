// SPDX-License-Identifier: AGPL-3.0-or-later

package emailwatch

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestRecentUIDsSortsAndCaps(t *testing.T) {
	uids := make([]uint32, previewMessageLimit+3)
	for i := range uids {
		uids[i] = uint32(len(uids) - i)
	}

	got := recentUIDs(uids)
	want := make([]uint32, previewMessageLimit)
	for i := range want {
		want[i] = uint32(i + 4)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("recent UIDs = %v, want %v", got, want)
	}
}

func TestPreviewResultRecordsDecisions(t *testing.T) {
	result := PreviewResult{}
	for i := range 7 {
		result.record(
			policyDecision{content: "email_and_files"},
			PreviewSample{Subject: fmt.Sprintf("accepted-%d", i)},
		)
		result.record(
			policyDecision{excluded: true},
			PreviewSample{Subject: fmt.Sprintf("excluded-%d", i)},
		)
	}
	result.record(policyDecision{}, PreviewSample{Subject: "unmatched-1"})
	result.record(policyDecision{}, PreviewSample{Subject: "unmatched-2"})

	if result.Inspected != 16 || result.Accepted != 7 || result.Excluded != 7 {
		t.Fatalf("preview counts=%+v, want inspected=16 accepted=7 excluded=7", result)
	}
	if len(result.AcceptedSamples) != previewSampleLimit ||
		result.AcceptedSamples[previewSampleLimit-1].Subject != "accepted-4" {
		t.Fatalf("accepted samples=%+v, want first five accepted messages", result.AcceptedSamples)
	}
	if len(result.ExcludedSamples) != previewSampleLimit ||
		result.ExcludedSamples[previewSampleLimit-1].Subject != "excluded-4" {
		t.Fatalf("excluded samples=%+v, want first five excluded messages", result.ExcludedSamples)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"inspected", "accepted", "excluded", "accepted_samples", "excluded_samples"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("preview response missing %q: %s", key, raw)
		}
	}
	for _, obsolete := range []string{"matched", "samples"} {
		if _, ok := decoded[obsolete]; ok {
			t.Fatalf("preview response retained %q alias: %s", obsolete, raw)
		}
	}
}
