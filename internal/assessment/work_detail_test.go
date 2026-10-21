package assessment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func workRequest(r sdk.InteractionRecord, field string) ReviewRequest {
	q := DefaultReviewRequest("ws")
	ref := RecordRef(r)
	q.WorkRef, q.WorkField, q.SourceIncarnation = &ref, field, "incarnation"
	return q
}

func TestWorkDetailRoutesRecoverEveryClippedField(t *testing.T) {
	for _, name := range []string{"state", "focus", "next_move", "plan", "expected_evidence", "falsifier", "decision_needed", "result", "evidence", "evidence_readback"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(sample("passed"), 0)
			r := f.rows[1]
			r.Details.Work = &sdk.InteractionWorkChange{Session: "ws"}
			text := strings.Repeat("界", 300) + " exact tail " + name
			*workField(r.Details.Work, name) = &text
			f.rows = []sdk.InteractionRecord{r}
			q := DefaultReviewRequest("ws")
			q.View, q.IncludeDescendants = "full", true
			q.ReadLimit, q.RecordLimit, q.DetailLimit = 64, 256, 512<<10
			first, err := Review(f, q)
			if err != nil || len(first.WorkChanges) != 1 || len(first.WorkChanges[0].WorkDetailRequests) != 1 {
				t.Fatal(first, err)
			}
			route := first.WorkChanges[0].WorkDetailRequests[0]
			if route.WorkRef != reviewRef(r) || route.WorkField != name || route.WorkSession != "ws" || !route.IncludeDescendants || route.SourceIncarnation != "incarnation" || route.ReadLimit != 64 || route.RecordLimit != 256 || route.DetailLimit != 512<<10 {
				t.Fatal("route changed original scope or bounds", route)
			}
			work := CurrentWork(first)
			if len(work.WorkDetailRequests) != 1 || work.WorkDetailRequests[0] != route || len([]rune(**workField(&work.Fields, name))) != 256 {
				t.Fatal("material lost its clipped-field route", work)
			}
			raw, _ := json.Marshal(route)
			var exact ReviewRequest
			if err := json.Unmarshal(raw, &exact); err != nil {
				t.Fatal(err)
			}
			got, err := Review(f, exact)
			if err != nil || got.WorkDetail == nil || got.WorkDetail.Value != text || got.WorkDetail.Ref != route.WorkRef || got.WorkDetail.Field != name || got.WorkDetail.WorkSession != "ws" {
				t.Fatal(got, err)
			}
			encoded, _ := json.Marshal(got)
			var wire map[string]json.RawMessage
			json.Unmarshal(encoded, &wire)
			if len(wire) != 6 || wire["work_detail"] == nil || wire["coverage"] == nil || wire["work_changes"] != nil || len(encoded) > MaxReviewBytes || got.Scope != "retained_work_field_detail" {
				t.Fatal("detail repeated aggregate or exceeded bound", string(encoded))
			}
			if **workField(r.Details.Work, name) != text {
				t.Fatal("presentation mutated the retained source")
			}
		})
	}
}

func TestCurrentWorkDropsOnlyOverwrittenDetailRoutes(t *testing.T) {
	long := strings.Repeat("x", 300)
	newLong := long + "new"
	short, clear, unrelated := "short", "", "another next action"
	change := func(id string, seq uint64, field, value string) WorkFact {
		w := sdk.InteractionWorkChange{Session: "ws"}
		*workField(&w, field) = &value
		ref := Ref{ID: id, Sequence: seq, SHA256: strings.Repeat("a", 64)}
		fact := WorkFact{Ref: ref, Change: w, TruncatedFields: []string{}}
		if len(value) > 256 {
			fact.TruncatedFields = []string{field}
			fact.WorkDetailRequests = []WorkSelection{{WorkSession: "ws", WorkRef: ref, WorkField: field}}
		}
		return fact
	}
	r := ReviewResult{WorkSession: "ws", WorkChanges: []WorkFact{change("long", 1, "falsifier", long)}}
	check := func(want string) {
		t.Helper()
		work := CurrentWork(r)
		if want == "" {
			if len(work.WorkDetailRequests) != 0 || len(work.TruncatedFields) != 0 {
				t.Fatal("stale route survived overwrite/clear", work)
			}
		} else if len(work.WorkDetailRequests) != 1 || work.WorkDetailRequests[0].WorkRef.ID != want {
			t.Fatal("latest retained field route lost", work)
		}
	}
	check("long")
	r.WorkChanges = append(r.WorkChanges, change("unrelated", 2, "next_move", unrelated))
	check("long")
	r.WorkChanges = append(r.WorkChanges, change("short", 3, "falsifier", short))
	check("")
	r.WorkChanges = append(r.WorkChanges, change("long-again", 4, "falsifier", long))
	check("long-again")
	r.WorkChanges = append(r.WorkChanges, change("clear", 5, "falsifier", clear))
	check("")
	r.WorkChanges = append(r.WorkChanges, change("new-long", 6, "falsifier", newLong))
	check("new-long")
	child := change("child", 7, "falsifier", long)
	child.Change.Session = "child"
	r.WorkChanges = append(r.WorkChanges, child)
	check("new-long")
}

func TestWorkDetailScopeAndRefusals(t *testing.T) {
	for _, name := range []string{"hash", "sequence", "incarnation", "missing", "wrong kind", "wrong session", "notice mismatch", "absent field", "read budget", "denied", "child", "grandchild", "foreign", "no descendants", "ancestry incomplete", "cleared"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(sample("passed"), 0)
			r := f.rows[1]
			text := "whole criterion"
			r.Details.Work = &sdk.InteractionWorkChange{Session: "ws", Falsifier: &text}
			q := workRequest(r, "falsifier")
			want := error(nil)
			switch name {
			case "hash":
				q.WorkRef.SHA256 = strings.Repeat("b", 64)
				want = ErrConflict
			case "sequence":
				q.WorkRef.Sequence++
				want = ErrConflict
			case "incarnation":
				q.SourceIncarnation = "old"
				want = ErrSourceChanged
			case "missing":
				q.WorkRef.ID = "absent"
				want = ErrWorkFieldUnavailable
			case "wrong kind":
				r.Kind = sdk.InteractionMessage
				q = workRequest(r, "falsifier")
				want = ErrWorkFieldUnavailable
			case "wrong session":
				q.WorkSession = "other"
				want = ErrWorkFieldUnavailable
			case "notice mismatch":
				r.Details.Work.Session = "other"
				q = workRequest(r, "falsifier")
				want = ErrProtocol
			case "absent field":
				q.WorkField = "expected_evidence"
				want = ErrWorkFieldUnavailable
			case "read budget":
				q.ReadLimit = 1
				want = ErrWorkFieldUnavailable
			case "denied":
				want = &sdk.Denied{ReasonCode: "POLICY_DENY"}
				f.denial = want
			case "child", "grandchild", "foreign", "no descendants", "ancestry incomplete":
				r.Details.WorkSession, r.Details.Work.Session = "child", "child"
				f.descendants = map[string]bool{"child": true, "grandchild": true}
				if name == "grandchild" {
					r.Details.WorkSession, r.Details.Work.Session = "grandchild", "grandchild"
				}
				if name == "foreign" {
					r.Details.WorkSession, r.Details.Work.Session = "foreign", "foreign"
				}
				q = workRequest(r, "falsifier")
				q.IncludeDescendants = name != "no descendants"
				if name == "foreign" || name == "no descendants" {
					want = ErrWorkFieldUnavailable
				}
			case "cleared":
				text = ""
				q = workRequest(r, "falsifier")
			}
			f.rows = []sdk.InteractionRecord{r}
			var reader Reader = f
			if name == "ancestry incomplete" {
				reader = readerOverride{query: func(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
					page, err := f.Query(q)
					if page != nil {
						page.Availability = "ancestry_incomplete"
					}
					return page, err
				}, read: f.Read}
			}
			got, err := Review(reader, q)
			if want != nil {
				if !errors.Is(err, want) {
					t.Fatal(err, "want", want)
				}
				return
			}
			if err != nil || got.WorkDetail == nil || got.WorkDetail.Value != text || got.WorkDetail.WorkSession != r.Details.WorkSession || got.WorkSession != "ws" {
				t.Fatal(got, err)
			}
			if name == "ancestry incomplete" && got.Coverage.Complete {
				t.Fatal("ancestry omission became complete")
			}
		})
	}
}

func TestWorkDetailAvoidsTheAggregateBudgetCycle(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	text := strings.Repeat("界", 300) + " exact criterion tail"
	f.rows = nil
	for i := 1; i <= 40; i++ {
		f.rows = append(f.rows, sdk.InteractionRecord{ID: fmt.Sprint("work-", i), Sequence: uint64(i), Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Falsifier: &text}}})
	}
	q := DefaultReviewRequest("ws")
	compact, err := Review(f, q)
	if err != nil || len(compact.Compact().CurrentWork.WorkDetailRequests) != 2 {
		t.Fatal(compact, err)
	}
	route := compact.Compact().CurrentWork.WorkDetailRequests[0]
	q.View = "full"
	if _, err := Review(f, q); !errors.Is(err, ErrReviewTooLarge) {
		t.Fatal("fixture did not pressure aggregate", err)
	}
	raw, _ := json.Marshal(route)
	var exact ReviewRequest
	json.Unmarshal(raw, &exact)
	got, err := Review(f, exact)
	if err != nil || got.WorkDetail == nil || got.WorkDetail.Value != text {
		t.Fatal("detail depended on aggregate fitting", got, err)
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > MaxReviewBytes {
		t.Fatal("detail exceeded output budget")
	}
}

func TestWorkDetailValidationBeforeRead(t *testing.T) {
	for _, name := range []string{"no ref", "no field", "unknown field", "both selectors", "no incarnation", "bad ref", "zero sequence"} {
		f := newFixture(sample("passed"), 0)
		q := workRequest(f.rows[1], "plan")
		switch name {
		case "no ref":
			q.WorkRef = nil
		case "no field":
			q.WorkField = ""
		case "unknown field":
			q.WorkField = "annotations"
		case "both selectors":
			q.ResultRef = q.WorkRef
		case "no incarnation":
			q.SourceIncarnation = ""
		case "bad ref":
			q.WorkRef.SHA256 = "bad"
		case "zero sequence":
			q.WorkRef.Sequence = 0
		}
		if _, err := Review(f, q); !errors.Is(err, ErrInvalid) || f.queries != 0 {
			t.Fatal(name, err, f.queries)
		}
	}
}

func TestWorkDetailUsesTransportHashAndWholeFieldBudget(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	r := f.rows[1]
	text := strings.Repeat("界", 6000) + "whole tail"
	r.Details.Work = &sdk.InteractionWorkChange{Session: "ws", Falsifier: &text}
	r.Annotations = map[string]json.RawMessage{"note": json.RawMessage(`"first"`)}
	f.rows = []sdk.InteractionRecord{r}
	reader := &detailFixture{fixture: f, full: r}
	q := workRequest(r, "falsifier")
	q.ReadLimit = 3
	if _, err := Review(reader, q); !errors.Is(err, ErrWorkFieldUnavailable) || reader.reads != 1 {
		t.Fatal("partial record exposed", err, reader.reads)
	}
	q.ReadLimit = 8
	reader.full.Annotations["note"] = json.RawMessage(`"changed enrichment"`)
	got, err := Review(reader, q)
	if err != nil || got.WorkDetail == nil || got.WorkDetail.Value != text || got.Coverage.DetailBytes <= sdk.InteractionReadLimit {
		t.Fatal("stable ref/transport hash were interchanged", got, err)
	}
	reader.corrupt = true
	if _, err := Review(reader, q); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("corrupt detail accepted", err)
	}
	reader.corrupt = false
	reader.denyRead = &sdk.Denied{ReasonCode: "POLICY_DENY"}
	if _, err := Review(reader, q); err != reader.denyRead {
		t.Fatal("read denial was hidden", err)
	}
	for _, value := range []string{strings.Repeat("界", 5000), strings.Repeat("<", 8000), strings.Repeat("<", MaxReviewBytes/6)} {
		text = value
		f.rows = []sdk.InteractionRecord{r}
		q = workRequest(r, "falsifier")
		got, err = Review(f, q)
		if len(value) == MaxReviewBytes/6 {
			if !errors.Is(err, ErrWorkFieldTooLarge) || !strings.Contains(err.Error(), "lowering record_limit cannot shrink") {
				t.Fatal("whole-field overflow has misleading recovery", err)
			}
		} else if err != nil || got.WorkDetail.Value != value {
			t.Fatal("fitting field clipped", got, err)
		}
	}
}
