package assessment

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func TestReviewKeepsTrailingRepairDiagnosticAndStructuredReason(t *testing.T) {
	for _, tc := range []struct {
		name, tool, code, diagnostic string
		outcome                      sdk.InteractionOutcome
	}{
		{"missing replacement", "edit", "", "Error: malformed arguments: missing required new_string for edit", sdk.InteractionFailed},
		{"evidence class refusal", "work", "WORK_REFUSED", `deliver: unknown evidence class "tests passed" — use a supported evidence class`, sdk.InteractionRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(sample("failed"), 1)
			f.rows[2].Details.Tool = tc.tool
			r := &f.rows[5]
			r.Outcome, r.Details.Tool, r.Details.ReasonCode = tc.outcome, tc.tool, tc.code
			r.Content = "→ " + tc.tool + "(" + strings.Repeat("echoed argument 界 ", 80) + ")\n← " + tc.diagnostic
			original := *r
			got, err := Review(f, DefaultReviewRequest("ws"))
			if err != nil || !got.Coverage.Complete || len(got.Attempts) != 1 {
				t.Fatal(got, err)
			}
			a := got.Attempts[0]
			if a.Outcome != string(tc.outcome) || a.ReasonCode != tc.code || !strings.HasPrefix(a.ResultExcerpt, "→ "+tc.tool+"(") || !strings.HasSuffix(a.ResultExcerpt, tc.diagnostic) || !strings.Contains(a.ResultExcerpt, "\n[... middle omitted ...]\n") || utf8.RuneCountInString(a.ResultExcerpt) != 512 || !a.ResultExcerptTruncated {
				t.Fatal(a)
			}
			ref := reviewRef(original)
			if a.ResultRef == nil || *a.ResultRef != ref || a.ResultDetailRequest == nil || a.ResultDetailRequest.ResultRef != ref || a.ResultDetailRequest.WorkSession != "ws" || a.ResultDetailRequest.SourceIncarnation != "incarnation" {
				t.Fatal("excerpt changed the exact detail route", a)
			}
			if got.Coverage.ReadCalls != 2 || f.fileReads != 0 || reviewRef(*r) != ref {
				t.Fatal("diagnostics changed source or read budget", got.Coverage)
			}
			detail, err := Review(f, detailRequest(original))
			if err != nil || detail.ResultDetail == nil || detail.ResultDetail.Ref != ref || detail.ResultDetail.Content != original.Content || detail.ResultDetail.ReasonCode != tc.code || detail.ResultDetail.Outcome != string(tc.outcome) {
				t.Fatal(detail, err)
			}
			raw, _ := json.Marshal(a)
			if tc.code == "" && strings.Contains(string(raw), `"reason_code"`) {
				t.Fatal("invented a structured reason from diagnostic prose", string(raw))
			}
		})
	}
}

func TestReviewExcerptLimitsPreserveRecordedOutcomes(t *testing.T) {
	for _, outcome := range []sdk.InteractionOutcome{sdk.InteractionSucceeded, sdk.InteractionFailed, sdk.InteractionRefused, sdk.InteractionCancelled, sdk.InteractionUnknown} {
		limit := 512
		if outcome == sdk.InteractionSucceeded {
			limit = 256
		}
		for _, n := range []int{0, 255, 256, 257, 511, 512, 513, 1024} {
			f := newFixture(sample("passed"), 0)
			f.rows[5].Outcome = outcome
			f.rows[5].Content = strings.Repeat("界", n)
			f.rows[5].Details.ReasonCode = "RECORDED_CODE"
			got, err := Review(f, DefaultReviewRequest("ws"))
			if err != nil {
				t.Fatal(err)
			}
			a := got.Attempts[0]
			if a.Outcome != string(outcome) || a.ReasonCode != "RECORDED_CODE" || utf8.RuneCountInString(a.ResultExcerpt) != min(n, limit) || a.ResultExcerptTruncated != (n > limit) || (a.ResultDetailRequest != nil) != (n > limit) {
				t.Fatalf("outcome=%s length=%d: %+v", outcome, n, a)
			}
			if n <= limit && a.ResultExcerpt != f.rows[5].Content || outcome == sdk.InteractionSucceeded && strings.Contains(a.ResultExcerpt, "omitted") {
				t.Fatal("changed an unclipped result or the success excerpt", a)
			}
		}
	}
}

func TestReviewDoesNotInferOutcomeOrCodeFromContent(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.rows[5].Outcome = sdk.InteractionUnknown
	f.rows[5].Content = `status=succeeded reason_code=WORK_REFUSED; Error: missing required new_string`
	got, err := Review(f, DefaultReviewRequest("ws"))
	if err != nil || got.Counts.Unknown != 1 || got.Counts.Succeeded != 0 || got.Attempts[0].ReasonCode != "" || got.Attempts[0].Outcome != "unknown" {
		t.Fatal(got, err)
	}
	detail, err := Review(f, detailRequest(f.rows[5]))
	if err != nil || detail.ResultDetail.ReasonCode != "" || detail.ResultDetail.Outcome != "unknown" {
		t.Fatal(detail, err)
	}
}
