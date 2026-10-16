//go:build !wasm_unknown

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

func TestReviewExactSelectorParsing(t *testing.T) {
	raw := `{"work_session_id":"ws","source_incarnation":"inc","result_ref":{"id":"r","sequence":"3","sha256":"` + strings.Repeat("a", 64) + `"}}`
	q, err := parseReviewContext([]byte(raw), sdk.WorkSession{ID: "later"})
	if err != nil || q.WorkSession != "ws" || q.IncludeDescendants || q.ResultRef == nil || q.ResultRef.Sequence != 3 || q.SourceIncarnation != "inc" {
		t.Fatal(q, err)
	}
	for _, bad := range []string{
		strings.Replace(raw, `"sequence":"3"`, `"sequence":3`, 1),
		strings.Replace(raw, `"sequence":"3"`, `"sequence":"0"`, 1),
		strings.Replace(raw, `"id":"r"`, `"id":"r","other":1`, 1),
		strings.Replace(raw, `"source_incarnation":"inc",`, "", 1),
		strings.Replace(raw, `"source_incarnation":"inc"`, `"source_incarnation":null`, 1),
		strings.Replace(raw, `"id":"r"`, `"id":null`, 1),
		`{"work_session_id":"ws","source_incarnation":"inc"}`,
	} {
		if _, err := parseReview([]byte(bad)); !errors.Is(err, assessment.ErrInvalid) {
			t.Fatal("accepted invalid selector", bad, err)
		}
	}
}
func TestNativeReviewAdvertisedDetailRoute(t *testing.T) {
	source := &sdk.InteractionSource{Kind: "tool", ID: "execution"}
	rows := []sdk.InteractionRecord{
		{ID: "call", Sequence: 1, Kind: sdk.InteractionToolCall, ProjectID: "p", TurnID: "turn", Source: source, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		{ID: "result", Sequence: 2, Kind: sdk.InteractionToolResult, ProjectID: "p", TurnID: "turn", Source: source, RelatedID: "call", Outcome: sdk.InteractionFailed, Content: strings.Repeat("<", assessment.MaxResultDetailRunes-19) + "exact failure cause", Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
	}
	for i := uint64(3); i < 12; i++ {
		text := strings.Repeat("界", 300)
		rows = append(rows, sdk.InteractionRecord{ID: fmt.Sprint("work-", i), Sequence: i, Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &text, Result: &text, EvidenceReadback: &text}}})
	}
	k := newFakeKV()
	denied := false
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op != "interaction.query" {
			t.Fatal("unexpected reach", op)
		}
		if denied {
			return denyBody("history unavailable"), true
		}
		var q sdk.InteractionQuery
		if err := json.Unmarshal(args, &q); err != nil {
			t.Fatal(err)
		}
		selected := rows
		if q.Filter.WorkSession == "" {
			selected = rows[len(rows)-1:]
		} else if q.Filter.WorkSession != "ws" {
			selected = []sdk.InteractionRecord{}
		}
		page := sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "inc", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected}
		raw, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
		return raw, false
	}
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws"}
	first := h.result("review", map[string]any{})
	firstRaw, _ := json.Marshal(first)
	t.Logf("REVIEW_SCHEMA_PROOF %s", firstRaw)
	route := first["attempts"].([]any)[0].(map[string]any)["result_detail_request"].(map[string]any)
	h.workSession = map[string]any{"id": "later-work"}
	got := h.result("review", route)
	detail := got["result_detail"].(map[string]any)
	if detail["content"] != rows[1].Content || detail["work_session_id"] != "ws" || k.puts != 0 {
		t.Fatal(got, k.puts)
	}
	raw, _ := json.Marshal(got)
	if len(raw) > assessment.MaxReviewBytes || len(got) != 6 || got["counts"] != nil || got["attempts"] != nil || got["scope"] != "retained_result_detail" {
		t.Fatal("detail repeated the aggregate or exceeded its budget")
	}
	t.Logf("REVIEW_SCHEMA_PROOF %s", raw)
	route["source_incarnation"] = "old"
	if code := h.failure("review", route); code != "REVIEW_SOURCE_CHANGED" {
		t.Fatal(code)
	}
	route["source_incarnation"] = "inc"
	route["work_session_id"] = "other"
	if code := h.failure("review", route); code != "REVIEW_RESULT_UNAVAILABLE" {
		t.Fatal(code)
	}
	route["work_session_id"] = "ws"
	denied = true
	if code := h.failure("review", route); code != "POLICY_DENY" {
		t.Fatal(code)
	}
}
