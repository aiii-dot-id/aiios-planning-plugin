package assessment

import sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"

type WorkMaterial struct {
	Session         string                    `json:"work_session_id"`
	Availability    string                    `json:"availability"`
	Fields          sdk.InteractionWorkChange `json:"fields"`
	FieldRefs       map[string]Ref            `json:"field_refs"`
	TruncatedFields []string                  `json:"truncated_fields"`
}

func CurrentWork(review ReviewResult) WorkMaterial {
	out := WorkMaterial{Session: review.WorkSession, Availability: "not_retained", Fields: sdk.InteractionWorkChange{Session: review.WorkSession}, FieldRefs: map[string]Ref{}, TruncatedFields: []string{}}
	truncated := map[string]bool{}
	for _, fact := range review.WorkChanges {
		if fact.Change.Session != review.WorkSession {
			continue
		}
		out.Availability = "retained"
		fields := []struct {
			name string
			src  *string
			dst  **string
		}{
			{"state", fact.Change.State, &out.Fields.State}, {"focus", fact.Change.Focus, &out.Fields.Focus},
			{"next_move", fact.Change.NextMove, &out.Fields.NextMove}, {"plan", fact.Change.Plan, &out.Fields.Plan},
			{"expected_evidence", fact.Change.ExpectedEvidence, &out.Fields.ExpectedEvidence}, {"falsifier", fact.Change.Falsifier, &out.Fields.Falsifier},
			{"decision_needed", fact.Change.DecisionNeeded, &out.Fields.DecisionNeeded}, {"result", fact.Change.Result, &out.Fields.Result},
			{"evidence", fact.Change.Evidence, &out.Fields.Evidence}, {"evidence_readback", fact.Change.EvidenceReadback, &out.Fields.EvidenceReadback},
		}
		for _, f := range fields {
			if f.src != nil {
				v := *f.src
				*f.dst = &v
				out.FieldRefs[f.name] = fact.Ref
				truncated[f.name] = false
				for _, name := range fact.TruncatedFields {
					if name == f.name {
						truncated[name] = true
					}
				}
			}
		}
		if fact.Change.Steps != nil {
			v := *fact.Change.Steps
			out.Fields.Steps = &v
			out.FieldRefs["steps"] = fact.Ref
		}
		if fact.Change.Independent != nil {
			v := *fact.Change.Independent
			out.Fields.Independent = &v
			out.FieldRefs["independent"] = fact.Ref
		}
	}
	for _, name := range []string{"state", "focus", "next_move", "plan", "expected_evidence", "falsifier", "decision_needed", "result", "evidence", "evidence_readback"} {
		if truncated[name] {
			out.TruncatedFields = append(out.TruncatedFields, name)
		}
	}
	return out
}
