package api

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testPositiveIDs(count int) []int64 {
	ids := make([]int64, count)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

func testCSVRange(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = strconv.Itoa(i + 1)
	}
	return strings.Join(parts, ",")
}

func testRepeatedCSV(id, count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

func TestNormalizedPositiveIDs(t *testing.T) {
	got, err := normalizedPositiveIDs([]int64{3, 1, 3, 2}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v, want %v", got, want)
	}
	if _, err := normalizedPositiveIDs([]int64{1, 0}, 2); err == nil {
		t.Fatal("non-positive id was accepted")
	}
	if _, err := normalizedPositiveIDs([]int64{1, 1, 1}, 2); err == nil {
		t.Fatal("oversized input was accepted before deduplication")
	}
}

func TestParseBoundedCSVIDs(t *testing.T) {
	got, err := parseBoundedCSVIDs("3,1,3,2", 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{3, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids=%v, want %v", got, want)
	}
	if _, err := parseBoundedCSVIDs("1,1,1,1,1", 4); err == nil {
		t.Fatal("raw token limit was applied after deduplication")
	}
}
