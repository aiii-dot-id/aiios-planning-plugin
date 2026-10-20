//go:build !wasm_unknown

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiospkg"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

func TestReviewInputAndSchemas(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"work_session_id":null}`, `{"work_session_id":"ws","read_limit":0}`, `{"work_session_id":"ws","identity":"another"}`, `{"work_session_id":"ws","Work_session_id":"other"}`, `{"work_session_id":"ws","work_session_id":"other"}`, `{"work_session_id":"ws","read_limit":1.5}`} {
		if _, err := parseReview([]byte(raw)); !errors.Is(err, assessment.ErrInvalid) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
	q, err := parseReview([]byte(`{"work_session_id":"ws","read_limit":32.0,"record_limit":1e2,"_host_now_ms":123}`))
	if err != nil || q.ReadLimit != 32 || q.RecordLimit != 100 {
		t.Fatal(q, err)
	}
	for _, name := range []string{"review_in", "review_out", "plan_in", "plan_out"} {
		raw, err := os.ReadFile("schemas/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if err = aiiospkg.CheckSchemaSubset(raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewArgumentFailuresNameTheRequirementWithoutEchoingInput(t *testing.T) {
	const private = "PRIVATE_ARGUMENT_CONTENT"
	ref := `{"id":"record","sequence":"1","sha256":"` + strings.Repeat("a", 64) + `"}`
	host := sdk.WorkSession{ID: "ws-current", CallID: "own-call"}
	for _, tc := range []struct{ name, raw, want string }{
		{"null object", `null`, "arguments must be a JSON object"},
		{"array", `[]`, "arguments must be a JSON object"},
		{"duplicate key", `{"view":"compact","view":"full"}`, "unique keys"},
		{"view", `{"view":"` + private + `"}`, `view must be "compact" or "full"`},
		{"session type", `{"work_session_id":17}`, "work_session_id must name an existing session"},
		{"empty session", `{"work_session_id":""}`, "work_session_id must name an existing session"},
		{"session characters", `{"work_session_id":"../` + private + `/"}`, "1..128 ASCII"},
		{"boolean type", `{"include_descendants":"true"}`, "include_descendants must be a JSON boolean"},
		{"null field", `{"read_limit":null}`, "read_limit must be an integer from 1 to 128"},
		{"integer type", `{"read_limit":"` + private + `"}`, "read_limit must be an integer from 1 to 128"},
		{"integer fraction", `{"read_limit":1.5}`, "read_limit must be an integer from 1 to 128"},
		{"read bound", `{"read_limit":129}`, "read_limit must be an integer from 1 to 128"},
		{"record bound", `{"record_limit":513}`, "record_limit must be an integer from 1 to 512"},
		{"detail bound", `{"detail_limit":1048577}`, "detail_limit must be an integer from 1 to 1048576 bytes"},
		{"unpaired source", `{"source_incarnation":"` + private + `"}`, "source_incarnation must be a string"},
		{"result shape", `{"result_ref":{"id":"` + private + `"}}`, "result_ref and work_ref must each contain only"},
		{"reference sequence", `{"source_incarnation":"source","result_ref":{"id":"` + private + `","sequence":"0","sha256":"` + strings.Repeat("a", 64) + `"}}`, "positive decimal string"},
		{"reference hash", `{"source_incarnation":"source","result_ref":{"id":"` + private + `","sequence":"1","sha256":"` + strings.Repeat("A", 64) + `"}}`, "64 lowercase hexadecimal"},
		{"reference conflict", `{"source_incarnation":"source","result_ref":` + ref + `,"work_ref":` + ref + `,"work_field":"state"}`, "result_ref and work_ref are mutually exclusive"},
		{"field without reference", `{"work_field":"state"}`, "work_ref and work_field must be supplied together"},
		{"reference without field", `{"source_incarnation":"source","work_ref":` + ref + `}`, "work_ref and work_field must be supplied together"},
		{"unknown work field", `{"source_incarnation":"source","work_ref":` + ref + `,"work_field":"` + private + `"}`, "work_field must be state, focus"},
		{"unknown argument", `{"` + private + `":"` + private + `"}`, "unsupported review argument"},
		{"unknown null argument", `{"` + private + `":null}`, "unsupported review argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReviewContext([]byte(tc.raw), host)
			if !errors.Is(err, assessment.ErrInvalid) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `Example with an acting work session: {"view":"compact"}`) {
				t.Fatalf("invalid review lost its classification, requirement or usable example: %v", err)
			}
			if strings.Contains(err.Error(), private) {
				t.Fatalf("review refusal echoed a supplied argument: %v", err)
			}
			var failure *sdk.OperationError
			if !errors.As(reviewFailure(err), &failure) || failure.ReasonCode != "OPERATION_ARGUMENT_INVALID" {
				t.Fatalf("invalid review changed its plugin error classification: %v", reviewFailure(err))
			}
		})
	}
}

func TestReviewDiagnosticsPreserveValidInputsAndScope(t *testing.T) {
	host := sdk.WorkSession{ID: "ws-current", CallID: "own-call"}
	for _, raw := range []string{`{}`, `{"view":"compact"}`, `{"view":"full","include_descendants":false}`, `{"read_limit":32.0,"record_limit":1e2,"detail_limit":1048576,"_host_now_ms":123}`, `{"source_incarnation":""}`} {
		q, err := parseReviewContext([]byte(raw), host)
		if err != nil || q.WorkSession != host.ID || q.ExcludeCallID != host.CallID {
			t.Fatalf("previously valid aggregate input or host scope changed: %s, %+v, %v", raw, q, err)
		}
	}
	q, err := parseReviewContext([]byte(`{"work_session_id":"ws-other","read_limit":128,"record_limit":512,"detail_limit":1}`), host)
	if err != nil || q.WorkSession != "ws-other" || q.IncludeDescendants || q.ReadLimit != 128 || q.RecordLimit != 512 || q.DetailLimit != 1 {
		t.Fatal("explicit-session scope or valid boundary changed", q, err)
	}
	ref := `{"id":"record","sequence":"1","sha256":"` + strings.Repeat("a", 64) + `"}`
	for _, field := range []string{"state", "focus", "next_move", "plan", "expected_evidence", "falsifier", "decision_needed", "result", "evidence", "evidence_readback"} {
		q, err := parseReviewContext([]byte(`{"source_incarnation":"source","work_ref":`+ref+`,"work_field":"`+field+`"}`), host)
		if err != nil || q.WorkRef == nil || q.WorkRef.ID != "record" || q.WorkRef.Sequence != 1 || q.WorkField != field || q.SourceIncarnation != "source" || !q.IncludeDescendants {
			t.Fatalf("valid exact work-detail route changed: %s, %+v, %v", field, q, err)
		}
	}
	q, err = parseReviewContext([]byte(`{"source_incarnation":"source","result_ref":`+ref+`}`), host)
	if err != nil || q.ResultRef == nil || q.ResultRef.ID != "record" || q.ResultRef.Sequence != 1 || q.WorkRef != nil {
		t.Fatal("valid exact result-detail route changed", q, err)
	}
}

func TestNativeReviewArgumentFailuresAreActionableBeforeHostReads(t *testing.T) {
	k := newFakeKV()
	reads := 0
	k.readOnlyCall = func(string, json.RawMessage, json.RawMessage) ([]byte, bool) {
		reads++
		return denyBody("invalid review must not reach host data"), true
	}
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws-current", "call_id": "own-call"}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"view", map[string]any{"view": "PRIVATE_ARGUMENT_CONTENT"}, `view must be "compact" or "full"`},
		{"read bound", map[string]any{"read_limit": 129}, "read_limit must be an integer from 1 to 128"},
		{"reference pair", map[string]any{"work_field": "state"}, "work_ref and work_field must be supplied together"},
		{"reference shape", map[string]any{"result_ref": map[string]any{"id": "PRIVATE_ARGUMENT_CONTENT"}}, "positive decimal string"},
		{"unknown argument", map[string]any{"PRIVATE_ARGUMENT_CONTENT": "PRIVATE_ARGUMENT_CONTENT"}, "unsupported review argument"},
		{"unknown null argument", map[string]any{"PRIVATE_ARGUMENT_CONTENT": nil}, "unsupported review argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := h.call("review", tc.args)
			var failure struct {
				Status     string `json:"status"`
				ReasonCode string `json:"reasonCode"`
				Reason     string `json:"reason"`
			}
			if err := json.Unmarshal(response["result"], &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Status != "failed" || failure.ReasonCode != "OPERATION_ARGUMENT_INVALID" || !strings.Contains(failure.Reason, tc.want) || !strings.Contains(failure.Reason, `Example with an acting work session: {"view":"compact"}`) || strings.Contains(failure.Reason, "PRIVATE_ARGUMENT_CONTENT") {
				t.Fatalf("registered plugin lost its safe actionable refusal: %+v", failure)
			}
		})
	}
	if reads != 0 || k.puts != 0 {
		t.Fatalf("argument failures read host data or wrote state: reads=%d puts=%d", reads, k.puts)
	}
}

func TestNativeReviewIsReadOnlyAndPreservesDenial(t *testing.T) {
	plan, outcome := "inspect and run the check", "served: resident reports completion"
	rows := []sdk.InteractionRecord{
		{ID: "plan", Sequence: 1, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &plan}}},
		{ID: "call", Sequence: 2, Kind: sdk.InteractionToolCall, ProjectID: "project", TurnID: "turn", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		{ID: "result", Sequence: 3, Kind: sdk.InteractionToolResult, ProjectID: "project", TurnID: "turn", RelatedID: "call", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Outcome: sdk.InteractionFailed, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		{ID: "outcome", Sequence: 4, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Result: &outcome}}},
	}
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "admitted", true: "denied"}[denied], func(t *testing.T) {
			k := newFakeKV()
			reads := 0
			k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
				reads++
				if op != "interaction.query" {
					t.Errorf("unexpected reach %s", op)
					return denyBody("only history read granted"), true
				}
				if denied {
					return denyBody("interaction grant absent"), true
				}
				var q sdk.InteractionQuery
				if err := json.Unmarshal(args, &q); err != nil {
					t.Fatal(err)
				}
				selected := rows
				if q.Filter.WorkSession == "" {
					if q.Limit != 1 {
						t.Fatal(q)
					}
					selected = rows[len(rows)-1:]
				}
				if q.Filter.WorkSession != "" && q.Filter.WorkSession != "ws" {
					selected = []sdk.InteractionRecord{}
				}
				page := sdk.InteractionPage{Version: 1, Identity: "fixture-identity", Source: "recorded", Incarnation: "fixture", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected}
				raw, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
				return raw, false
			}
			h := startHarness(t, k)
			if denied {
				if code := h.failure("review", map[string]any{"work_session_id": "ws"}); code != "POLICY_DENY" {
					t.Fatal(code)
				}
				return
			}
			got := h.result("review", map[string]any{"work_session_id": "ws", "view": "full"})
			counts := got["counts"].(map[string]any)
			if counts["failed"] != float64(1) || counts["succeeded"] != float64(0) || reads != 2 || k.puts != 0 {
				t.Fatal(got, reads, k.puts)
			}
			if _, ok := got["finding"]; ok {
				t.Fatal("review manufactured a suite grade")
			}
			changes := got["work_changes"].([]any)
			if changes[1].(map[string]any)["change"].(map[string]any)["result"] != outcome {
				t.Fatal("lost resident report")
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("REVIEW_SCHEMA_PROOF %s", encoded)
			missing := h.result("review", map[string]any{"work_session_id": "absent"})
			if missing["counts"] != nil || missing["coverage"].(map[string]any)["complete"] != false {
				t.Fatal(missing)
			}
			encoded, err = json.Marshal(missing)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("REVIEW_SCHEMA_PROOF %s", encoded)
		})
	}
}

func TestReviewDefaultsToTrustedCurrentWork(t *testing.T) {
	host := sdk.WorkSession{ID: "ws-current", CallID: "tool_call_tex_example"}
	q, err := parseReviewContext([]byte(`{}`), host)
	if err != nil || q.WorkSession != host.ID || !q.IncludeDescendants || q.ExcludeCallID != host.CallID {
		t.Fatal(q, err)
	}
	q, err = parseReviewContext([]byte(`{"work_session_id":"ws-other"}`), host)
	if err != nil || q.WorkSession != "ws-other" || q.IncludeDescendants {
		t.Fatal(q, err)
	}
	q, err = parseReviewContext([]byte(`{"include_descendants":false}`), host)
	if err != nil || q.IncludeDescendants {
		t.Fatal(q, err)
	}
	for _, bad := range []string{`{"include_descendants":"true"}`, `{"include_descendants":null}`, `{"include_descendants":1}`} {
		if _, err = parseReviewContext([]byte(bad), host); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if _, err = parseReviewContext([]byte(`{}`), sdk.WorkSession{Reason: "no_session"}); err == nil {
		t.Fatal("missing host context invented session")
	}
}

func TestNativePlanOversizeRecoveryUsesReview(t *testing.T) {
	text := strings.Repeat("界", 300)
	rows := make([]sdk.InteractionRecord, 36)
	for i := range rows {
		rows[i] = sdk.InteractionRecord{
			ID: fmt.Sprint("work-", i), Sequence: uint64(i + 1), Kind: sdk.InteractionNotice,
			Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{
				Session: "ws", Plan: &text, Result: &text, EvidenceReadback: &text,
			}},
		}
	}
	k := newFakeKV()
	k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
		if op != "interaction.query" {
			t.Fatalf("recovery attempted %s", op)
		}
		var q sdk.InteractionQuery
		if err := json.Unmarshal(args, &q); err != nil {
			t.Fatal(err)
		}
		if q.Cursor != "" || q.Filter.WorkSession != "" && q.Filter.WorkSession != "ws" {
			t.Fatal("unexpected query", q)
		}
		selected, next := rows, ""
		if q.Filter.WorkSession == "" {
			selected = rows[len(rows)-1:]
		} else if len(selected) > q.Limit {
			selected = selected[len(selected)-q.Limit:]
			next = fmt.Sprint(selected[0].Sequence)
		}
		page := sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "fixture", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected, NextCursor: next}
		raw, err := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
		if err != nil {
			t.Fatal(err)
		}
		return raw, false
	}
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws", "call_id": "current-call"}
	response := h.call("plan", map[string]any{"view": "full"})
	var failure struct {
		Status     string `json:"status"`
		ReasonCode string `json:"reasonCode"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(response["result"], &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Status != "failed" || failure.ReasonCode != "REVIEW_RESULT_TOO_LARGE" {
		t.Fatalf("overflow lost its failure semantics: %+v", failure)
	}
	start, end := strings.Index(failure.Reason, "{"), strings.LastIndex(failure.Reason, "}")
	if !strings.Contains(failure.Reason, "call review") || start < 0 || end < start {
		t.Fatalf("overflow has no usable review route: %s", failure.Reason)
	}
	var recovery map[string]any
	if err := json.Unmarshal([]byte(failure.Reason[start:end+1]), &recovery); err != nil {
		t.Fatal(err)
	}
	recovery["view"] = "full"
	got := h.result("review", recovery)
	encoded, err := json.Marshal(got)
	if err != nil || len(encoded) > assessment.MaxReviewBytes {
		t.Fatal("advertised recovery did not fit the response budget", err, len(encoded))
	}
	coverage := got["coverage"].(map[string]any)
	if len(got["work_changes"].([]any)) != int(recovery["record_limit"].(float64)) || coverage["complete"] != false || k.puts != 0 {
		t.Fatal("recovery lost bounded evidence or wrote state", got, k.puts)
	}
}

func TestNativeCurrentMaterialUsesHostWorkWithoutRegistration(t *testing.T) {
	focus, plan := "inspect the requested module", "read, change and verify"
	rows := []sdk.InteractionRecord{
		{ID: "work", Sequence: 1, Kind: sdk.InteractionNotice, Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Focus: &focus, Plan: &plan}}},
		{ID: "own-call", Sequence: 2, Kind: sdk.InteractionToolCall, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "plan"}},
	}
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "denied"}[denied], func(t *testing.T) {
			k := newFakeKV()
			reads := 0
			k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
				if op != "interaction.query" {
					t.Fatalf("current material attempted %s", op)
				}
				reads++
				if denied {
					return denyBody("interaction grant absent"), true
				}
				var q sdk.InteractionQuery
				if err := json.Unmarshal(args, &q); err != nil {
					t.Fatal(err)
				}
				selected := rows
				if q.Filter.WorkSession == "" {
					selected = rows[1:]
				} else if q.Filter.WorkSession != "ws" || !q.Filter.IncludeDescendants {
					t.Fatal("lost current descendant scope", q)
				}
				page := sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "inc", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: "fixture", Availability: "complete", Rows: selected}
				raw, err := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
				if err != nil {
					t.Fatal(err)
				}
				return raw, false
			}
			h := startHarness(t, k)
			h.workSession = map[string]any{"id": "ws", "call_id": "own-call"}
			args := map[string]any{}
			if denied {
				if got := h.failure("plan", args); got != "POLICY_DENY" {
					t.Fatal(got)
				}
				return
			}
			for _, mode := range []string{"", "material", "status"} {
				if mode != "" {
					args["mode"] = mode
				}
				got := h.result("plan", args)
				work := got["current_work"].(map[string]any)
				fields := work["fields"].(map[string]any)
				if work["availability"] != "retained" || fields["focus"] != focus || fields["plan"] != plan {
					t.Fatal(got)
				}
				review := got["review"].(map[string]any)
				if review["own_call_exclusion"] != "host_record_excluded" || review["counts"].(map[string]any)["recorded_requests"] != float64(0) {
					t.Fatal(got)
				}
				if _, ok := got["plan_id"]; ok {
					t.Fatal("material registered a calibration plan", got)
				}
				b, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("PLAN_SCHEMA_PROOF %s", b)
			}
			if reads != 6 || k.puts != 0 || len(k.m) != 0 || len(k.files) != 0 {
				t.Fatal(reads, k.puts, k.m, k.files)
			}
			h.workSession = nil
			if code := h.failure("plan", map[string]any{}); code != "REVIEW_UNAVAILABLE" {
				t.Fatal(code)
			}
			h.workSession = map[string]any{"id": "ws", "call_id": "own-call"}
			for _, bad := range []any{nil, 42, true, ""} {
				if code := h.failure("plan", map[string]any{"plan_id": bad}); code != "OPERATION_ARGUMENT_INVALID" {
					t.Fatal(bad, code)
				}
			}
			if code := h.failure("plan", map[string]any{"mode": nil}); code != "OPERATION_ARGUMENT_INVALID" {
				t.Fatal(code)
			}
			if code := h.failure("plan", map[string]any{"_host_work_session": h.workSession}); code != "OPERATION_ARGUMENT_INVALID" {
				t.Fatal(code)
			}
		})
	}
}

func TestNativeBeginCanUseHostCallIDWithoutInventingDeclarations(t *testing.T) {
	k := newFakeKV()
	h := startHarness(t, k)
	host := map[string]any{"version": 1, "available": true, "session_id": "ws", "stream_id": "gen", "emitted": 0, "settled": 0, "loss_count": 0}
	work := map[string]any{"id": "ws", "call_id": "tool_call_example"}
	h.workSession = work
	args := map[string]any{"mode": "begin", "family": "checks", "declared_steps": 1, "budget_calls": 10, "falsifier_class": "failed", "expected_evidence_class": "passed", "consequence_class": "local", "_host_calibration": host}
	delete(args, "expected_evidence_class")
	if code := h.failure("plan", args); code != "OPERATION_ARGUMENT_INVALID" {
		t.Fatal(code)
	}
	if k.puts != 0 {
		t.Fatal("invalid declaration wrote state")
	}
	args["expected_evidence_class"] = "passed"
	work["id"] = "other"
	if code := h.failure("plan", args); code != "CALIBRATION_SESSION_REFUSED" {
		t.Fatal(code)
	}
	work["id"] = "ws"
	delete(work, "call_id")
	if code := h.failure("plan", args); code != "OPERATION_ARGUMENT_INVALID" {
		t.Fatal(code)
	}
	work["call_id"] = "tool_call_example"
	got := h.result("plan", args)
	if got["plan_id"] != "plan_tool_call_example" || got["session_id"] != "ws" {
		t.Fatal(got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PLAN_SCHEMA_PROOF %s", b)

	h.workSession = nil
	args["plan_id"] = "explicit"
	got = h.result("plan", args)
	if got["plan_id"] != "explicit" {
		t.Fatal(got)
	}
}
