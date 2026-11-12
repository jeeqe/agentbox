package ui

import "testing"

func TestParseDiffNumbersLinesFromHunkHeaders(t *testing.T) {
	patch := "diff --git a/internal/worker/worker.go b/internal/worker/worker.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/internal/worker/worker.go\n" +
		"+++ b/internal/worker/worker.go\n" +
		"@@ -84,3 +84,4 @@ func (q *Queue) Push(j Job) {\n" +
		" func (q *Queue) Push(j Job) {\n" +
		"-\tq.items = append(q.items, j)\n" +
		"+\tq.mu.Lock()\n" +
		"+\tq.items = append(q.items, j)\n" +
		" }\n"

	got := parseDiff(patch)
	want := []diffLine{
		{Kind: "file", Text: "internal/worker/worker.go"},
		{Kind: "hunk", Text: "@@ -84,3 +84,4 @@ func (q *Queue) Push(j Job) {"},
		{Kind: "ctx", Text: "func (q *Queue) Push(j Job) {", Old: 84, New: 84},
		{Kind: "del", Text: "\tq.items = append(q.items, j)", Old: 85},
		{Kind: "add", Text: "\tq.mu.Lock()", New: 85},
		{Kind: "add", Text: "\tq.items = append(q.items, j)", New: 86},
		{Kind: "ctx", Text: "}", Old: 86, New: 87},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDiffKeepsContentThatLooksLikeMetadata(t *testing.T) {
	// A removed line starting with "-- " or an added "++ " is content, not a header.
	patch := "diff --git a/a.sql b/a.sql\n@@ -1,2 +1,2 @@\n--- old comment\n+++ new comment\n ok\n"
	got := parseDiff(patch)
	if len(got) != 5 || got[2].Kind != "del" || got[3].Kind != "add" || got[4].Kind != "ctx" {
		t.Fatalf("rows = %+v, want file, hunk, del, add, ctx", got)
	}
}

func TestParseDiffEmpty(t *testing.T) {
	if rows := parseDiff("  \n"); rows != nil {
		t.Fatalf("rows = %+v, want nil", rows)
	}
}
