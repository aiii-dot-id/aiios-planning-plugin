package assessment

import sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"

type workTextField struct {
	name  string
	value **string
}

func workFields(w *sdk.InteractionWorkChange) []workTextField {
	return []workTextField{
		{"state", &w.State}, {"focus", &w.Focus}, {"next_move", &w.NextMove}, {"plan", &w.Plan},
		{"expected_evidence", &w.ExpectedEvidence}, {"falsifier", &w.Falsifier}, {"decision_needed", &w.DecisionNeeded},
		{"result", &w.Result}, {"evidence", &w.Evidence}, {"evidence_readback", &w.EvidenceReadback},
	}
}

func workField(w *sdk.InteractionWorkChange, name string) **string {
	for _, field := range workFields(w) {
		if field.name == name {
			return field.value
		}
	}
	return nil
}

type WorkMaterial struct {
	Session            string                    `json:"work_session_id"`
	Availability       string                    `json:"availability"`
	Fields             sdk.InteractionWorkChange `json:"fields"`
	FieldRefs          map[string]Ref            `json:"field_refs"`
	TruncatedFields    []string                  `json:"truncated_fields"`
	WorkDetailRequests []WorkSelection           `json:"work_detail_requests,omitempty"`
}

func CurrentWork(review ReviewResult) WorkMaterial {
	out := WorkMaterial{Session: review.WorkSession, Availability: "not_retained", Fields: sdk.InteractionWorkChange{Session: review.WorkSession}, FieldRefs: map[string]Ref{}, TruncatedFields: []string{}}
	truncated := map[string]bool{}
	routes := map[string]WorkSelection{}
	for _, fact := range review.WorkChanges {
		if fact.Change.Session != review.WorkSession {
			continue
		}
		out.Availability = "retained"
		for _, f := range workFields(&fact.Change) {
			if *f.value != nil {
				v := **f.value
				*workField(&out.Fields, f.name) = &v
				out.FieldRefs[f.name] = fact.Ref
				truncated[f.name] = false
				delete(routes, f.name)
				for _, name := range fact.TruncatedFields {
					if name == f.name {
						truncated[name] = true
					}
				}
				for _, route := range fact.WorkDetailRequests {
					if route.WorkField == f.name {
						routes[f.name] = route
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
	for _, field := range workFields(&out.Fields) {
		if truncated[field.name] {
			out.TruncatedFields = append(out.TruncatedFields, field.name)
			if route, ok := routes[field.name]; ok {
				out.WorkDetailRequests = append(out.WorkDetailRequests, route)
			}
		}
	}
	return out
}
