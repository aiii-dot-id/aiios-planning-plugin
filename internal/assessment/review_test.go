package assessment

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func TestReviewNeedsNoPlanRegistrationOrCapture(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	got, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != "identity" || !got.Coverage.Complete || got.Counts == nil || got.Counts.Admissions != 1 || got.Counts.Succeeded != 1 || len(got.WorkChanges) != 4 {
		t.Fatalf("%+v", got)
	}
	if got.Coverage.ReadCalls != 2 || f.fileReads != 0 {
		t.Fatalf("redundant reads: %+v", got.Coverage)
	}
	if got.Attempts[0].Tool != "shell" || got.WorkChanges[2].Change.Result == nil {
		t.Fatal(got)
	}
}

func TestReviewValidationDiagnosticsKeepTheOriginalBoundaries(t *testing.T) {
	validRef := Ref{ID: "record", Sequence: 1, SHA256: strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name, want string
		change     func(*ReviewRequest)
	}{
		{"view", "view must be", func(q *ReviewRequest) { q.View = "PRIVATE_ARGUMENT_CONTENT" }},
		{"session", "work_session_id must name", func(q *ReviewRequest) { q.WorkSession = "../PRIVATE_ARGUMENT_CONTENT/" }},
		{"read limit", "read_limit must be an integer from 1 to 128", func(q *ReviewRequest) { q.ReadLimit = 129 }},
		{"record limit", "record_limit must be an integer from 1 to 512", func(q *ReviewRequest) { q.RecordLimit = 513 }},
		{"detail limit", "detail_limit must be an integer from 1 to 1048576", func(q *ReviewRequest) { q.DetailLimit = 1<<20 + 1 }},
		{"source length", "source_incarnation must be", func(q *ReviewRequest) { q.SourceIncarnation = strings.Repeat("a", 257) }},
		{"missing source", "source_incarnation must be", func(q *ReviewRequest) { q.ResultRef = &validRef }},
		{"source without reference", "source_incarnation must be", func(q *ReviewRequest) { q.SourceIncarnation = "PRIVATE_ARGUMENT_CONTENT" }},
		{"reference conflict", "mutually exclusive", func(q *ReviewRequest) {
			q.SourceIncarnation = "source"
			q.ResultRef, q.WorkRef, q.WorkField = &validRef, &validRef, "state"
		}},
		{"missing work field", "must be supplied together", func(q *ReviewRequest) { q.SourceIncarnation = "source"; q.WorkRef = &validRef }},
		{"missing work reference", "must be supplied together", func(q *ReviewRequest) { q.WorkField = "state" }},
		{"unknown work field", "work_field must be", func(q *ReviewRequest) {
			q.SourceIncarnation = "source"
			q.WorkRef, q.WorkField = &validRef, "PRIVATE_ARGUMENT_CONTENT"
		}},
		{"reference ID", "non-empty string of at most 256 bytes", func(q *ReviewRequest) {
			q.SourceIncarnation = "source"
			q.ResultRef = &Ref{Sequence: 1, SHA256: validRef.SHA256}
		}},
		{"reference sequence", "positive decimal string", func(q *ReviewRequest) {
			q.SourceIncarnation = "source"
			q.ResultRef = &Ref{ID: "PRIVATE_ARGUMENT_CONTENT", SHA256: validRef.SHA256}
		}},
		{"reference hash", "64 lowercase hexadecimal", func(q *ReviewRequest) {
			q.SourceIncarnation = "source"
			q.WorkRef, q.WorkField = &Ref{ID: "record", Sequence: 1, SHA256: strings.Repeat("A", 64)}, "state"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := DefaultReviewRequest("ws")
			tc.change(&q)
			f := newFixture(sample("passed"), 0)
			_, err := Review(f, q)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `Example with an acting work session: {"view":"compact"}`) || strings.Contains(err.Error(), "PRIVATE_ARGUMENT_CONTENT") {
				t.Fatalf("validation lost the fixed requirement, example or original classification: %v", err)
			}
			if f.queries != 0 {
				t.Fatal("an invalid review read the recorded source", f.queries)
			}
		})
	}
	for _, q := range []ReviewRequest{
		{WorkSession: "ws", ReadLimit: 1, RecordLimit: 1, DetailLimit: 1},
		{WorkSession: strings.Repeat("a", 128), ReadLimit: 128, RecordLimit: 512, DetailLimit: 1 << 20},
		{WorkSession: "ws", View: "full", ReadLimit: 1, RecordLimit: 1, DetailLimit: 1, ResultRef: &validRef, SourceIncarnation: strings.Repeat("a", 256)},
	} {
		if err := q.Validate(); err != nil {
			t.Fatal("diagnostics changed a valid boundary", q, err)
		}
	}
}

func TestReviewUnknownIsNotZero(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	got, err := Review(f, DefaultReviewRequest("absent"))
	if err != nil || got.Counts != nil || got.Coverage.Complete {
		t.Fatalf("%+v %v", got, err)
	}
	f = newFixture(sample("passed"), 0)
	f.rows = f.rows[:5]
	got, err = Review(f, DefaultReviewRequest("ws"))
	if err != nil || got.Counts == nil || got.Counts.Unknown != 1 || got.Counts.Admissions != 1 || got.Coverage.Complete {
		t.Fatalf("%+v %v", got, err)
	}
	f = newFixture(sample("passed"), 0)
	f.rows = append(f.rows[:2], f.rows[3:5]...)
	got, err = Review(f, DefaultReviewRequest("ws"))
	if err != nil || got.Counts == nil || got.Counts.Admissions != 0 || !got.Coverage.Complete {
		t.Fatalf("known zero: %+v %v", got, err)
	}
}

func TestReviewRecordedOutcomesAndReportedClaims(t *testing.T) {
	for _, status := range []sdk.InteractionOutcome{sdk.InteractionFailed, sdk.InteractionRefused, sdk.InteractionCancelled, sdk.InteractionUnknown} {
		f := newFixture(sample("passed"), 0)
		f.rows[5].Outcome = status
		f.rows[3].Details.Work.Result = ptr("served: resident says the task passed")
		got, err := Review(f, DefaultReviewRequest("ws"))
		if err != nil || got.Attempts[0].Outcome != string(status) || got.Counts.Succeeded != 0 {
			t.Fatalf("%+v %v", got, err)
		}
		if *got.WorkChanges[2].Change.Result != "served: resident says the task passed" {
			t.Fatal("lost report")
		}
	}
}

func TestReviewBoundedExcerptsAndCounts(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.rows[1].Details.Work.Plan = ptr(strings.Repeat("界", 300))
	got, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil {
		t.Fatal(err)
	}
	w := got.WorkChanges[1]
	if len([]rune(*w.Change.Plan)) != 256 || len(w.TruncatedFields) != 1 || w.TruncatedFields[0] != "plan" || w.Ref != RecordRef(f.rows[1]) {
		t.Fatal(w)
	}
	if len([]rune(*f.rows[1].Details.Work.Plan)) != 300 {
		t.Fatal("mutated source")
	}
	q := DefaultReviewRequest("ws")
	q.RecordLimit = 2
	got, err = Review(f, q)
	if err != nil || got.Coverage.Complete || got.Coverage.Records != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, w := range got.WorkChanges {
		if w.Ref.Sequence < 5 {
			t.Fatal("budget retained older page prefix", w)
		}
	}
}

func TestReviewRejectsBadRequestsBeforeReads(t *testing.T) {
	for _, q := range []ReviewRequest{{}, DefaultReviewRequest("../ws"), {WorkSession: "ws", ReadLimit: 129, RecordLimit: 1, DetailLimit: 1}} {
		f := newFixture(sample("passed"), 0)
		if _, err := Review(f, q); !errors.Is(err, ErrInvalid) || f.queries != 0 {
			t.Fatal(err, f.queries)
		}
	}
	f := newFixture(sample("passed"), 0)
	denied := &sdk.Denied{Code: -32000, ReasonCode: "POLICY_DENY"}
	f.denial = denied
	if _, err := Review(f, DefaultReviewRequest("ws")); err != denied {
		t.Fatal(err)
	}
}

func TestReviewOrphanResultsAndResponseBound(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.rows = append(f.rows[:2], f.rows[3:]...)
	got, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil || got.Coverage.Complete || len(got.UnmatchedResults) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	f = newFixture(sample("passed"), 0)
	for i := uint64(7); i < 37; i++ {
		r := f.rows[1]
		r.ID = fmt.Sprint("notice", i)
		r.Sequence = i
		text := strings.Repeat("界", 300)
		r.Details.Work = &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Result: &text, EvidenceReadback: &text}
		f.rows = append(f.rows, r)
	}
	full := DefaultReviewRequest("ws")
	full.View = "full"
	if _, err := Review(f, full); !errors.Is(err, ErrReviewTooLarge) {
		t.Fatal(err)
	}
}

func TestReviewDescendantsOwnCallAndEvidence(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.descendants = map[string]bool{"child": true, "grandchild": true}
	for i, session := range []string{"child", "grandchild", "unrelated"} {
		call := f.rows[2]
		call.ID = fmt.Sprint("child-call-", i)
		call.Sequence = uint64(7 + i*2)
		call.TurnID = fmt.Sprint("turn-", i)
		call.Source = &sdk.InteractionSource{Kind: "tool", ID: fmt.Sprint("execution-", i)}
		call.Details.WorkSession = session
		result := f.rows[5]
		result.ID = fmt.Sprint("child-result-", i)
		result.Sequence = call.Sequence + 1
		result.RelatedID = call.ID
		result.TurnID = call.TurnID
		result.Source = call.Source
		result.Details.WorkSession = session
		result.Content = "returned evidence"
		result.Outcome = sdk.InteractionFailed
		f.rows = append(f.rows, call, result)
	}
	own := f.rows[2]
	own.ID = "own-review"
	own.Sequence = 20
	own.Source = &sdk.InteractionSource{Kind: "tool", ID: "own-execution"}
	own.Details.Tool = "review"
	f.rows = append(f.rows, own)
	q := DefaultReviewRequest("ws")
	q.IncludeDescendants = true
	q.ExcludeCallID = own.ID
	got, err := Review(f, q)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Coverage.Complete || got.Counts.RecordedRequests != 3 || got.Counts.Failed != 2 || got.Counts.Unknown != 0 || got.OwnCallExclusion != "host_record_excluded" || got.ExcludedCallID != own.ID {
		t.Fatalf("%+v", got)
	}
	if got.Scope != "retained_session_and_proven_descendants" || got.CountBasis != "retained_recorded_requests" {
		t.Fatal(got)
	}
	for _, a := range got.Attempts {
		if a.ID == own.ID || a.WorkSession == "unrelated" || a.RequestRef == nil || a.ResultRef == nil {
			t.Fatal(a)
		}
	}
	for _, e := range got.Evidence {
		if e.ID == own.ID || e.Incarnation != "incarnation" {
			t.Fatal(e)
		}
	}
	if got.Attempts[1].ResultExcerpt != "returned evidence" {
		t.Fatal(got.Attempts)
	}

	q.ExcludeCallID = ""
	got, err = Review(f, q)
	if err != nil || got.OwnCallExclusion != "unavailable" || got.Counts.RecordedRequests != 4 || got.Counts.Unknown != 1 || got.Coverage.Complete {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCurrentWorkUsesExistingFieldHistoryAndClears(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.rows[0].Details.Work.Focus = ptr("existing objective")
	f.rows[1].Details.Work.ExpectedEvidence = ptr("actual check output")
	f.rows[4].Details.Work.Focus = ptr("")
	f.rows[4].Details.Work.Plan = ptr(strings.Repeat("界", 300))
	review, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil {
		t.Fatal(err)
	}
	got := CurrentWork(review)
	if got.Availability != "retained" || *got.Fields.Focus != "" || *got.Fields.ExpectedEvidence != "actual check output" || len([]rune(*got.Fields.Plan)) != 256 || got.FieldRefs["focus"] != RecordRef(f.rows[4]) || len(got.TruncatedFields) != 1 {
		t.Fatalf("%+v", got)
	}
	missing := CurrentWork(ReviewResult{WorkSession: "missing", WorkChanges: review.WorkChanges})
	if missing.Availability != "not_retained" || len(missing.FieldRefs) != 0 {
		t.Fatal(missing)
	}
}

func TestReviewDoesNotAttachMisattributedResultContent(t *testing.T) {
	for _, mismatch := range []string{"session", "project"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newFixture(sample("passed"), 0)
			f.rows[5].Content = "unrelated result detail"
			f.rows[5].Details.ReasonCode = "UNRELATED_REASON"
			if mismatch == "session" {
				f.rows[5].Details.WorkSession = "other"
			} else {
				f.rows[5].ProjectID = "other"
			}
			got, err := Review(f, DefaultReviewRequest("ws"))
			if err != nil {
				t.Fatal(err)
			}
			if got.Counts.Unknown != 1 || got.Coverage.Complete {
				t.Fatal(got)
			}
			a := got.Attempts[0]
			if a.ResultExcerpt != "" || a.ResultRef != nil || a.ResultRecord != "" || a.DurationMS != nil || a.ReasonCode != "" {
				t.Fatal("attached unproved result attribution", a)
			}
		})
	}
}
