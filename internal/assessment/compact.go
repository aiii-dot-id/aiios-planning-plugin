package assessment

import "sort"

const CompactAttemptLimit = 5

type CompactReview struct {
	View              string           `json:"view"`
	Identity          string           `json:"identity"`
	WorkSession       string           `json:"work_session_id"`
	SourceIncarnation string           `json:"source_incarnation"`
	Scope             string           `json:"scope"`
	CountBasis        string           `json:"count_basis"`
	OwnCallExclusion  string           `json:"own_call_exclusion"`
	ExcludedCallID    string           `json:"excluded_call_id,omitempty"`
	Coverage          Coverage         `json:"coverage"`
	Counts            *Counts          `json:"counts"`
	CurrentWork       WorkMaterial     `json:"current_work"`
	Correctness       string           `json:"correctness"`
	Attempts          []Attempt        `json:"attempts"`
	UnmatchedResults  []Ref            `json:"unmatched_results"`
	Omitted           CompactOmissions `json:"omitted"`
	FullReviewRequest ReviewRequest    `json:"full_review_request"`
	FullReviewScope   string           `json:"full_review_scope"`
}

type CompactOmissions struct {
	Attempts         int `json:"attempts"`
	UnmatchedResults int `json:"unmatched_results"`
	WorkChanges      int `json:"work_changes"`
}

func (r ReviewResult) Compact() CompactReview {
	q := r.Request
	q.View = "full"
	q.ResultRef = nil
	q.WorkRef = nil
	q.WorkField = ""
	q.SourceIncarnation = ""
	out := CompactReview{View: "compact", Identity: r.Identity, WorkSession: r.WorkSession, SourceIncarnation: r.SourceIncarnation, Scope: r.Scope, CountBasis: r.CountBasis, OwnCallExclusion: r.OwnCallExclusion, ExcludedCallID: r.ExcludedCallID, Coverage: r.Coverage, Counts: r.Counts, CurrentWork: CurrentWork(r),
		Correctness: "Command and task correctness are unknown from recorded host outcomes alone; a succeeded host call can contain a failed command. Inspect applicable results and checks.",
		Attempts:    []Attempt{}, UnmatchedResults: []Ref{}, FullReviewRequest: q,
		FullReviewScope: "A fresh bounded read, not a frozen replay: compare source_incarnation and coverage.cutoff. Full lists retain the original limits; lower record_limit if the response is too large. Result detail requests bind exact records."}
	attempts := append([]Attempt{}, r.Attempts...)
	sort.SliceStable(attempts, func(i, j int) bool {
		a, b := attempts[i].RequestRef, attempts[j].RequestRef
		if a != nil && b != nil && a.Sequence != b.Sequence {
			return a.Sequence > b.Sequence
		}
		return attempts[i].ID < attempts[j].ID
	})
	shown := map[bool]int{}
	for _, a := range attempts {
		succeeded := a.Outcome == "succeeded"
		if shown[succeeded] == CompactAttemptLimit {
			out.Omitted.Attempts++
			continue
		}
		shown[succeeded]++
		if a.ResultRef != nil {

			a.ResultDetailRequest = &ResultSelection{WorkSession: a.WorkSession, SourceIncarnation: r.SourceIncarnation, ResultRef: *a.ResultRef, ReadLimit: r.Request.ReadLimit, RecordLimit: r.Request.RecordLimit, DetailLimit: r.Request.DetailLimit}
		}
		out.Attempts = append(out.Attempts, a)
	}
	out.Omitted.WorkChanges = len(r.WorkChanges)
	out.UnmatchedResults = append(out.UnmatchedResults, r.UnmatchedResults...)
	if n := len(out.UnmatchedResults); n > CompactAttemptLimit {
		out.Omitted.UnmatchedResults = n - CompactAttemptLimit
		out.UnmatchedResults = out.UnmatchedResults[n-CompactAttemptLimit:]
	}
	return out
}
