//go:build !wasm_unknown

package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiospkg"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/t3"
)

func contractBegin() map[string]any {
	return map[string]any{"mode": "begin", "plan_id": "p", "family": "checks", "falsifier_class": "failed", "expected_evidence_class": "passed", "consequence_class": "local", "declared_steps": 1, "budget_calls": 10,
		"_host_calibration": map[string]any{"version": 1, "available": true, "session_id": "ws", "stream_id": "gen", "emitted": 0, "settled": 0, "loss_count": 0}}
}

func contractFailure(t *testing.T, h *harness, operation string, args map[string]any, code, detail string) {
	t.Helper()
	obj := h.call(operation, args)
	var result struct {
		Status string `json:"status"`
		Code   string `json:"reasonCode"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(obj["result"], &result); err != nil {
		t.Fatalf("decode %s: %v; %s", operation, err, obj["result"])
	}
	if result.Status != "failed" || result.Code != code || !strings.Contains(result.Reason, detail) {
		t.Fatalf("%s: got %+v, want %s with %q", operation, result, code, detail)
	}
	h.kv.mu.Lock()
	defer h.kv.mu.Unlock()
	if h.kv.puts != 0 || len(h.kv.m) != 0 || len(h.kv.files) != 0 {
		t.Fatal("refusal changed persistent state")
	}
}

func TestNativeBeginNamesTheInvalidFieldBeforeWriting(t *testing.T) {
	k := newFakeKV()
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws", "call_id": "call", "reason": ""}
	for _, tc := range []struct {
		name, field string
		value       any
		absent      bool
		detail      string
	}{
		{name: "prose", field: "falsifier_class", value: "test fails", detail: "falsifier_class must be 1..96 ASCII"},
		{name: "class boundary", field: "expected_evidence_class", value: strings.Repeat("x", 97), detail: "expected_evidence_class must be 1..96 ASCII"},
		{name: "unicode", field: "consequence_class", value: "résultat", detail: "consequence_class must be 1..96 ASCII"},
		{name: "family", field: "family", value: "with spaces", detail: "family must be 1..128 ASCII"},
		{name: "id", field: "plan_id", value: "with spaces", detail: "plan_id must be 1..128 ASCII"},
		{name: "missing", field: "falsifier_class", absent: true, detail: "begin requires falsifier_class"},
		{name: "wrong type", field: "family", value: 12, detail: "begin requires family"},
		{name: "null", field: "budget_calls", value: nil, detail: "budget_calls must not be null"},
		{name: "fractional", field: "declared_steps", value: 1.5, detail: "declared_steps"},
		{name: "steps boundary", field: "declared_steps", value: 257, detail: "declared_steps must be an integer from 1 to 256"},
		{name: "calls boundary", field: "budget_calls", value: 100001, detail: "budget_calls must be an integer from 1 to 100000"},
		{name: "wrong mode field", field: "count", value: 1, detail: `field "count" is not accepted by mode "begin"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := contractBegin()
			if tc.absent {
				delete(args, tc.field)
			} else {
				args[tc.field] = tc.value
			}
			contractFailure(t, h, "plan", args, "OPERATION_ARGUMENT_INVALID", tc.detail)
		})
	}
}

func TestNativeMissingWorkIsActionableWithoutOpeningWork(t *testing.T) {
	for _, reason := range []string{"no_session", "session_lookup_failed"} {
		t.Run(reason, func(t *testing.T) {
			k := newFakeKV()
			h := startHarness(t, k)
			h.workSession = map[string]any{"id": "", "call_id": "call", "reason": reason}
			detail := "core work action=start"
			if reason == "session_lookup_failed" {
				detail = "resolve that lookup failure before retrying"
			}
			args := contractBegin()
			args["_host_calibration"] = map[string]any{"version": 1, "available": false, "reason": reason}
			contractFailure(t, h, "plan", args, "CALIBRATION_SESSION_REFUSED", detail)
			contractFailure(t, h, "review", map[string]any{}, "REVIEW_UNAVAILABLE", detail)
			for _, mode := range []string{"material", "status"} {
				contractFailure(t, h, "plan", map[string]any{"mode": mode}, "REVIEW_UNAVAILABLE", detail)
			}
		})
	}
}

func TestNativeBeginAcceptsDocumentedBoundariesAndPreservesMaterial(t *testing.T) {
	k := newFakeKV()
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws", "call_id": "call", "reason": ""}
	args := contractBegin()
	args["plan_id"] = strings.Repeat("p", 128)
	args["family"] = strings.Repeat("f", 128)
	for _, key := range []string{"falsifier_class", "expected_evidence_class", "consequence_class"} {
		args[key] = strings.Repeat("x", 92) + "._:-"
	}
	args["declared_steps"] = 256
	args["budget_calls"] = 100000
	args["session_id"] = "ws"
	got := h.result("plan", args)
	for _, key := range []string{"plan_id", "family", "falsifier_class", "expected_evidence_class", "consequence_class", "session_id"} {
		if got[key] != args[key] {
			t.Fatalf("%s changed: %v", key, got[key])
		}
	}
	if got["declared_steps"] != float64(256) || got["budget_calls"] != float64(100000) {
		t.Fatal(got)
	}
	before, _ := json.Marshal(got)
	for _, mode := range []string{"material", "status"} {
		after, _ := json.Marshal(h.result("plan", map[string]any{"mode": mode, "plan_id": args["plan_id"], "_host_calibration": args["_host_calibration"]}))
		if string(after) != string(before) {
			t.Fatalf("%s changed the accepted record", mode)
		}
	}
}

func TestBeginSchemaAndRuntimeAgreeAtTokenAndCountBoundaries(t *testing.T) {
	raw, err := os.ReadFile("schemas/plan_in.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = aiiospkg.CheckSchemaSubset(raw); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Pattern                                string
			MinLength, MaxLength, Minimum, Maximum int
			Description                            string
		}
	}
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	valid := func() t3.Plan {
		return t3.Plan{ID: "p", Family: "checks", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 10}
	}
	for _, field := range []struct {
		name string
		set  func(*t3.Plan, string)
	}{
		{"plan_id", func(p *t3.Plan, s string) { p.ID = s }}, {"family", func(p *t3.Plan, s string) { p.Family = s }}, {"session_id", func(p *t3.Plan, s string) { p.SessionID = s }},
		{"falsifier_class", func(p *t3.Plan, s string) { p.FalsifierClass = s }}, {"expected_evidence_class", func(p *t3.Plan, s string) { p.ExpectedEvidenceClass = s }}, {"consequence_class", func(p *t3.Plan, s string) { p.ConsequenceClass = s }},
	} {
		s := schema.Properties[field.name]
		rx, err := regexp.Compile(s.Pattern)
		if err != nil || s.Pattern == "" {
			t.Fatalf("%s pattern: %q %v", field.name, s.Pattern, err)
		}
		for _, v := range []string{"", "A0._:-", "a b", "a/b", "é", "x\n", strings.Repeat("x", 96), strings.Repeat("x", 97), strings.Repeat("x", 128), strings.Repeat("x", 129)} {
			p := valid()
			field.set(&p, v)
			advertised := len([]rune(v)) >= s.MinLength && len([]rune(v)) <= s.MaxLength && rx.MatchString(v)
			if accepted := t3.ValidBegin(&p) == nil; accepted != advertised {
				t.Fatalf("%s length%d: schema=%v runtime=%v", field.name, len(v), advertised, accepted)
			}
		}
	}
	for _, field := range []string{"declared_steps", "budget_calls"} {
		s := schema.Properties[field]
		for _, v := range []int{0, 1, 256, 257, 100000, 100001} {
			p := valid()
			if field == "declared_steps" {
				p.DeclaredSteps = v
			} else {
				p.BudgetCalls = v
			}
			if (t3.ValidBegin(&p) == nil) != (v >= s.Minimum && v <= s.Maximum) {
				t.Fatalf("%s=%d schema disagrees", field, v)
			}
		}
	}
	if !strings.Contains(schema.Properties["mode"].Description, "begin registers optional calibration") {
		t.Fatal("schema hides begin's role and requirements")
	}
}

func TestNativeRegisteredPlanRemainsReadableWithoutCurrentWork(t *testing.T) {
	k := newFakeKV()
	h := startHarness(t, k)
	h.workSession = map[string]any{"id": "ws", "call_id": "call", "reason": ""}
	registered := h.result("plan", contractBegin())
	if registered["plan_id"] != "p" || registered["session_id"] != "ws" {
		t.Fatal(registered)
	}
	k.mu.Lock()
	before, err := json.Marshal(k.m)
	puts := k.puts
	k.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"no_session", "session_lookup_failed"} {
		t.Run(reason, func(t *testing.T) {
			h.workSession = map[string]any{"id": "", "call_id": "read-call", "reason": reason}
			for _, mode := range []string{"material", "status"} {
				got := h.result("plan", map[string]any{
					"mode": mode, "plan_id": "p",
					"_host_calibration": map[string]any{"version": 1, "available": false, "reason": reason},
				})
				if got["plan_id"] != "p" || got["session_id"] != "ws" || got["host_calibration"] != "unresolved" {
					t.Fatalf("%s lost the retained plan or invented current calibration: %v", mode, got)
				}
				for _, key := range []string{"family", "falsifier_class", "expected_evidence_class", "consequence_class", "declared_steps", "budget_calls", "state"} {
					if got[key] != registered[key] {
						t.Fatalf("%s changed %s", mode, key)
					}
				}
			}
		})
	}
	k.mu.Lock()
	after, err := json.Marshal(k.m)
	if err != nil || string(after) != string(before) || k.puts != puts || len(k.files) != 0 {
		t.Error("reading a retained plan changed persistent state", err)
	}
	k.mu.Unlock()
}
