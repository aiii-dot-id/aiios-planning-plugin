package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

func registerReview(p *sdk.Plugin) {
	p.Describe("review", sdk.Descriptor{
		Summary: "Review recorded tool calls, outcomes and evidence for the current work and its proven descendants. For a clipped result excerpt, call review with its result_detail_request to read bounded retained transcript text. Request text may be clipped: do not infer unseen arguments. A refusal is the host check's diagnostic, not proof of filesystem access. If it cannot distinguish inline program text from a path, use its suggested program-file route; do not claim an actual outside path was accessed. Does not verify task correctness or grade a plan.",
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
		return assessment.ReviewRequest{}, assessment.ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return assessment.ReviewRequest{}, assessment.ErrInvalid
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
			return q, assessment.ErrInvalid
		}
		switch k {
		case "work_session_id":
			if json.Unmarshal(v, &q.WorkSession) != nil {
				return q, assessment.ErrInvalid
			}
		case "include_descendants":
			if string(v) != "true" && string(v) != "false" {
				return q, assessment.ErrInvalid
			}
			q.IncludeDescendants = string(v) == "true"
		case "read_limit":
			if !reviewInteger(v, &q.ReadLimit) {
				return q, assessment.ErrInvalid
			}
		case "record_limit":
			if !reviewInteger(v, &q.RecordLimit) {
				return q, assessment.ErrInvalid
			}
		case "detail_limit":
			if !reviewInteger(v, &q.DetailLimit) {
				return q, assessment.ErrInvalid
			}
		case "source_incarnation":
			if json.Unmarshal(v, &q.SourceIncarnation) != nil {
				return q, assessment.ErrInvalid
			}
		case "result_ref":
			var ref map[string]json.RawMessage
			if json.Unmarshal(v, &ref) != nil || len(ref) != 3 || ref["id"] == nil || ref["sequence"] == nil || ref["sha256"] == nil || json.Unmarshal(v, &q.ResultRef) != nil {
				return q, assessment.ErrInvalid
			}
		default:
			return q, assessment.ErrInvalid
		}
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
	case errors.Is(err, assessment.ErrReviewTooLarge):
		code = "REVIEW_RESULT_TOO_LARGE"
	case errors.Is(err, assessment.ErrResultUnavailable):
		code = "REVIEW_RESULT_UNAVAILABLE"
	}
	return sdk.Fail(code, err.Error())
}
