package assessment

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

const MaxReviewBytes = 60 << 10
const MaxResultDetailRunes = 8192

var ErrReviewTooLarge = errors.New(`assessment: review exceeds response budget; call review with a lower record_limit (for example, {"record_limit":8})`)
var ErrResultUnavailable = errors.New("assessment: result not available in the selected work and read limits; use the original work_session_id and source_incarnation, or increase bounded record_limit/read_limit/detail_limit and review again")

type ResultSelection struct {
	WorkSession       string `json:"work_session_id"`
	SourceIncarnation string `json:"source_incarnation"`
	ResultRef         Ref    `json:"result_ref"`
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
	SourceIncarnation  string `json:"source_incarnation,omitempty"`
}

func DefaultReviewRequest(session string) ReviewRequest {
	return ReviewRequest{View: "compact", WorkSession: session, ReadLimit: 32, RecordLimit: 128, DetailLimit: 256 << 10}
}

func (q ReviewRequest) Validate() error {
	if q.View != "" && q.View != "compact" && q.View != "full" {
		return ErrInvalid
	}
	if (q.ResultRef == nil) != (q.SourceIncarnation == "") || len(q.SourceIncarnation) > 256 {
		return ErrInvalid
	}
	if r := q.ResultRef; r != nil && (r.ID == "" || len(r.ID) > 256 || r.Sequence == 0 || !validHash(r.SHA256)) {
		return ErrInvalid
	}
	if len(q.WorkSession) == 0 || len(q.WorkSession) > 128 || q.ReadLimit < 1 || q.ReadLimit > 128 || q.RecordLimit < 1 || q.RecordLimit > 512 || q.DetailLimit < 1 || q.DetailLimit > 1<<20 {
		return ErrInvalid
	}
	for _, r := range q.WorkSession {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':') {
			return ErrInvalid
		}
	}
	return nil
}

type Counts struct {
	RecordedRequests int `json:"recorded_requests"`

	Admissions int `json:"admissions"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	Refused    int `json:"refused"`
	Cancelled  int `json:"cancelled"`
	Unknown    int `json:"unknown"`
}

type WorkFact struct {
	Ref             Ref                       `json:"ref"`
	ProjectID       string                    `json:"project_id"`
	TurnID          string                    `json:"turn_id"`
	Change          sdk.InteractionWorkChange `json:"change"`
	TruncatedFields []string                  `json:"truncated_fields"`
}

type ReviewResult struct {
	Request           ReviewRequest           `json:"-"`
	OwnCallExclusion  string                  `json:"own_call_exclusion"`
	ExcludedCallID    string                  `json:"excluded_call_id,omitempty"`
	Evidence          []sdk.MemoryEvidenceRef `json:"evidence"`
	Identity          string                  `json:"identity"`
	WorkSession       string                  `json:"work_session_id"`
	SourceIncarnation string                  `json:"source_incarnation"`
	Scope             string                  `json:"scope"`
	CountBasis        string                  `json:"count_basis"`
	Coverage          Coverage                `json:"coverage"`
	Counts            *Counts                 `json:"counts"`
	Attempts          []Attempt               `json:"attempts"`
	UnmatchedResults  []Ref                   `json:"unmatched_results"`
	WorkChanges       []WorkFact              `json:"work_changes"`
	ResultDetail      *ResultDetail           `json:"result_detail,omitempty"`
}

func (r ReviewResult) MarshalJSON() ([]byte, error) {
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
	c := &collection{
		sessionOnly: true, includeDescendants: q.IncludeDescendants, excludeCallID: q.ExcludeCallID, selected: map[string]bool{},
		q: Request{WorkSession: q.WorkSession, End: Ref{Sequence: math.MaxInt64}, Limits: Limits{ReadCalls: q.ReadLimit, Records: q.RecordLimit, DetailBytes: q.DetailLimit}},
		r: reader, seen: map[string]sdk.InteractionRecord{}, seq: map[uint64]string{},
		out: Result{Coverage: Coverage{Complete: true, Reasons: []string{}}, Attempts: []Attempt{}},
	}
	if err := c.collect(); err != nil {
		return ReviewResult{}, err
	}
	if q.ResultRef != nil && q.SourceIncarnation != c.out.SourceIncarnation {
		return ReviewResult{}, ErrSourceChanged
	}
	out := ReviewResult{Request: q, Identity: c.q.Identity, WorkSession: q.WorkSession, SourceIncarnation: c.out.SourceIncarnation, Scope: "retained_session_without_descendants", CountBasis: "retained_recorded_requests", Attempts: c.out.Attempts, UnmatchedResults: []Ref{}, WorkChanges: []WorkFact{}}
	out.OwnCallExclusion = "unavailable"
	out.Evidence = []sdk.MemoryEvidenceRef{}
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
		ref := RecordRef(r)
		out.Evidence = append(out.Evidence, sdk.MemoryEvidenceRef{ID: ref.ID, Sequence: strconv.FormatUint(ref.Sequence, 10), SHA256: ref.SHA256, Incarnation: c.out.SourceIncarnation})
		if r.Kind == sdk.InteractionToolResult {
			parent, ok := c.seen[r.RelatedID]
			if !ok || parent.Kind != sdk.InteractionToolCall || parent.Details.WorkSession != r.Details.WorkSession {
				out.UnmatchedResults = append(out.UnmatchedResults, RecordRef(r))
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
		fact := WorkFact{Ref: RecordRef(r), ProjectID: r.ProjectID, TurnID: r.TurnID, Change: w, TruncatedFields: []string{}}
		for name, field := range map[string]**string{
			"state": &fact.Change.State, "focus": &fact.Change.Focus, "next_move": &fact.Change.NextMove, "plan": &fact.Change.Plan, "expected_evidence": &fact.Change.ExpectedEvidence, "falsifier": &fact.Change.Falsifier, "decision_needed": &fact.Change.DecisionNeeded, "result": &fact.Change.Result, "evidence": &fact.Change.Evidence, "evidence_readback": &fact.Change.EvidenceReadback,
		} {
			if *field == nil {
				continue
			}
			runes := []rune(**field)
			if len(runes) > 256 {
				text := string(runes[:256])
				*field = &text
				fact.TruncatedFields = append(fact.TruncatedFields, name)
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
		out.Counts = &Counts{Admissions: len(out.Attempts), RecordedRequests: len(out.Attempts)}
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
		return ReviewResult{}, ErrReviewTooLarge
	}
	return out, nil
}
