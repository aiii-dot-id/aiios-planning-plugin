package assessment

import (
	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"sort"
)

const CompactDiagnosticLimit = 5

type CompactReview struct {
	View              string                  `json:"view"`
	Identity          string                  `json:"identity"`
	WorkSession       string                  `json:"work_session_id"`
	SourceIncarnation string                  `json:"source_incarnation"`
	Scope             string                  `json:"scope"`
	CountBasis        string                  `json:"count_basis"`
	OwnCallExclusion  string                  `json:"own_call_exclusion"`
	ExcludedCallID    string                  `json:"excluded_call_id,omitempty"`
	Coverage          Coverage                `json:"coverage"`
	Counts            *Counts                 `json:"counts"`
	CurrentWork       *WorkMaterial           `json:"current_work,omitempty"`
	Correctness       string                  `json:"correctness"`
	Diagnostics       []Attempt               `json:"diagnostics"`
	UnmatchedResults  []Ref                   `json:"unmatched_results"`
	Evidence          []sdk.MemoryEvidenceRef `json:"evidence"`
	Omitted           CompactOmissions        `json:"omitted"`
	FullReviewRequest ReviewRequest           `json:"full_review_request"`
	FullReviewScope   string                  `json:"full_review_scope"`
}

type CompactOmissions struct {
	SuccessfulAttempts int `json:"successful_attempts"`
	OtherDiagnostics   int `json:"other_diagnostics"`
	UnmatchedResults   int `json:"unmatched_results"`
	WorkChanges        int `json:"work_changes"`
	EvidenceRefs       int `json:"evidence_refs"`
}

func (r ReviewResult) Compact() CompactReview {
	work := CurrentWork(r)
	q := r.Request
	q.View = "full"
	q.ResultRef = nil
	q.WorkRef = nil
	q.WorkField = ""
	q.SourceIncarnation = ""
	if q.WorkSession == "" {
		q = DefaultReviewRequest(r.WorkSession)
		q.View = "full"
		q.IncludeDescendants = r.Scope == "retained_session_and_proven_descendants"
	}
	out := CompactReview{View: "compact", Identity: r.Identity, WorkSession: r.WorkSession, SourceIncarnation: r.SourceIncarnation, Scope: r.Scope, CountBasis: r.CountBasis, OwnCallExclusion: r.OwnCallExclusion, ExcludedCallID: r.ExcludedCallID, Coverage: r.Coverage, Counts: r.Counts, CurrentWork: &work,
		Correctness: "Command and task correctness are unknown from recorded host outcomes alone; a succeeded host call can contain a failed command. Inspect applicable results and checks.",
		Diagnostics: []Attempt{}, UnmatchedResults: []Ref{}, Evidence: []sdk.MemoryEvidenceRef{}, FullReviewRequest: q,
		FullReviewScope: "A fresh bounded read, not a frozen replay: compare source_incarnation and coverage.cutoff. Full lists retain the original limits; lower record_limit if the response is too large. Result detail requests bind exact records."}
	for _, a := range r.Attempts {
		if a.Outcome == "succeeded" {
			out.Omitted.SuccessfulAttempts++
			continue
		}
		out.Diagnostics = append(out.Diagnostics, a)
	}
	sort.SliceStable(out.Diagnostics, func(i, j int) bool {
		a, b := out.Diagnostics[i].RequestRef, out.Diagnostics[j].RequestRef
		if a != nil && b != nil && a.Sequence != b.Sequence {
			return a.Sequence > b.Sequence
		}
		return out.Diagnostics[i].ID < out.Diagnostics[j].ID
	})
	if n := len(out.Diagnostics); n > CompactDiagnosticLimit {
		out.Omitted.OtherDiagnostics = n - CompactDiagnosticLimit
		out.Diagnostics = out.Diagnostics[:CompactDiagnosticLimit]
	}
	selected := map[string]bool{}
	for i := range out.Diagnostics {
		a := &out.Diagnostics[i]
		if a.RequestRef != nil {
			selected[a.RequestRef.ID] = true
		}
		if a.ResultRef != nil {
			selected[a.ResultRef.ID] = true

			a.ResultDetailRequest = &ResultSelection{WorkSession: a.WorkSession, SourceIncarnation: r.SourceIncarnation, ResultRef: *a.ResultRef}
		}
	}
	out.Omitted.WorkChanges = len(r.WorkChanges)
	for _, ref := range work.FieldRefs {
		selected[ref.ID] = true
	}
	out.UnmatchedResults = append(out.UnmatchedResults, r.UnmatchedResults...)
	if n := len(out.UnmatchedResults); n > CompactDiagnosticLimit {
		out.Omitted.UnmatchedResults = n - CompactDiagnosticLimit
		out.UnmatchedResults = out.UnmatchedResults[n-CompactDiagnosticLimit:]
	}
	for _, ref := range out.UnmatchedResults {
		selected[ref.ID] = true
	}
	for _, ref := range r.Evidence {
		if selected[ref.ID] {
			out.Evidence = append(out.Evidence, ref)
		} else {
			out.Omitted.EvidenceRefs++
		}
	}
	return out
}

func (r ReviewResult) Material() map[string]any {
	if r.Request.View == "compact" && r.ResultDetail == nil && r.WorkDetail == nil {
		compact := r.Compact()
		work := compact.CurrentWork
		compact.CurrentWork = nil
		return map[string]any{"current_work": work, "review": compact}
	}
	return map[string]any{"current_work": CurrentWork(r), "review": r}
}
