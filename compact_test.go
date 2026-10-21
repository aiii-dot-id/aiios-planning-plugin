//go:build !wasm_unknown

package main

import (
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func TestCompactSelectionAndPrimaryReview(t *testing.T) {
	for _, raw := range []string{`{"view":"compact"}`, `{"view":"full"}`} {
		q, err := parseReviewContext([]byte(raw), sdk.WorkSession{ID: "ws"})
		if err != nil || q.View == "" {
			t.Fatal(q, err)
		}
	}
	for _, raw := range []string{`{"view":""}`, `{"view":"all"}`, `{"view":null}`, `{"view":true}`} {
		if _, err := parseReviewContext([]byte(raw), sdk.WorkSession{ID: "ws"}); err == nil {
			t.Fatal(raw)
		}
	}
	primary := []string{}
	operations := []string{}
	for _, d := range newPlanningPlugin().Descriptors() {
		operations = append(operations, d.ID)
		if d.Primary {
			primary = append(primary, d.ID)
		}
	}
	if !reflect.DeepEqual(primary, []string{"review"}) || !reflect.DeepEqual(operations, []string{"review"}) {
		t.Fatal(primary, operations)
	}
}

func TestNativeCompactFullRoutePreservesUnknownEvidence(t *testing.T) {
	source := &sdk.InteractionSource{Kind: "tool", ID: "run"}
	rows := []sdk.InteractionRecord{
		{ID: "call", Sequence: 1, Kind: sdk.InteractionToolCall, TurnID: "turn", Source: source, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		{ID: "result", Sequence: 2, Kind: sdk.InteractionToolResult, TurnID: "turn", Source: source, RelatedID: "call", Outcome: sdk.InteractionSucceeded, Content: "command exited 9", Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
	}
	k := newFakeBroker()
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op != "interaction.query" {
			t.Fatal(op)
		}
		var q sdk.InteractionQuery
		if err := json.Unmarshal(args, &q); err != nil {
			t.Fatal(err)
		}
		selected := rows
		if q.Filter.WorkSession == "" {
			selected = rows[1:]
		}
		page := sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "inc", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected}
		raw, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
		return raw, false
	}
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws"}
	compact := h.result("review", map[string]any{})
	shown := compact["attempts"].([]any)
	if compact["view"] != "compact" || compact["correctness"] == nil || len(shown) != 1 || compact["omitted"].(map[string]any)["attempts"] != float64(0) {
		t.Fatal(compact)
	}

	worked := shown[0].(map[string]any)
	for _, name := range []string{"request_ref", "result_ref"} {
		ref := worked[name].(map[string]any)
		if worked["outcome"] != "succeeded" || len(ref) != 4 || ref["id"] == nil || ref["sequence"] == nil || ref["sha256"] == nil || ref["incarnation"] != "inc" {
			t.Fatal(name, worked)
		}
	}
	if compact["evidence"] != nil || compact["diagnostics"] != nil || compact["counts"].(map[string]any)["admissions"] != nil {
		t.Fatal("compact repeated references or counts", compact)
	}
	route := compact["full_review_request"].(map[string]any)
	full := h.result("review", route)
	attempts := full["attempts"].([]any)
	if len(attempts) != 1 || attempts[0].(map[string]any)["result_excerpt"] != "command exited 9" || full["view"] != nil || len(full["evidence"].([]any)) != 2 {
		t.Fatal(full)
	}
	if !reflect.DeepEqual(full["counts"], compact["counts"]) || full["source_incarnation"] != compact["source_incarnation"] {
		t.Fatal("full route changed counts/source or wrote state")
	}
	for _, v := range []map[string]any{compact, full} {
		b, _ := json.Marshal(v)
		t.Logf("REVIEW_SCHEMA_PROOF %s", b)
	}
}
