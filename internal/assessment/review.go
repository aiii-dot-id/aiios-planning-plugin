package assessment

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

const MaxReviewBytes = 60 << 10
const MaxResultDetailRunes = 8192

var ErrReviewTooLarge = errors.New(`assessment: review exceeds response budget; call review with a lower record_limit (for example, {"record_limit":8})`)
var ErrResultUnavailable = errors.New("assessment: result not available in the selected work and read limits; use the original work_session_id and source_incarnation, or increase bounded record_limit/read_limit/detail_limit and review again")
var ErrWorkFieldUnavailable = errors.New("assessment: work field not available in the selected retained notice and read limits; use its original work session, scope, source incarnation and reference")
var ErrWorkFieldTooLarge = errors.New("assessment: the whole retained work field exceeds the 60 KiB encoded response budget; it was not clipped, and lowering record_limit cannot shrink this field")

type ResultSelection struct {
	WorkSession       string `json:"work_session_id"`
	SourceIncarnation string `json:"source_incarnation"`
	ResultRef         Ref    `json:"result_ref"`
	ReadLimit         int    `json:"read_limit"`
	RecordLimit       int    `json:"record_limit"`
	DetailLimit       int    `json:"detail_limit"`
}

type WorkSelection struct {
	WorkSession        string `json:"work_session_id"`
	IncludeDescendants bool   `json:"include_descendants"`
	SourceIncarnation  string `json:"source_incarnation"`
	WorkRef            Ref    `json:"work_ref"`
	WorkField          string `json:"work_field"`
	ReadLimit          int    `json:"read_limit"`
	RecordLimit        int    `json:"record_limit"`
	DetailLimit        int    `json:"detail_limit"`
}

type WorkDetail struct {
	Ref               Ref    `json:"ref"`
	SourceIncarnation string `json:"source_incarnation"`
	WorkSession       string `json:"work_session_id"`
	Field             string `json:"field"`
	Value             string `json:"value"`
}

type ResultDetail struct {
	Ref                Ref      `json:"ref"`
	SourceIncarnation  string   `json:"source_incarnation"`
	WorkSession        string   `json:"work_session_id"`
	Content            string   `json:"content"`
	ContentTruncated   bool     `json:"content_truncated"`
	Outcome            string   `json:"outcome"`
	ReasonCode         string   `json:"reason_code,omitempty"`
	ResultRecord       string   `json:"result_record"`
	ExecutionTruncated bool     `json:"execution_truncated"`
	Omissions          []string `json:"omissions"`
}

type ReviewRequest struct {
	View               string `json:"view,omitempty"`
	IncludeDescendants bool   `json:"include_descendants,omitempty"`
	ExcludeCallID      string `json:"-"`
	WorkSession        string `json:"work_session_id"`
	ReadLimit          int    `json:"read_limit"`
	RecordLimit        int    `json:"record_limit"`
	DetailLimit        int    `json:"detail_limit"`
	ResultRef          *Ref   `json:"result_ref,omitempty"`
	WorkRef            *Ref   `json:"work_ref,omitempty"`
	WorkField          string `json:"work_field,omitempty"`
	SourceIncarnation  string `json:"source_incarnation,omitempty"`
}

func DefaultReviewRequest(session string) ReviewRequest {
	return ReviewRequest{View: "compact", WorkSession: session, ReadLimit: 32, RecordLimit: 128, DetailLimit: 256 << 10}
}

func InvalidReviewArgument(field string) error {
	var requirement string
	switch field {
	case "arguments":
		requirement = "arguments must be a JSON object with unique keys"
	case "view":
		requirement = `view must be "compact" or "full"`
	case "work_session_id":
		requirement = "work_session_id must name an existing session using 1..128 ASCII letters, digits, underscores, hyphens, periods or colons; omit it only when the host supplies current work"
	case "include_descendants":
		requirement = "include_descendants must be a JSON boolean, true or false"
	case "read_limit":
		requirement = "read_limit must be an integer from 1 to 128"
	case "record_limit":
		requirement = "record_limit must be an integer from 1 to 512"
	case "detail_limit":
		requirement = "detail_limit must be an integer from 1 to 1048576 bytes"
	case "source_incarnation":
		requirement = "source_incarnation must be a string of at most 256 bytes, non-empty exactly when result_ref or work_ref is supplied; copy the original detail request unchanged"
	case "result_ref", "work_ref":
		requirement = "result_ref and work_ref must each contain only id (a non-empty string of at most 256 bytes), sequence (a positive decimal string), sha256 (64 lowercase hexadecimal characters) and optionally incarnation (equal to source_incarnation); copy the original result_detail_request or work_detail_requests entry unchanged"
	case "reference_selection":
		requirement = "result_ref and work_ref are mutually exclusive; copy just one original detail request unchanged"
	case "work_field_pair":
		requirement = "work_ref and work_field must be supplied together; copy one original work_detail_requests entry unchanged"
	case "work_field":
		requirement = "work_field must be state, focus, next_move, plan, expected_evidence, falsifier, decision_needed, result, evidence or evidence_readback"
	default:
		requirement = "unsupported review argument; use only the documented work_session_id, view, include_descendants, read_limit, record_limit, detail_limit, source_incarnation, result_ref, work_ref and work_field fields"
	}
	return fmt.Errorf("%w: %s. Example with an acting work session: {\"view\":\"compact\"}; otherwise supply an existing work_session_id", ErrInvalid, requirement)
}

func (r *Ref) in(incarnation string) *Ref {
	if r == nil {
		return nil
	}
	v := *r
	v.Incarnation = incarnation
	return &v
}

func (q ReviewRequest) Validate() error {
	if q.View != "" && q.View != "compact" && q.View != "full" {
		return InvalidReviewArgument("view")
	}
	if q.ResultRef != nil && q.WorkRef != nil {
		return InvalidReviewArgument("reference_selection")
	}
	if (q.WorkRef == nil) != (q.WorkField == "") {
		return InvalidReviewArgument("work_field_pair")
	}
	selected := q.ResultRef != nil || q.WorkRef != nil
	if selected != (q.SourceIncarnation != "") || len(q.SourceIncarnation) > 256 {
		return InvalidReviewArgument("source_incarnation")
	}
	for i, r := range []*Ref{q.ResultRef, q.WorkRef} {
		if r != nil && (r.ID == "" || len(r.ID) > 256 || r.Sequence == 0 || !validHash(r.SHA256) || r.Incarnation != "" && r.Incarnation != q.SourceIncarnation) {
			return InvalidReviewArgument([]string{"result_ref", "work_ref"}[i])
		}
	}
	if q.WorkRef != nil {
		var work sdk.InteractionWorkChange
		if workField(&work, q.WorkField) == nil {
			return InvalidReviewArgument("work_field")
		}
	}
	if len(q.WorkSession) == 0 || len(q.WorkSession) > 128 {
		return InvalidReviewArgument("work_session_id")
	}
	if q.ReadLimit < 1 || q.ReadLimit > 128 {
		return InvalidReviewArgument("read_limit")
	}
	if q.RecordLimit < 1 || q.RecordLimit > 512 {
		return InvalidReviewArgument("record_limit")
	}
	if q.DetailLimit < 1 || q.DetailLimit > 1<<20 {
		return InvalidReviewArgument("detail_limit")
	}
	for _, r := range q.WorkSession {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':') {
			return InvalidReviewArgument("work_session_id")
		}
	}
	return nil
}

type Counts struct {
	RecordedRequests int `json:"recorded_requests"`
	Succeeded        int `json:"succeeded"`
	Failed           int `json:"failed"`
	Refused          int `json:"refused"`
	Cancelled        int `json:"cancelled"`
	Unknown          int `json:"unknown"`
}

type WorkFact struct {
	Ref                Ref                       `json:"ref"`
	ProjectID          string                    `json:"project_id"`
	TurnID             string                    `json:"turn_id"`
	Change             sdk.InteractionWorkChange `json:"change"`
	TruncatedFields    []string                  `json:"truncated_fields"`
	WorkDetailRequests []WorkSelection           `json:"work_detail_requests,omitempty"`
}

type ReviewResult struct {
	Request           ReviewRequest `json:"-"`
	OwnCallExclusion  string        `json:"own_call_exclusion"`
	ExcludedCallID    string        `json:"excluded_call_id,omitempty"`
	Evidence          []Ref         `json:"evidence"`
	Identity          string        `json:"identity"`
	WorkSession       string        `json:"work_session_id"`
	SourceIncarnation string        `json:"source_incarnation"`
	Scope             string        `json:"scope"`
	CountBasis        string        `json:"count_basis"`
	Coverage          Coverage      `json:"coverage"`
	Counts            *Counts       `json:"counts"`
	Attempts          []Attempt     `json:"attempts"`
	UnmatchedResults  []Ref         `json:"unmatched_results"`
	WorkChanges       []WorkFact    `json:"work_changes"`
	ResultDetail      *ResultDetail `json:"result_detail,omitempty"`
	WorkDetail        *WorkDetail   `json:"work_detail,omitempty"`
}

func (r ReviewResult) MarshalJSON() ([]byte, error) {
	if r.WorkDetail != nil {
		return json.Marshal(struct {
			Identity          string      `json:"identity"`
			WorkSession       string      `json:"work_session_id"`
			SourceIncarnation string      `json:"source_incarnation"`
			Scope             string      `json:"scope"`
			Coverage          Coverage    `json:"coverage"`
			WorkDetail        *WorkDetail `json:"work_detail"`
		}{r.Identity, r.WorkSession, r.SourceIncarnation, r.Scope, r.Coverage, r.WorkDetail})
	}
	if r.ResultDetail != nil {
		return json.Marshal(struct {
			Identity          string        `json:"identity"`
			WorkSession       string        `json:"work_session_id"`
			SourceIncarnation string        `json:"source_incarnation"`
			Scope             string        `json:"scope"`
			Coverage          Coverage      `json:"coverage"`
			ResultDetail      *ResultDetail `json:"result_detail"`
		}{r.Identity, r.WorkSession, r.SourceIncarnation, r.Scope, r.Coverage, r.ResultDetail})
	}
	if r.Request.View == "compact" {
		return json.Marshal(r.Compact())
	}
	type aggregate ReviewResult
	return json.Marshal(aggregate(r))
}

func Review(reader Reader, q ReviewRequest) (ReviewResult, error) {
	if err := q.Validate(); err != nil {
		return ReviewResult{}, err
	}
	if reader == nil {
		return ReviewResult{}, ErrInvalid
	}

	q.ResultRef, q.WorkRef = q.ResultRef.in(q.SourceIncarnation), q.WorkRef.in(q.SourceIncarnation)
	c := &collection{
		sessionOnly: true, includeDescendants: q.IncludeDescendants, excludeCallID: q.ExcludeCallID, selected: map[string]bool{},
		q: Request{WorkSession: q.WorkSession, End: Ref{Sequence: math.MaxInt64}, Limits: Limits{ReadCalls: q.ReadLimit, Records: q.RecordLimit, DetailBytes: q.DetailLimit}},
		r: reader, seen: map[string]sdk.InteractionRecord{}, seq: map[uint64]string{},
		out: Result{Coverage: Coverage{Complete: true, Reasons: []string{}}, Attempts: []Attempt{}},
	}
	if err := c.collect(); err != nil {
		return ReviewResult{}, err
	}
	if (q.ResultRef != nil || q.WorkRef != nil) && q.SourceIncarnation != c.out.SourceIncarnation {
		return ReviewResult{}, ErrSourceChanged
	}
	out := ReviewResult{Request: q, Identity: c.q.Identity, WorkSession: q.WorkSession, SourceIncarnation: c.out.SourceIncarnation, Scope: "retained_session_without_descendants", CountBasis: "retained_recorded_requests", Attempts: c.out.Attempts, UnmatchedResults: []Ref{}, WorkChanges: []WorkFact{}}
	out.OwnCallExclusion = "unavailable"
	out.Evidence = []Ref{}
	if q.ExcludeCallID != "" {
		out.OwnCallExclusion = "host_record_excluded"
		out.ExcludedCallID = q.ExcludeCallID
	}
	if q.IncludeDescendants {
		out.Scope = "retained_session_and_proven_descendants"
	}
	matched := false
	for _, r := range c.seen {
		if !c.selected[r.ID] {
			continue
		}
		matched = true
		out.Evidence = append(out.Evidence, c.ref(r))
		if r.Kind == sdk.InteractionToolResult {
			parent, ok := c.seen[r.RelatedID]
			if !ok || parent.Kind != sdk.InteractionToolCall || parent.Details.WorkSession != r.Details.WorkSession {
				out.UnmatchedResults = append(out.UnmatchedResults, c.ref(r))
				c.missing("result_without_selected_admission")
			}
		}
		if r.Kind != sdk.InteractionNotice || r.Details.Work == nil {
			continue
		}
		w := *r.Details.Work
		if w.Session != r.Details.WorkSession {
			return ReviewResult{}, protocol("work change belongs to another session")
		}
		fact := WorkFact{Ref: c.ref(r), ProjectID: r.ProjectID, TurnID: r.TurnID, Change: w, TruncatedFields: []string{}}
		for _, field := range workFields(&fact.Change) {
			if *field.value == nil {
				continue
			}
			runes := []rune(**field.value)
			if len(runes) > 256 {
				text := string(runes[:256])
				*field.value = &text
				fact.TruncatedFields = append(fact.TruncatedFields, field.name)
				fact.WorkDetailRequests = append(fact.WorkDetailRequests, WorkSelection{WorkSession: q.WorkSession, IncludeDescendants: q.IncludeDescendants, SourceIncarnation: out.SourceIncarnation, WorkRef: fact.Ref, WorkField: field.name, ReadLimit: q.ReadLimit, RecordLimit: q.RecordLimit, DetailLimit: q.DetailLimit})
			}
		}
		sort.Strings(fact.TruncatedFields)
		out.WorkChanges = append(out.WorkChanges, fact)
	}
	sort.Slice(out.Evidence, func(i, j int) bool { return c.seen[out.Evidence[i].ID].Sequence < c.seen[out.Evidence[j].ID].Sequence })
	sort.Slice(out.WorkChanges, func(i, j int) bool { return out.WorkChanges[i].Ref.Sequence < out.WorkChanges[j].Ref.Sequence })
	sort.Slice(out.UnmatchedResults, func(i, j int) bool { return out.UnmatchedResults[i].Sequence < out.UnmatchedResults[j].Sequence })
	if !matched {
		c.missing("no_recorded_session_evidence")
	} else {
		out.Counts = &Counts{RecordedRequests: len(out.Attempts)}
		for _, a := range out.Attempts {
			switch a.Outcome {
			case "succeeded":
				out.Counts.Succeeded++
			case "failed":
				out.Counts.Failed++
			case "refused":
				out.Counts.Refused++
			case "cancelled":
				out.Counts.Cancelled++
			default:
				out.Counts.Unknown++
			}
		}
	}
	out.Coverage = c.out.Coverage
	if q.WorkRef != nil {
		r, ok := c.seen[q.WorkRef.ID]
		if !ok || !c.selected[r.ID] || r.Kind != sdk.InteractionNotice || r.Details.Work == nil {
			return ReviewResult{}, ErrWorkFieldUnavailable
		}
		if c.ref(r) != *q.WorkRef {
			return ReviewResult{}, ErrConflict
		}
		if r.Details.WorkSession == "" || r.Details.Work.Session != r.Details.WorkSession || !q.IncludeDescendants && r.Details.WorkSession != q.WorkSession {
			return ReviewResult{}, protocol("work notice belongs to another session")
		}
		field := workField(r.Details.Work, q.WorkField)
		if field == nil || *field == nil {
			return ReviewResult{}, ErrWorkFieldUnavailable
		}
		out.WorkDetail = &WorkDetail{Ref: *q.WorkRef, SourceIncarnation: out.SourceIncarnation, WorkSession: r.Details.WorkSession, Field: q.WorkField, Value: **field}
		out.Scope = "retained_work_field_detail"
	}
	if q.ResultRef != nil {

		for _, a := range out.Attempts {
			if a.ResultRef == nil || a.ResultRef.ID != q.ResultRef.ID {
				continue
			}
			if *a.ResultRef != *q.ResultRef {
				return ReviewResult{}, ErrConflict
			}
			r := c.seen[a.ResultRef.ID]
			detail := &ResultDetail{Ref: *a.ResultRef, SourceIncarnation: out.SourceIncarnation, WorkSession: a.WorkSession, Content: r.Content, Outcome: a.Outcome, ReasonCode: r.Details.ReasonCode, ResultRecord: r.Details.ResultRecord, ExecutionTruncated: r.Details.Truncated, Omissions: []string{"retained_transcript_may_omit_original_output"}}
			if runes := []rune(detail.Content); len(runes) > MaxResultDetailRunes {
				detail.Content = string(runes[:MaxResultDetailRunes])
				detail.ContentTruncated = true
				detail.Omissions = append(detail.Omissions, "result_detail_content_limit")
			}
			if detail.ExecutionTruncated {
				detail.Omissions = append(detail.Omissions, "tool_execution_truncated")
			}
			out.ResultDetail = detail
			break
		}
		if out.ResultDetail == nil {
			return ReviewResult{}, ErrResultUnavailable
		}
		out.Scope = "retained_result_detail"
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ReviewResult{}, err
	}
	if len(raw) > MaxReviewBytes {
		if out.WorkDetail != nil {
			return ReviewResult{}, ErrWorkFieldTooLarge
		}
		return ReviewResult{}, ErrReviewTooLarge
	}
	return out, nil
}
