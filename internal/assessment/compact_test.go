package assessment

import (
	"encoding/json"
	"errors"
	"fmt"
	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"reflect"
	"strings"
	"testing"
)

func readTrace(n int) *fixture {
	f := newFixture(sample("passed"), 0)
	f.rows = f.rows[:1]
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("request-%02d-%s", i, strings.Repeat("a", 64))
		source := &sdk.InteractionSource{Kind: "tool", ID: fmt.Sprint("execution-", i)}
		details := sdk.InteractionDetails{WorkSession: "ws", Tool: "read"}
		f.rows = append(f.rows, sdk.InteractionRecord{ID: id, Sequence: uint64(2*i + 2), Kind: sdk.InteractionToolCall, ProjectID: "project", TurnID: "turn", Source: source, Details: details})
		details.ResultRecord = "[900 chars, sha256=" + strings.Repeat("b", 64) + "]"
		f.rows = append(f.rows, sdk.InteractionRecord{ID: "result-" + id, RelatedID: id, Sequence: uint64(2*i + 3), Kind: sdk.InteractionToolResult, ProjectID: "project", TurnID: "turn", Source: source, Details: details, Outcome: sdk.InteractionSucceeded, Content: strings.Repeat("observed source line\n", 45)})
	}
	return f
}

func TestCompactFourteenReadsPreservesUnknownsAndFullRoute(t *testing.T) {
	f := readTrace(14)
	q := DefaultReviewRequest("ws")
	got, err := Review(f, q)
	if err != nil {
		t.Fatal(err)
	}
	compact := got.Compact()
	if compact.Counts.RecordedRequests != 14 || compact.Counts.Succeeded != 14 || len(compact.Attempts) != CompactAttemptLimit || compact.Omitted.Attempts != 14-CompactAttemptLimit || !strings.Contains(compact.Correctness, "unknown") {
		t.Fatal(compact)
	}
	for i, a := range compact.Attempts {
		if a.ID != got.Attempts[13-i].ID || a.ResultDetailRequest == nil || a.RequestRef.Incarnation != got.SourceIncarnation || a.ResultRef.Incarnation != got.SourceIncarnation {
			t.Fatal("what worked is not shown newest first with citable references", a)
		}
	}
	if compact.FullReviewRequest.View != "full" || compact.FullReviewRequest.WorkSession != "ws" || compact.FullReviewRequest.RecordLimit != q.RecordLimit {
		t.Fatal(compact.FullReviewRequest)
	}
	full, err := Review(readTrace(14), compact.FullReviewRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Attempts, got.Attempts) || !reflect.DeepEqual(full.Evidence, got.Evidence) || !reflect.DeepEqual(full.Coverage, got.Coverage) || !reflect.DeepEqual(full.Counts, got.Counts) {
		t.Fatal("full route changed collected facts")
	}
	small, _ := json.Marshal(got)
	large, _ := json.Marshal(full)
	if len(small)*2 >= len(large) {
		t.Fatal("compact repeated every successful attempt", len(small), len(large))
	}
	t.Logf("SYNTHETIC_14_READS full=%d compact=%d reduction=%.2f%%", len(large), len(small), 100*(1-float64(len(small))/float64(len(large))))
	if len(got.Attempts) != 14 || len(got.Evidence) != 29 {
		t.Fatal("projection mutated full review")
	}
}

func TestCompactRecentFailuresKeepExactReferencesAndDiagnosticTails(t *testing.T) {
	f := readTrace(9)
	for i := 0; i < 7; i++ {
		r := &f.rows[2*i+2]
		r.Outcome = sdk.InteractionRefused
		r.Details.ReasonCode = "ARGUMENTS_REQUIRED"
		r.Content = strings.Repeat("echoed request ", 100) + "missing required command for shell"
	}

	f.rows[18].Content = "command failed with exit status 7"
	q := DefaultReviewRequest("ws")
	got, err := Review(f, q)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Compact()
	if c.Counts.RecordedRequests != 9 || c.Counts.Refused != 7 || c.Counts.Succeeded != 2 || len(c.Attempts) != 7 || c.Omitted.Attempts != 2 {
		t.Fatal(c)
	}
	if c.Attempts[0].ID != got.Attempts[8].ID || c.Attempts[1].ID != got.Attempts[7].ID || c.Attempts[0].Outcome != "succeeded" || c.Attempts[0].ResultDetailRequest == nil {
		t.Fatal("the attempts that worked are not shown", c.Attempts[:2])
	}
	for i, a := range c.Attempts[2:] {
		original := got.Attempts[6-i]
		if a.ID != original.ID || a.RequestRef == nil || a.ResultRef == nil || *a.ResultRef != *original.ResultRef || a.ReasonCode != "ARGUMENTS_REQUIRED" || !strings.HasSuffix(a.ResultExcerpt, "missing required command for shell") {
			t.Fatal(a)
		}
		route := a.ResultDetailRequest
		if route == nil || route.SourceIncarnation != got.SourceIncarnation || route.WorkSession != a.WorkSession || route.ResultRef != *a.ResultRef {
			t.Fatal(route)
		}
		d := DefaultReviewRequest(route.WorkSession)
		d.SourceIncarnation = route.SourceIncarnation
		d.ResultRef = &route.ResultRef
		detail, err := Review(f, d)
		if err != nil || detail.ResultDetail.Ref != route.ResultRef || !strings.HasSuffix(detail.ResultDetail.Content, "missing required command for shell") {
			t.Fatal(detail, err)
		}
	}
	for _, a := range c.Attempts {
		for _, e := range []Ref{*a.RequestRef, *a.ResultRef, a.ResultDetailRequest.ResultRef} {
			if e.Incarnation != got.SourceIncarnation {
				t.Fatal("a reference cannot be copied into Memory evidence", e)
			}
		}
	}
	if got.Attempts[6].ResultDetailRequest == nil || !got.Attempts[6].ResultExcerptTruncated {
		t.Fatal("lost full excerpt route")
	}

	absent, err := Review(f, DefaultReviewRequest("absent"))
	if err != nil || absent.Compact().Counts != nil || absent.Compact().Coverage.Complete {
		t.Fatal(absent, err)
	}
	f.rows = f.rows[:len(f.rows)-1]
	missing, err := Review(f, q)
	if err != nil || missing.Counts.Unknown != 1 || missing.Coverage.Complete || missing.Compact().Attempts[0].Outcome != "unknown" {
		t.Fatal(missing, err)
	}
	route := c.Attempts[0].ResultDetailRequest
	d := DefaultReviewRequest("other")
	d.SourceIncarnation = route.SourceIncarnation
	d.ResultRef = &route.ResultRef
	if _, err = Review(f, d); !errors.Is(err, ErrResultUnavailable) {
		t.Fatal("unavailable evidence was guessed", err)
	}
}

func TestCompactFoldsWorkWithoutHidingCoverageOrMutatingFullHistory(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	q := DefaultReviewRequest("ws")
	q.RecordLimit = 2
	got, err := Review(f, q)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Compact()
	if c.Coverage.Complete || !reflect.DeepEqual(c.Coverage, got.Coverage) || c.Omitted.WorkChanges != len(got.WorkChanges) || !reflect.DeepEqual(c.CurrentWork, CurrentWork(got)) {
		t.Fatal(c)
	}
	for _, view := range []string{"all", "none", "FULL"} {
		q.View = view
		if _, err = Review(f, q); !errors.Is(err, ErrInvalid) {
			t.Fatal(view, err)
		}
	}
}
