//go:build !wasm_unknown

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

func workNoticeHarness(t *testing.T, rows []sdk.InteractionRecord, transportID string) (*harness, *fakeBroker) {
	t.Helper()
	k := newFakeBroker()
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		var value any
		switch op {
		case "interaction.query":
			var q sdk.InteractionQuery
			if err := json.Unmarshal(args, &q); err != nil {
				t.Fatal(err)
			}
			before := ^uint64(0)
			if q.Cursor != "" {
				var err error
				before, err = strconv.ParseUint(q.Cursor, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
			}
			selected := []sdk.InteractionRecord{}
			for _, r := range rows {
				if r.Sequence >= before || q.Filter.WorkSession != "" && r.Details.WorkSession != q.Filter.WorkSession && !(q.Filter.IncludeDescendants && q.Filter.WorkSession == "ws" && r.Details.WorkSession == "child") {
					continue
				}
				if r.ID == transportID {
					raw, _ := json.Marshal(r)
					r.Content, r.Details, r.Annotations, r.Deliveries = "", sdk.InteractionDetails{}, nil, nil
					r.ContentRef = &sdk.InteractionContentRef{ID: r.ID, Source: "recorded", Incarnation: "inc", SHA256: assessment.Hash(raw), Bytes: len(raw)}
				}
				selected = append(selected, r)
			}
			next := ""
			if len(selected) > q.Limit {
				selected = selected[len(selected)-q.Limit:]
				next = fmt.Sprint(selected[0].Sequence)
			}
			value = sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "inc", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected, NextCursor: next}
		case "interaction.read":
			var q sdk.InteractionReadRequest
			if err := json.Unmarshal(args, &q); err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.ID != q.ID {
					continue
				}
				raw, _ := json.Marshal(r)
				if q.Source != "recorded" || q.Incarnation != "inc" || q.SHA256 != assessment.Hash(raw) || q.Offset < 0 || q.Offset >= int64(len(raw)) || q.Length > sdk.InteractionReadLimit {
					t.Fatal("detail used a stable reference as transport hash or an invalid range", q)
				}
				end := min(int(q.Offset)+q.Length, len(raw))
				chunk := raw[q.Offset:end]
				value = sdk.InteractionReadResult{Version: 1, Source: "recorded", ID: r.ID, Incarnation: "inc", SHA256: q.SHA256, Data: base64.StdEncoding.EncodeToString(chunk), Bytes: len(chunk), Offset: q.Offset, Size: int64(len(raw)), EOF: end == len(raw)}
				break
			}
			if value == nil {
				t.Fatal("unexpected detail id", q.ID)
			}
		default:
			t.Fatal("work detail attempted another capability", op)
		}
		raw, err := json.Marshal(map[string]any{"status": "succeeded", "operation_result": value})
		if err != nil {
			t.Fatal(err)
		}
		return raw, false
	}
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws"}
	return h, k
}

func TestWorkNoticeSelectorParsing(t *testing.T) {
	base := `{"work_session_id":"ws","include_descendants":true,"source_incarnation":"inc","work_ref":{"id":"notice","sequence":"3","sha256":"` + strings.Repeat("a", 64) + `"},"work_field":"falsifier"}`
	q, err := parseReviewContext([]byte(base), sdk.WorkSession{ID: "later"})
	if err != nil || q.WorkRef == nil || q.WorkField != "falsifier" || q.WorkSession != "ws" || !q.IncludeDescendants {
		t.Fatal(q, err)
	}
	for _, raw := range []string{
		strings.Replace(base, `"work_field":"falsifier"`, `"work_field":"annotations"`, 1),
		strings.Replace(base, `,"work_field":"falsifier"`, "", 1),
		strings.Replace(base, `"source_incarnation":"inc",`, "", 1),
		strings.Replace(base, `"sequence":"3"`, `"sequence":3`, 1),
		strings.Replace(base, `"id":"notice"`, `"id":"notice","extra":true`, 1),
		strings.Replace(base, `"work_field":"falsifier"`, `"work_field":null`, 1),
		strings.Replace(base, `"work_field":"falsifier"`, `"work_field":"falsifier","result_ref":{"id":"r","sequence":"1","sha256":"`+strings.Repeat("a", 64)+`"}`, 1),
		`{"work_session_id":"ws","work_field":"plan"}`,
	} {
		if _, err := parseReviewContext([]byte(raw), sdk.WorkSession{ID: "ws"}); !errors.Is(err, assessment.ErrInvalid) {
			t.Fatal("accepted selector", raw, err)
		}
	}
}

func TestNativeWorkNoticeRoutesAndRefusals(t *testing.T) {
	text := strings.Repeat("界", 300) + " complete authored tail"
	criterion := strings.Repeat("界", 6000) + " exact falsifier tail"
	w := &sdk.InteractionWorkChange{Session: "ws", State: &text, Focus: &text, NextMove: &text, Plan: &text, ExpectedEvidence: &text, Falsifier: &criterion, DecisionNeeded: &text, Result: &text, Evidence: &text, EvidenceReadback: &text}
	rows := []sdk.InteractionRecord{
		{ID: "notice", Sequence: 1, Kind: sdk.InteractionNotice, Annotations: map[string]json.RawMessage{"note": json.RawMessage(`"enrichment"`)}, Details: sdk.InteractionDetails{WorkSession: "ws", Work: w}},
		{ID: "child-notice", Sequence: 2, Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "child", Work: &sdk.InteractionWorkChange{Session: "child", Falsifier: &text}}},
		{ID: "foreign-notice", Sequence: 3, Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "foreign", Work: &sdk.InteractionWorkChange{Session: "foreign", Falsifier: &text}}},
		{ID: "not-a-notice", Sequence: 4, Kind: sdk.InteractionMessage, Details: sdk.InteractionDetails{WorkSession: "ws"}},
	}
	h, k := workNoticeHarness(t, rows, "notice")
	first := h.result("review", map[string]any{"read_limit": 64, "record_limit": 256, "detail_limit": 524288})
	work := first["current_work"].(map[string]any)
	routes := work["work_detail_requests"].([]any)
	if len(routes) != 10 {
		t.Fatal("not every clipped field has a route", work)
	}
	log := func(marker string, value any) {
		t.Helper()
		raw, _ := json.Marshal(value)
		t.Logf("%s %s", marker, raw)
	}
	log("REVIEW_SCHEMA_PROOF", first)
	h.workSession = map[string]any{"id": "later-work", "project_id": "unrelated-ui-project"}
	for _, rawRoute := range routes {
		serialized, _ := json.Marshal(rawRoute)
		var route map[string]any
		json.Unmarshal(serialized, &route)
		if route["work_session_id"] != "ws" || route["include_descendants"] != true || route["read_limit"] != float64(64) || route["record_limit"] != float64(256) || route["detail_limit"] != float64(524288) {
			t.Fatal("route lost original scope", route)
		}
		got := h.result("review", route)
		detail := got["work_detail"].(map[string]any)
		want := text
		if route["work_field"] == "falsifier" {
			want = criterion
		}
		if detail["value"] != want || detail["field"] != route["work_field"] || detail["work_session_id"] != "ws" || got["work_session_id"] != "ws" || got["scope"] != "retained_work_field_detail" || len(got) != 6 {
			t.Fatal("whole-field route failed", got)
		}
		log("REVIEW_INPUT_SCHEMA_PROOF", route)
		log("REVIEW_SCHEMA_PROOF", got)
	}

	h.workSession = map[string]any{"id": "ws"}
	current := h.result("review", map[string]any{})
	if len(current["current_work"].(map[string]any)["work_detail_requests"].([]any)) != 10 {
		t.Fatal(current)
	}
	full := h.result("review", map[string]any{"work_session_id": "ws", "include_descendants": true, "view": "full"})
	log("REVIEW_SCHEMA_PROOF", full)
	var childRoute map[string]any
	for _, v := range full["work_changes"].([]any) {
		fact := v.(map[string]any)
		if fact["change"].(map[string]any)["session"] == "child" {
			childRoute = fact["work_detail_requests"].([]any)[0].(map[string]any)
		}
	}
	if childRoute == nil || childRoute["work_session_id"] != "ws" || childRoute["include_descendants"] != true {
		t.Fatal("child route changed root scope", full)
	}
	got := h.result("review", childRoute)
	if got["work_detail"].(map[string]any)["work_session_id"] != "child" || got["work_session_id"] != "ws" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ name, code string }{{"incarnation", "REVIEW_SOURCE_CHANGED"}, {"hash", "REVIEW_EVIDENCE_CONFLICT"}, {"sequence", "REVIEW_EVIDENCE_CONFLICT"}, {"kind", "REVIEW_RESULT_UNAVAILABLE"}, {"foreign", "REVIEW_RESULT_UNAVAILABLE"}, {"descendants", "REVIEW_RESULT_UNAVAILABLE"}} {
		raw, _ := json.Marshal(childRoute)
		var route map[string]any
		json.Unmarshal(raw, &route)
		ref := route["work_ref"].(map[string]any)
		switch tc.name {
		case "incarnation":
			route["source_incarnation"], ref["incarnation"] = "old", "old"
		case "hash":
			ref["sha256"] = strings.Repeat("b", 64)
		case "sequence":
			ref["sequence"] = "99"
		case "kind":
			b, _ := json.Marshal(assessment.RecordRef(rows[3]))
			json.Unmarshal(b, &ref)
			route["work_ref"] = ref
		case "foreign":
			b, _ := json.Marshal(assessment.RecordRef(rows[2]))
			json.Unmarshal(b, &ref)
			route["work_ref"] = ref
		case "descendants":
			route["include_descendants"] = false
		}
		if code := h.failure("review", route); code != tc.code {
			t.Fatal(tc.name, code, tc.code)
		}
	}
	broker := k.readOnlyCall
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op == "interaction.read" {
			return denyBody("detail denied"), true
		}
		return broker(op, target, args)
	}
	if code := h.failure("review", routes[0].(map[string]any)); code != "POLICY_DENY" {
		t.Fatal("detail denial hidden", code)
	}
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op == "interaction.read" {
			return []byte(`{"status":"failed","reasonCode":"INTERACTION_SOURCE_UNAVAILABLE"}`), false
		}
		return broker(op, target, args)
	}
	if code := h.failure("review", routes[0].(map[string]any)); code != "INTERACTION_SOURCE_UNAVAILABLE" {
		t.Fatal("source error hidden", code)
	}
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op == "interaction.query" {
			return denyBody("query denied"), true
		}
		return broker(op, target, args)
	}
	if code := h.failure("review", routes[0].(map[string]any)); code != "POLICY_DENY" {
		t.Fatal("query denial hidden", code)
	}
}

func TestNativeWorkDetailAvoidsAggregateBudgetCycle(t *testing.T) {
	text := strings.Repeat("界", 300) + " exact retained tail"
	rows := []sdk.InteractionRecord{}
	for i := 1; i <= 40; i++ {
		rows = append(rows, sdk.InteractionRecord{ID: fmt.Sprint("work-", i), Sequence: uint64(i), Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Falsifier: &text}}})
	}
	h, _ := workNoticeHarness(t, rows, "")
	compact := h.result("review", map[string]any{})
	routes := compact["current_work"].(map[string]any)["work_detail_requests"].([]any)
	if len(routes) != 2 {
		t.Fatal(compact)
	}
	if code := h.failure("review", map[string]any{"work_session_id": "ws", "view": "full"}); code != "REVIEW_RESULT_TOO_LARGE" {
		t.Fatal("fixture did not pressure aggregate", code)
	}
	h.workSession = map[string]any{"id": "later"}
	for _, route := range routes {
		got := h.result("review", route.(map[string]any))
		if got["work_detail"].(map[string]any)["value"] != text || len(got) != 6 {
			t.Fatal("detail requires fitting aggregate", got)
		}
		raw, _ := json.Marshal(got)
		if len(raw) > assessment.MaxReviewBytes {
			t.Fatal("detail exceeds bound")
		}
		t.Logf("REVIEW_SCHEMA_PROOF %s", raw)
	}
}

func TestNativeWorkFieldClearMissingAndEncodedBudget(t *testing.T) {
	for _, tc := range []struct{ name, value, field, code string }{
		{"cleared", "", "falsifier", ""},
		{"missing", "authored", "expected_evidence", "REVIEW_RESULT_UNAVAILABLE"},
		{"unicode", strings.Repeat("界", 5000) + "tail", "falsifier", ""},
		{"escaping", strings.Repeat("<", 8000), "falsifier", ""},
		{"overflow", strings.Repeat("<", assessment.MaxReviewBytes/6), "falsifier", "REVIEW_RESULT_TOO_LARGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sdk.InteractionRecord{ID: "work", Sequence: 1, Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Falsifier: &tc.value}}}
			h, _ := workNoticeHarness(t, []sdk.InteractionRecord{r}, "work")
			ref, _ := json.Marshal(assessment.RecordRef(r))
			var wireRef map[string]any
			json.Unmarshal(ref, &wireRef)
			route := map[string]any{"work_session_id": "ws", "source_incarnation": "inc", "work_ref": wireRef, "work_field": tc.field}
			if tc.code != "" {
				if code := h.failure("review", route); code != tc.code {
					t.Fatal(code, tc.code)
				}
			} else {
				got := h.result("review", route)
				if got["work_detail"].(map[string]any)["value"] != tc.value {
					t.Fatal("field clipped", got)
				}
				raw, _ := json.Marshal(got)
				if len(raw) > assessment.MaxReviewBytes {
					t.Fatal("unbounded detail")
				}
				t.Logf("REVIEW_SCHEMA_PROOF %s", raw)
			}
		})
	}
}
