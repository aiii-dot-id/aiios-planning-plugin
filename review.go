package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

const reviewSummary = "Read back what your earlier calls in one work session did and returned: what failed and what worked, current work, coverage and counts, with exact references. Pass a result_detail_request unchanged to read one earlier result whole. Omit work_session_id for the acting session; pass one for a delivered or abandoned session (work start and work deliver print it). Each reference can be copied unchanged into Memory store evidence. Compact by default; view=full returns every bounded attempt. A work_detail_requests entry, passed unchanged, reads one whole work field. Host success does not establish command or task correctness."

func registerReview(p *sdk.Plugin) {
	p.Describe("review", sdk.Descriptor{
		Summary: reviewSummary,
		Family:  "plan", Primary: true, Keywords: []string{"history", "work", "outcomes", "review"},
		Effects: sdk.EffectsReadInternal, Capabilities: []string{"interaction.read"},
		Input: "schemas/review_in.json", Output: "schemas/review_out.json", MaxResultBytes: assessment.MaxReviewBytes,
	})
	p.Handle("review", func(c sdk.Call) (any, error) {
		q, err := parseReviewContext(c.Arguments, c.WorkSession())
		if err != nil {
			return nil, reviewFailure(err)
		}
		result, err := assessment.Review(sdk.Interactions, q)
		if err != nil {
			return nil, reviewFailure(err)
		}
		raw, err := json.Marshal(result)
		return json.RawMessage(raw), err
	})
}

func parseReview(raw []byte) (assessment.ReviewRequest, error) {
	return parseReviewContext(raw, sdk.WorkSession{})
}

func parseReviewContext(raw []byte, context sdk.WorkSession) (assessment.ReviewRequest, error) {
	if sdk.ValidateStrict(raw) != nil {
		return assessment.ReviewRequest{}, assessment.InvalidReviewArgument("arguments")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return assessment.ReviewRequest{}, assessment.InvalidReviewArgument("arguments")
	}
	for k := range fields {
		if strings.HasPrefix(k, "_host") {
			delete(fields, k)
		}
	}
	q := assessment.DefaultReviewRequest(context.ID)
	q.ExcludeCallID = context.CallID
	_, explicitSession := fields["work_session_id"]
	q.IncludeDescendants = !explicitSession && context.ID != ""
	for k, v := range fields {
		if string(v) == "null" {
			return q, assessment.InvalidReviewArgument(k)
		}
		switch k {
		case "view":
			if json.Unmarshal(v, &q.View) != nil || (q.View != "compact" && q.View != "full") {
				return q, assessment.InvalidReviewArgument("view")
			}
		case "work_session_id":
			if json.Unmarshal(v, &q.WorkSession) != nil {
				return q, assessment.InvalidReviewArgument("work_session_id")
			}
		case "include_descendants":
			if string(v) != "true" && string(v) != "false" {
				return q, assessment.InvalidReviewArgument("include_descendants")
			}
			q.IncludeDescendants = string(v) == "true"
		case "read_limit":
			if !reviewInteger(v, &q.ReadLimit) {
				return q, assessment.InvalidReviewArgument("read_limit")
			}
		case "record_limit":
			if !reviewInteger(v, &q.RecordLimit) {
				return q, assessment.InvalidReviewArgument("record_limit")
			}
		case "detail_limit":
			if !reviewInteger(v, &q.DetailLimit) {
				return q, assessment.InvalidReviewArgument("detail_limit")
			}
		case "source_incarnation":
			if json.Unmarshal(v, &q.SourceIncarnation) != nil {
				return q, assessment.InvalidReviewArgument("source_incarnation")
			}
		case "work_field":
			if json.Unmarshal(v, &q.WorkField) != nil || q.WorkField == "" {
				return q, assessment.InvalidReviewArgument("work_field")
			}
		case "result_ref", "work_ref":
			var ref map[string]json.RawMessage
			var parsed assessment.Ref
			if json.Unmarshal(v, &ref) != nil || ref["id"] == nil || ref["sequence"] == nil || ref["sha256"] == nil || len(ref) > 4 || len(ref) == 4 && ref["incarnation"] == nil || json.Unmarshal(v, &parsed) != nil {
				return q, assessment.InvalidReviewArgument(k)
			}
			if k == "work_ref" {
				q.WorkRef = &parsed
			} else {
				q.ResultRef = &parsed
			}
		default:
			return q, assessment.InvalidReviewArgument("unknown")
		}
	}
	if q.WorkSession == "" && !explicitSession {
		return q, sdk.Fail("REVIEW_UNAVAILABLE", currentWorkRequirement(context.Reason))
	}
	return q, q.Validate()
}

func reviewInteger(raw []byte, out *int) bool {
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < 1 || n > 1<<20 || math.Trunc(n) != n {
		return false
	}
	*out = int(n)
	return true
}

func reviewFailure(err error) error {
	var denied *sdk.Denied
	var operation *sdk.OperationError
	if errors.As(err, &denied) || errors.As(err, &operation) {
		return err
	}
	code := "REVIEW_UNAVAILABLE"
	switch {
	case errors.Is(err, assessment.ErrInvalid):
		code = "OPERATION_ARGUMENT_INVALID"
	case errors.Is(err, assessment.ErrSourceChanged):
		code = "REVIEW_SOURCE_CHANGED"
	case errors.Is(err, assessment.ErrConflict):
		code = "REVIEW_EVIDENCE_CONFLICT"
	case errors.Is(err, assessment.ErrProtocol):
		code = "REVIEW_READER_INVALID"
	case errors.Is(err, assessment.ErrPageRefused):
		code = "REVIEW_PAGE_REFUSED"
	case errors.Is(err, assessment.ErrReviewTooLarge), errors.Is(err, assessment.ErrWorkFieldTooLarge):
		code = "REVIEW_RESULT_TOO_LARGE"
	case errors.Is(err, assessment.ErrResultUnavailable), errors.Is(err, assessment.ErrWorkFieldUnavailable):
		code = "REVIEW_RESULT_UNAVAILABLE"
	}
	return sdk.Fail(code, err.Error())
}
