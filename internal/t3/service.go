package t3

import (
	"errors"
	"strconv"
)

type Service struct {
	KV          KV
	Context     HostContext
	Files       ArchiveFiles
	refs        map[string]measurementRef
	readyStream string
}

func NewService(kv KV) *Service { return &Service{KV: kv} }

func (s *Service) Begin(p Plan, nowMs int64) (*Plan, error) {
	p.State = "open"
	p.OpenedMs = nowMs
	p.HostCalibration = "unavailable"
	if !s.Context.Available || s.Context.Session != p.SessionID {
		return nil, ErrUnavailable
	}
	if err := ValidBegin(&p); err != nil {
		return nil, err
	}
	exists, err := s.exists(p.ID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrExists
	}
	keys, truncated, err := s.KV.List(KeyPrefix, MaxPlans)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrCapacity
	}
	if len(keys) >= MaxPlans {
		if err := s.archiveOne(p.ID); err != nil {
			return nil, err
		}
	}
	c, err := s.sharedForBegin()
	if err != nil {
		return nil, err
	}
	if err = c.reserve(p.SessionID, s.Context.Emitted); err != nil {
		return nil, err
	}
	p.Measurement = &Measurement{Version: 2, Storage: "shared", Stream: s.Context.Stream, Start: s.Context.Emitted, Loss: s.Context.Loss}
	p.HostCalibration = "provisional"
	p.Steps = make([]Step, p.DeclaredSteps)
	if err := ValidBegin(&p); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range p.Steps {
		p.Steps[i] = Step{ID: "s" + strconv.Itoa(i+1), State: "planned"}
		if seen[p.Steps[i].ID] {
			return nil, ErrInvalid
		}
		seen[p.Steps[i].ID] = true
	}
	refs := s.refs
	s.refs = nil
	if err := s.saveSharedPublic(c); err != nil {
		return nil, err
	}
	if err := s.save(&p); err != nil {
		return nil, err
	}
	s.refs = refs
	s.readyStream = c.Stream
	s.remember(&p)
	n, at, known, err := c.snapshot(&p)
	if err != nil {
		return nil, err
	}
	p.Measurement.Count = n
	p.Measurement.Cursor = at
	p.Measurement.Known = known
	return &p, nil
}

func (s *Service) Material(id string) (*Plan, error) {
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	s.viewMeasurement(p)
	if p.State == "open" && (p.Measurement == nil || !s.Context.Available || p.Measurement.Stream != s.Context.Stream || p.Measurement.Loss != s.Context.Loss) {
		p.HostCalibration = "unresolved"
	}
	return p, nil
}

func (s *Service) Apply(id, applyID, stepID, kind, from, to string, count int, nowMs int64) (*Plan, error) {
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if p.State != "open" || (p.Measurement != nil && p.Measurement.End != nil) {
		return nil, ErrClosed
	}
	if err := s.session(p); err != nil {
		return nil, err
	}
	if !validToken(applyID, MaxIDBytes) || !validToken(stepID, MaxIDBytes) || (kind != "refinement" && kind != "deviation") || !validToken(from, MaxClassBytes) || !validToken(to, MaxClassBytes) || !Count(count, 1, 100000) {
		return nil, ErrInvalid
	}
	for _, old := range p.Applies {
		if old.ID == applyID {
			return nil, ErrExists
		}
	}
	idx := -1
	for i := range p.Steps {
		if p.Steps[i].ID == stepID {
			idx = i
			break
		}
	}
	if idx < 0 || p.Steps[idx].State != "planned" {
		return nil, ErrInvalid
	}
	a := Apply{ID: applyID, StepID: stepID, KindClass: kind, FromClass: from, ToClass: to, Count: count, AtMs: nowMs}
	if len(p.Applies) >= MaxApplies {
		return nil, ErrInvalid
	}
	p.Applies = append(p.Applies, a)
	p.Steps[idx].State = "applied"
	p.Steps[idx].Applied += count
	if err := s.save(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) Grade(id, resultClass string, nowMs int64) (*Plan, error) {
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if err := s.session(p); err != nil {
		return nil, err
	}
	if !validToken(resultClass, MaxClassBytes) {
		return nil, ErrNoRegistration
	}
	m := p.Measurement
	if p.State == "closed" {
		if !p.Abandoned && m != nil && m.Result == resultClass {
			return p, nil
		}
		return nil, ErrClosed
	}
	if m != nil && m.Result != "" && m.Result != resultClass {
		return nil, ErrInvalid
	}
	completed := 0
	for _, step := range p.Steps {
		if step.State == "applied" {
			completed++
		}
	}
	stepOK := completed == p.DeclaredSteps
	falsifierOK := resultClass == p.FalsifierClass
	p.StepCountOK = boolPtr(stepOK)
	p.FalsifierOK = boolPtr(falsifierOK)
	if !stepOK {
		return nil, ErrNoRegistration
	}
	c, err := s.gradeMeasurement(p, resultClass)
	if err != nil {
		return nil, err
	}
	p.GradeClass = "unresolved"
	if falsifierOK {
		p.GradeClass = "refuted"
	}
	p.State = "closed"
	p.ClosedMs = nowMs
	refs := s.refs
	s.refs = nil
	if err := s.save(p); err != nil {
		return nil, err
	}
	if refs != nil {
		if err := s.finishReferences(p, c, refs); err != nil {
			return nil, err
		}
	}

	return p, nil
}

func (s *Service) Advise(id, controlClass string, cursor ...string) (*Plan, *Advice, error) {
	p, err := s.load(id)
	if err != nil {
		return nil, nil, err
	}
	if p.State != "closed" || p.Abandoned {
		return nil, nil, ErrInvalid
	}
	if controlClass != "control.advice_used" && controlClass != "control.advice_withheld" {
		return nil, nil, ErrInvalid
	}
	after := ""
	if len(cursor) > 0 {
		after = cursor[0]
	}
	rows, more, next, err := s.historyPage(p.Family, after)
	if err != nil {
		return nil, nil, err
	}
	sources := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.State == "closed" && !row.Abandoned {
			sources = append(sources, row.ID)
		}
	}
	if len(sources) == 0 {
		return p, &Advice{Family: p.Family, ProposalClass: "no_matching_history", ControlClass: controlClass, HistoryTruncated: more, HistoryNextCursor: next}, nil
	}
	cited := sources
	if len(cited) > 16 {
		cited = cited[:16]
	}
	a := Advice{ID: "a:" + digest([]byte(p.ID+":"+strconv.Itoa(len(p.Advice)+1))), HistoryTruncated: more, HistoryNextCursor: next, Family: p.Family, SourceRows: cited, SampleSize: len(sources), CounterCaseClass: "counter_case.falsifier_absent", ControlClass: controlClass}
	if controlClass == "control.advice_withheld" {
		a.ProposalClass = "control.withheld"
	} else {
		a.ProposalClass = "next_plan.repeat_family"
	}
	if len(p.Advice) >= MaxAdvice {
		return nil, nil, ErrInvalid
	}
	p.Advice = append(p.Advice, a)
	if err := s.save(p); err != nil {
		return nil, nil, err
	}
	return p, &a, nil
}

func (s *Service) Status(id string) (*Plan, error) { return s.Material(id) }

func Map(p *Plan) map[string]any {
	m := map[string]any{
		"plan_id": p.ID, "family": p.Family, "session_id": p.SessionID,
		"falsifier_class": p.FalsifierClass, "expected_evidence_class": p.ExpectedEvidenceClass,
		"consequence_class": p.ConsequenceClass, "declared_steps": p.DeclaredSteps,
		"budget_calls": p.BudgetCalls, "state": p.State, "opened_ms": p.OpenedMs,
		"host_calibration": p.HostCalibration, "applies": len(p.Applies),
	}
	if p.Abandoned {
		m["abandoned"] = true
	}
	if p.ObservedCalls != nil {
		m["observed_calls"] = *p.ObservedCalls
	}
	if p.ClosedMs != 0 {
		m["closed_ms"] = p.ClosedMs
	}
	if p.GradeClass != "" {
		m["grade_class"] = p.GradeClass
	}
	if p.StepCountOK != nil {
		m["step_count_ok"] = *p.StepCountOK
	}
	if p.FalsifierOK != nil {
		m["falsifier_ok"] = *p.FalsifierOK
	}
	if p.Measurement != nil {
		v := p.Measurement
		measured := map[string]any{"stream_id": v.Stream, "start": v.Start, "loss_count": v.Loss, "count": v.Count, "cursor": v.Cursor}
		if v.Version == 2 && !v.Known {
			measured["count"] = nil
			measured["cursor"] = nil
		}
		if v.End != nil {
			measured["end"] = *v.End
		}
		if v.Result != "" {
			measured["result_class"] = v.Result
		}
		m["measurement"] = measured
	}
	return m
}

func AdviceMap(a Advice) map[string]any {
	sources := a.SourceRows
	if sources == nil {
		sources = []string{}
	}
	m := map[string]any{"history_truncated": a.HistoryTruncated, "history_next_cursor": a.HistoryNextCursor, "family": a.Family, "proposal_class": a.ProposalClass, "source_rows": sources, "sample_size": a.SampleSize, "control_class": a.ControlClass}
	if a.ID != "" {
		m["advice_id"] = a.ID
		m["counter_case_class"] = a.CounterCaseClass
	}
	return m
}

func boolPtr(v bool) *bool { return &v }
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrInvalid):
		if err != ErrInvalid {
			return err.Error()
		}
		return "arguments must contain only identifiers, classes, counts, and enums"
	case errors.Is(err, ErrNotFound):
		return "no T3 plan with that id"
	case errors.Is(err, ErrExists):
		return "T3 plan or row already exists"
	case errors.Is(err, ErrClosed):
		return "T3 plan is not open"
	case errors.Is(err, ErrNoRegistration):
		return "T3 pre-registration or close-time requirements are incomplete"
	}
	return err.Error()
}
