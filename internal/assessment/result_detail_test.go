package assessment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

type detailFixture struct {
	*fixture
	full     sdk.InteractionRecord
	reads    int
	corrupt  bool
	denyRead error
}

func (f *detailFixture) Query(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
	p, err := f.fixture.Query(q)
	if err != nil {
		return p, err
	}
	for i, r := range p.Rows {
		if r.ID == f.full.ID {
			raw, _ := json.Marshal(f.full)
			r.Content = ""
			r.Annotations = nil
			r.Deliveries = nil
			r.ContentRef = &sdk.InteractionContentRef{ID: r.ID, Source: "recorded", Incarnation: p.Incarnation, SHA256: Hash(raw), Bytes: len(raw)}
			p.Rows[i] = r
		}
	}
	return p, nil
}
func (f *detailFixture) Read(q sdk.InteractionReadRequest) ([]byte, bool, int64, error) {
	f.reads++
	if f.denyRead != nil {
		return nil, false, 0, f.denyRead
	}
	raw, _ := json.Marshal(f.full)
	if q.ID != f.full.ID || q.Incarnation != "incarnation" || q.SHA256 != Hash(raw) || q.Offset < 0 || q.Offset >= int64(len(raw)) {
		return nil, false, 0, errors.New("unexpected detail request")
	}
	end := min(int(q.Offset)+q.Length, len(raw))
	b := append([]byte(nil), raw[q.Offset:end]...)
	if f.corrupt {
		b[0] ^= 1
	}
	return b, end == len(raw), int64(len(raw)), nil
}

func reviewRef(r sdk.InteractionRecord) Ref {
	ref := RecordRef(r)
	ref.Incarnation = "incarnation"
	return ref
}

func detailRequest(r sdk.InteractionRecord) ReviewRequest {
	q := DefaultReviewRequest(r.Details.WorkSession)
	ref := RecordRef(r)
	q.ResultRef = &ref
	q.SourceIncarnation = "incarnation"
	return q
}
func TestReviewExactResultKeepsRecordAndUsesTransportHash(t *testing.T) {
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = "→ shell(" + strings.Repeat("argument", 80) + ")\n← prerequisite missing\n[exit status 1]"
	r.Annotations = map[string]json.RawMessage{"note": json.RawMessage(`{"text":"mutable note"}`)}
	f.rows[5] = r
	reader := &detailFixture{fixture: f, full: r}
	q := detailRequest(r)
	for _, annotation := range []string{`{"text":"first"}`, `{"text":"second"}`} {
		reader.full.Annotations["note"] = json.RawMessage(annotation)
		got, err := Review(reader, q)
		if err != nil {
			t.Fatal(err)
		}
		d := got.ResultDetail
		if d == nil || d.Ref != reviewRef(r) || d.SourceIncarnation != q.SourceIncarnation || d.Content != r.Content || d.ContentTruncated || d.Outcome != "failed" || d.WorkSession != "ws" || len(d.Omissions) != 1 || d.Omissions[0] != "retained_transcript_may_omit_original_output" {
			t.Fatalf("%+v", d)
		}
		route := got.Attempts[0].ResultDetailRequest
		if route == nil || route.WorkSession != "ws" || route.SourceIncarnation != q.SourceIncarnation || route.ResultRef != reviewRef(r) {
			t.Fatal(route)
		}
		if !got.Attempts[0].ResultExcerptTruncated || reader.reads == 0 || f.fileReads != 0 {
			t.Fatal("detail did not use bounded read-only owner")
		}
	}
}
func TestReviewExactResultRefusalsAndReadLimits(t *testing.T) {
	for _, name := range []string{"hash", "sequence", "incarnation", "missing", "wrong kind", "wrong session", "wrong project", "excluded", "read budget", "detail budget", "denied query", "denied read", "corrupt detail", "changed source"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(sample("failed"), 1)
			r := f.rows[5]
			r.Content = "retained failure"
			f.rows[5] = r
			reader := &detailFixture{fixture: f, full: r}
			q := detailRequest(r)
			want := ErrResultUnavailable
			denied := &sdk.Denied{Code: -32000, ReasonCode: "POLICY_DENY"}
			switch name {
			case "hash":
				q.ResultRef.SHA256 = Hash([]byte("different"))
				want = ErrConflict
			case "sequence":
				q.ResultRef.Sequence++
				want = ErrConflict
			case "incarnation":
				q.SourceIncarnation = "old"
				want = ErrSourceChanged
			case "missing":
				q.ResultRef.ID = "absent"
			case "wrong kind":
				ref := RecordRef(f.rows[2])
				q.ResultRef = &ref
			case "wrong session":
				q.WorkSession = "other"
			case "wrong project":
				reader.full.ProjectID = "other"
				f.rows[5] = reader.full
				q = detailRequest(reader.full)
			case "excluded":
				q.ExcludeCallID = f.rows[2].ID
			case "read budget":
				q.ReadLimit = 2
			case "detail budget":
				q.DetailLimit = 1
			case "denied query":
				f.denial = denied
				want = denied
			case "denied read":
				reader.denyRead = denied
				want = denied
			case "corrupt detail":
				reader.corrupt = true
				want = ErrSourceChanged
			case "changed source":
				f.changeAt = 2
				want = ErrSourceChanged
			}
			if _, err := Review(reader, q); !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if reader.reads > q.ReadLimit || f.queries+reader.reads > q.ReadLimit {
				t.Fatal("exceeded read limit")
			}
			if name == "detail budget" && reader.reads != 0 {
				t.Fatal("read outside detail budget")
			}
		})
	}
}
func TestReviewResultDetailLimitsAndRefusedOutcome(t *testing.T) {
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = strings.Repeat("界", MaxResultDetailRunes+1)
	r.Outcome = sdk.InteractionRefused
	r.Details.Truncated = true
	f.rows[5] = r
	q := detailRequest(r)
	got, err := Review(f, q)
	if err != nil {
		t.Fatal(err)
	}
	d := got.ResultDetail
	if d == nil || len([]rune(d.Content)) != MaxResultDetailRunes || !d.ContentTruncated || !d.ExecutionTruncated || d.Outcome != "refused" || strings.Join(d.Omissions, ",") != "retained_transcript_may_omit_original_output,result_detail_content_limit,tool_execution_truncated" {
		t.Fatalf("%+v", d)
	}
	if raw, _ := json.Marshal(got); len(raw) > MaxReviewBytes {
		t.Fatal("oversized detail response")
	}

	r.Content = strings.Repeat("<", MaxResultDetailRunes)
	f.rows[5] = r
	q = detailRequest(r)
	for i := uint64(7); i < 16; i++ {
		v := f.rows[1]
		v.ID = fmt.Sprint("notice-", i)
		v.Sequence = i
		text := strings.Repeat("界", 300)
		v.Details.Work = &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Result: &text, EvidenceReadback: &text}
		f.rows = append(f.rows, v)
	}
	if got, err := Review(f, q); err != nil {
		t.Fatal(err)
	} else if raw, _ := json.Marshal(got); len(raw) > MaxReviewBytes {
		t.Fatal("lost whole-response budget")
	}
}
func TestReviewResultSelectionValidationBeforeRead(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	base := detailRequest(f.rows[5])
	for _, name := range []string{"missing incarnation", "missing ref", "empty id", "zero sequence", "bad hash", "oversized incarnation", "foreign reference incarnation"} {
		q := base
		ref := *base.ResultRef
		q.ResultRef = &ref
		switch name {
		case "missing incarnation":
			q.SourceIncarnation = ""
		case "missing ref":
			q.ResultRef = nil
		case "empty id":
			q.ResultRef.ID = ""
		case "zero sequence":
			q.ResultRef.Sequence = 0
		case "bad hash":
			q.ResultRef.SHA256 = "bad"
		case "oversized incarnation":
			q.SourceIncarnation = strings.Repeat("x", 257)
		case "foreign reference incarnation":
			q.ResultRef.Incarnation = "replacement"
		}
		if _, err := Review(f, q); !errors.Is(err, ErrInvalid) || f.queries != 0 {
			t.Fatal(name, err, f.queries)
		}
	}
}

func TestReviewDetailChunkBudgetAndStableReference(t *testing.T) {
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = "exact retained failure"
	annotation, _ := json.Marshal(strings.Repeat("mutable", 6000))
	r.Annotations = map[string]json.RawMessage{"note": annotation}
	f.rows[5] = r
	reader := &detailFixture{fixture: f, full: r}
	q := detailRequest(r)
	q.ReadLimit = 3
	if _, err := Review(reader, q); !errors.Is(err, ErrResultUnavailable) || reader.reads != 1 {
		t.Fatal("partial record was exposed", err, reader.reads)
	}
	q.ReadLimit = 8
	got, err := Review(reader, q)
	if err != nil || got.ResultDetail == nil || got.ResultDetail.Content != r.Content || got.Coverage.DetailBytes <= sdk.InteractionReadLimit {
		t.Fatal("bounded chunks did not recover exact retained content", got, err)
	}
}

func TestReviewExactDetailAvoidsAggregateBudgetCycle(t *testing.T) {
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = strings.Repeat("<", MaxResultDetailRunes)
	f.rows[5] = r
	for i := uint64(7); i < 16; i++ {
		v := f.rows[1]
		v.ID = fmt.Sprint("large-work-", i)
		v.Sequence = i
		text := strings.Repeat("界", 300)
		v.Details.Work = &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Result: &text, EvidenceReadback: &text}
		f.rows = append(f.rows, v)
	}
	first, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil {
		t.Fatal("initial compact review did not fit", err)
	}
	if len(first.Attempts) != 1 || first.Attempts[0].ResultDetailRequest == nil {
		t.Fatal("initial review lost target")
	}
	q := detailRequest(r)
	_, fullErr := Review(f, q)
	q.RecordLimit = 8
	_, smallErr := Review(f, q)
	t.Logf("aggregate detail=%v; lowering record_limit=8=%v", fullErr, smallErr)
	q.RecordLimit = 128
	got, err := Review(f, q)
	if err != nil {
		t.Fatal("an exact detail must not require lowering away its target", err)
	}
	if got.ResultDetail == nil || got.ResultDetail.Content != r.Content {
		t.Fatal("exact retained record missing")
	}
	raw, _ := json.Marshal(got)
	if len(raw) > MaxReviewBytes {
		t.Fatal("exact detail exceeded response ceiling")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 6 || wire["result_detail"] == nil || wire["coverage"] == nil || wire["counts"] != nil || wire["attempts"] != nil || got.Scope != "retained_result_detail" {
		t.Fatal("detail repeated aggregate payload", string(raw))
	}
}

func TestAResultDetailRequestKeepsTheReviewsBounds(t *testing.T) {
	f := newFixture(sample("failed"), 1)
	r := f.rows[5]
	r.Content = "→ shell(" + strings.Repeat("argument", 80) + ")\n← prerequisite missing\n[exit status 1]"
	f.rows[5] = r
	for i := 0; i < 200; i++ {
		f.rows = append(f.rows, sdk.InteractionRecord{ID: fmt.Sprintf("later-%d", i), Sequence: uint64(7 + i), Kind: sdk.InteractionNotice, ProjectID: "project", TurnID: "turn", Details: sdk.InteractionDetails{WorkSession: "ws"}})
	}
	reader := &detailFixture{fixture: f, full: r}
	q := DefaultReviewRequest("ws")
	q.ReadLimit, q.RecordLimit = 128, 512
	got, err := Review(reader, q)
	if err != nil {
		t.Fatal(err)
	}
	var route *ResultSelection
	for _, a := range got.Attempts {
		if a.ResultDetailRequest != nil {
			route = a.ResultDetailRequest
		}
	}
	if route == nil || route.ReadLimit != 128 || route.RecordLimit != 512 || route.DetailLimit != q.DetailLimit {
		t.Fatalf("the offered request must carry the review's bounds: %+v", route)
	}
	raw, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	copied := DefaultReviewRequest("ws")
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	detail, err := Review(reader, copied)
	if err != nil || detail.ResultDetail == nil || detail.ResultDetail.Content != r.Content {
		t.Fatalf("the copied request must return the result the review found: %v, %+v", err, detail.ResultDetail)
	}
}
