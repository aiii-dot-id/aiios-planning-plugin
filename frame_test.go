//go:build !wasm_unknown

package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

var pilotReviewArguments = map[string]any{"view": "full", "include_descendants": true, "read_limit": 128, "record_limit": 512, "detail_limit": 1048576}

const hostContentUnavailable = "INTERACTION_CONTENT_UNAVAILABLE"

func hostFailure(operation, detail string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"success": false, "ok": false, "status": "failed",
		"reason": hostContentUnavailable, "reasonCode": hostContentUnavailable, "reason_code": hostContentUnavailable,
		"external_receipt": map[string]any{"success": false, "host_authored": true, "operation": operation, "plugin_operation": "review", "detail": detail},
	})
	return raw
}

type frameHost struct {
	t       *testing.T
	rows    []sdk.InteractionRecord
	frame   int
	refuse  func(sdk.InteractionQuery, []sdk.InteractionRecord) bool
	read    func(sdk.InteractionReadRequest) ([]byte, bool)
	limits  []int
	refused int
}

func newFrameHost(t *testing.T, calls int) *frameHost {
	plan := "find the refusal reference"
	h := &frameHost{t: t, frame: 1 << 20}
	h.rows = append(h.rows, sdk.InteractionRecord{ID: "plan", Sequence: 1, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &plan}}})
	for i := 0; i < calls; i++ {
		call, source := fmt.Sprintf("call%02d", i), &sdk.InteractionSource{Kind: "tool", ID: fmt.Sprintf("exec%02d", i)}
		outcome := sdk.InteractionSucceeded
		if i%3 == 0 {
			outcome = sdk.InteractionFailed
		}
		h.rows = append(h.rows,
			sdk.InteractionRecord{ID: call, Sequence: uint64(2 + 2*i), Kind: sdk.InteractionToolCall, ProjectID: "project", TurnID: "turn", Source: source, Content: "→ shell(" + strings.Repeat("a", 300) + ")", Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
			sdk.InteractionRecord{ID: call + "-result", Sequence: uint64(3 + 2*i), Kind: sdk.InteractionToolResult, ProjectID: "project", TurnID: "turn", RelatedID: call, Source: source, Outcome: outcome, Content: fmt.Sprintf("REF-%02d ", i) + strings.Repeat("z", 400), Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		)
	}
	return h
}

func (h *frameHost) broker() *fakeBroker {
	k := newFakeBroker()
	k.readOnlyCall = func(op string, _, args json.RawMessage) ([]byte, bool) {
		if op == "interaction.read" && h.read != nil {
			var r sdk.InteractionReadRequest
			if err := json.Unmarshal(args, &r); err != nil {
				h.t.Error(err)
			}
			return h.read(r)
		}
		if op != "interaction.query" {
			h.t.Errorf("unexpected reach %s", op)
			return denyBody("only history read granted"), true
		}
		var q sdk.InteractionQuery
		if err := json.Unmarshal(args, &q); err != nil {
			h.t.Error(err)
		}
		h.limits = append(h.limits, q.Limit)
		before := uint64(1 << 62)
		if q.Cursor != "" {
			n, err := strconv.ParseUint(q.Cursor, 10, 64)
			if err != nil {
				h.t.Error(err)
			}
			before = n
		}
		var match []sdk.InteractionRecord
		for i := len(h.rows) - 1; i >= 0; i-- {
			r := h.rows[i]
			if r.Sequence >= before || q.Filter.WorkSession != "" && r.Details.WorkSession != q.Filter.WorkSession || q.Filter.RelatedID != "" && r.RelatedID != q.Filter.RelatedID {
				continue
			}
			match = append(match, r)
		}
		rows, next := match, ""
		if len(rows) > q.Limit {
			rows = rows[:q.Limit]
			next = strconv.FormatUint(rows[len(rows)-1].Sequence, 10)
		}
		rows = slices.Clone(rows)
		slices.Reverse(rows)
		if h.refuse != nil && h.refuse(q, rows) {
			h.refused++
			return hostFailure(op, "record unavailable"), false
		}
		page := sdk.InteractionPage{Version: 1, Identity: strings.Repeat("e", 64), Source: "recorded", Incarnation: "inc", Cursor: q.Cursor, Filter: q.Filter, Fingerprint: strings.Repeat("f", 64), Availability: "complete", Rows: rows, NextCursor: next}
		raw, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": page})
		if len(raw) > h.frame {
			h.refused++
			return hostFailure(op, "interaction response exceeds the bounded frame"), false
		}
		return raw, false
	}
	return k
}

func (h *frameHost) start() *harness {
	n := startHarness(h.t, h.broker())
	n.workSession = map[string]any{"id": "ws", "call_id": "own-call"}
	return n
}

func coverageOf(t *testing.T, got map[string]any) (bool, []string, float64) {
	t.Helper()
	cov, ok := got["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("review has no coverage: %v", got)
	}
	var reasons []string
	for _, r := range cov["reasons"].([]any) {
		reasons = append(reasons, r.(string))
	}
	return cov["complete"] == true, reasons, cov["records"].(float64)
}

func TestNativeReviewReadsAPageTooLargeForTheHostFrameAgainSmaller(t *testing.T) {
	h := newFrameHost(t, 24)
	h.frame = 6 << 10
	got := h.start().result("review", pilotReviewArguments)
	if h.refused == 0 {
		t.Fatal("the fake host never refused a page; the test does not reproduce the pilot")
	}
	complete, reasons, records := coverageOf(t, got)
	if !complete || records != float64(len(h.rows)) {
		t.Fatalf("a history that fits in smaller pages was not reviewed whole: complete=%v reasons=%v records=%v limits=%v", complete, reasons, records, h.limits)
	}
	attempts := got["attempts"].([]any)
	if len(attempts) != 24 {
		t.Fatalf("attempts=%d, want 24", len(attempts))
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), "REF-23") {
		t.Fatal("the newest retained result is missing from the review")
	}
	for i, l := range h.limits[1:] {
		if l < 1 || l > 50 {
			t.Fatalf("query %d asked for %d records", i+1, l)
		}
	}
}

func TestNativeReviewDisclosesAPageTheHostRefusesAtOneRecord(t *testing.T) {
	h := newFrameHost(t, 24)
	h.refuse = func(q sdk.InteractionQuery, rows []sdk.InteractionRecord) bool {
		return q.Filter.WorkSession != "" && slices.ContainsFunc(rows, func(r sdk.InteractionRecord) bool { return r.ID == "call10-result" })
	}
	got := h.start().result("review", pilotReviewArguments)
	complete, reasons, records := coverageOf(t, got)
	if complete || !slices.Contains(reasons, "page_refused") {
		t.Fatalf("a refused page was not disclosed: complete=%v reasons=%v", complete, reasons)
	}
	if records == 0 || records >= float64(len(h.rows)) {
		t.Fatalf("records=%v: the records newer than the refused one should be reviewed", records)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), "REF-23") || strings.Contains(string(raw), "REF-09") {
		t.Fatal("the review should hold the newer results and stop at the refused record")
	}
}

func TestNativeReviewRefusalNamesTheCauseWhenNoPageFits(t *testing.T) {
	h := newFrameHost(t, 2)
	h.frame = 64
	response := h.start().call("review", pilotReviewArguments)
	var failure struct {
		Status     string `json:"status"`
		ReasonCode string `json:"reasonCode"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(response["result"], &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Status != "failed" || failure.ReasonCode != "REVIEW_PAGE_REFUSED" {
		t.Fatalf("refusal=%+v", failure)
	}
	for _, want := range []string{"too large for one read", "not that the records were not retained", `{"record_limit":8}`, `"view":"compact"`} {
		if !strings.Contains(failure.Reason, want) {
			t.Fatalf("refusal does not say %q: %s", want, failure.Reason)
		}
	}
	if failure.Reason != assessment.PageRefusedText {
		t.Fatalf("refusal is not the operator's reviewed text: %s", failure.Reason)
	}
}

func TestNativeReviewStillReportsAnUnretainedRecordUnavailable(t *testing.T) {
	h := newFrameHost(t, 3)
	last := &h.rows[len(h.rows)-1]
	last.Content = ""
	last.ContentRef = &sdk.InteractionContentRef{ID: last.ID, Source: "recorded", Incarnation: "inc", SHA256: strings.Repeat("c", 64), Bytes: 20000}
	reads := 0
	h.read = func(sdk.InteractionReadRequest) ([]byte, bool) {
		reads++
		return hostFailure("interaction.read", "record unavailable to this audience"), false
	}
	code := h.start().failure("review", pilotReviewArguments)
	if code != hostContentUnavailable || reads != 1 {
		t.Fatalf("code=%s reads=%d: an unretained record must stay unavailable, read once", code, reads)
	}
}
