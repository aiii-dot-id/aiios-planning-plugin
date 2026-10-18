package main

import (
	"encoding/json"
	"errors"
	"fmt"
	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/t3"
)

const pluginID = "id.aeon.planning"
const kvCapability = "ring4.kv"

type t3KV struct{}

func (t3KV) Get(k string) (string, bool, error) { return sdk.KV.Get(k) }
func (t3KV) Put(k, v string) (t3.PutResult, error) {
	r, e := sdk.KV.Put(k, v)
	var op *sdk.OperationError
	if errors.As(e, &op) && op.ReasonCode == "KV_QUOTA_EXCEEDED" {
		e = errors.Join(t3.ErrKVQuota, e)
	}
	return t3.PutResult{Stored: r.Stored, Key: r.Key, ValueBytes: r.ValueBytes, Scope: r.Scope}, e
}
func (t3KV) Delete(k string) (bool, error)                { return sdk.KV.Delete(k) }
func (t3KV) List(p string, l int) ([]string, bool, error) { return sdk.KV.List(p, l) }
func nowMillis(c sdk.Call) int64                          { n, _ := c.HostNowMillis(); return n }

func newPlanningPlugin() *sdk.Plugin {
	p := sdk.New(pluginID)
	registerReview(p)
	service := t3.NewService(t3KV{})
	service.Files = privateArchive{}
	p.Describe("plan", sdk.Descriptor{
		Summary: "Current-work reads require an existing core work session; use work action=start description=... to open one, or review with an existing work_session_id. begin registers optional calibration in that session, not a new task. Read current work and recorded evidence without registering another task. Compact material is the default; view=full retains the complete bounded evidence. Prefer review for current work and failure inspection. Existing begin, apply, grade, advise, status and abandon modes retain explicit bounded-plan calibration; abandon closes unresolved work without inventing a grade.",
		Effects: sdk.EffectsWriteLocal,
		Input:   "schemas/plan_in.json", Output: "schemas/plan_out.json",
		Capabilities: []string{kvCapability, "fs.private", "interaction.read"},
		Family:       "plan", Primary: false,
		Keywords: []string{"plan", "pre-register", "grade", "advice"},
	})
	p.Handle("plan", func(c sdk.Call) (any, error) {
		if err := t3.ValidatePublic(c.Arguments); err != nil {
			return nil, t3Failure(err)
		}
		args := c.Args()
		host, err := t3.ParseContext(args.Raw("_host_calibration"))
		if err != nil {
			host = t3.HostContext{}
		}
		service.Context = host
		mode, _ := args.String("mode")
		if mode == "" {
			mode = "material"
		}
		work := c.WorkSession()
		if mode == "material" || mode == "status" {
			if _, hasID := args.String("plan_id"); !hasID {
				if work.ID == "" {
					return nil, sdk.Fail("REVIEW_UNAVAILABLE", currentWorkRequirement(work.Reason))
				}
				q := assessment.DefaultReviewRequest(work.ID)
				if view, ok := args.String("view"); ok {
					q.View = view
				}
				q.IncludeDescendants = true
				q.ExcludeCallID = work.CallID
				review, e := assessment.Review(sdk.Interactions, q)
				if e != nil {
					return nil, reviewFailure(e)
				}
				raw, e := json.Marshal(review.Material())
				if e == nil && len(raw) > assessment.MaxReviewBytes {
					return nil, reviewFailure(assessment.ErrReviewTooLarge)
				}
				return json.RawMessage(raw), e
			}
		}
		if err != nil && mode != "material" && mode != "status" {
			return nil, t3Failure(t3.ErrUnavailable)
		}
		switch mode {
		case "begin":
			if host.Session == "" {
				return nil, sdk.Fail("CALIBRATION_SESSION_REFUSED", currentWorkRequirement(host.Reason))
			}
			id, ok1 := args.String("plan_id")
			if !ok1 && work.CallID != "" {
				id, ok1 = "plan_"+work.CallID, true
			}
			if work.ID != "" && work.ID != host.Session {
				return nil, t3Failure(t3.ErrSession)
			}
			family, ok2 := args.String("family")
			session := host.Session
			if asserted, present := args.String("session_id"); present && asserted != session {
				return nil, t3Failure(t3.ErrSession)
			}
			falsifier, ok4 := args.String("falsifier_class")
			expected, ok5 := args.String("expected_evidence_class")
			consequence, ok6 := args.String("consequence_class")
			steps, ok7 := args.Int("declared_steps")
			budget, ok8 := args.Int("budget_calls")
			for _, field := range []struct {
				name, requirement string
				ok                bool
			}{
				{"plan_id", "a string, or omitted only when the host supplies a call reference", ok1},
				{"family", "a string", ok2}, {"falsifier_class", "a string", ok4},
				{"expected_evidence_class", "a string", ok5}, {"consequence_class", "a string", ok6},
				{"declared_steps", "an integer from 1 to 256", ok7}, {"budget_calls", "an integer from 1 to 100000", ok8},
			} {
				if !field.ok {
					return nil, t3Failure(fmt.Errorf("%w: begin requires %s to be %s", t3.ErrInvalid, field.name, field.requirement))
				}
			}
			pl, err := service.Begin(t3.Plan{ID: id, Family: family, SessionID: session, FalsifierClass: falsifier, ExpectedEvidenceClass: expected, ConsequenceClass: consequence, DeclaredSteps: int(steps), BudgetCalls: int(budget)}, nowMillis(c))
			if err != nil {
				return nil, t3Failure(err)
			}
			return t3.Map(pl), nil
		case "material", "status":
			id, ok := args.String("plan_id")
			if !ok {
				return nil, t3Failure(t3.ErrInvalid)
			}
			pl, err := service.Material(id)
			if err != nil {
				return nil, t3Failure(err)
			}
			return t3.Map(pl), nil
		case "apply":
			id, ok1 := args.String("plan_id")
			applyID, ok2 := args.String("apply_id")
			stepID, ok3 := args.String("step_id")
			kind, ok4 := args.String("kind_class")
			from, ok5 := args.String("from_class")
			to, ok6 := args.String("to_class")
			count, ok7 := args.Int("count")
			if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
				return nil, t3Failure(t3.ErrInvalid)
			}
			pl, err := service.Apply(id, applyID, stepID, kind, from, to, int(count), nowMillis(c))
			if err != nil {
				return nil, t3Failure(err)
			}
			return t3.Map(pl), nil
		case "grade":
			id, ok1 := args.String("plan_id")
			result, ok2 := args.String("result_class")
			if !ok1 || !ok2 {
				return nil, t3Failure(t3.ErrInvalid)
			}
			pl, err := service.Grade(id, result, nowMillis(c))
			if err != nil {
				return nil, t3Failure(err)
			}
			return t3.Map(pl), nil
		case "abandon":
			id, ok := args.String("plan_id")
			if !ok {
				return nil, t3Failure(t3.ErrInvalid)
			}
			pl, err := service.Abandon(id, nowMillis(c))
			if err != nil {
				return nil, t3Failure(err)
			}
			return t3.Map(pl), nil
		case "advise":
			id, ok1 := args.String("plan_id")
			control, ok2 := args.String("control_class")
			if !ok1 || !ok2 {
				return nil, t3Failure(t3.ErrInvalid)
			}
			cursor, _ := args.String("history_cursor")
			pl, advice, err := service.Advise(id, control, cursor)
			if err != nil {
				return nil, t3Failure(err)
			}
			m := t3.Map(pl)
			m["advice"] = t3.AdviceMap(*advice)
			return m, nil
		default:
			return nil, t3Failure(t3.ErrInvalid)
		}
	})
	p.Describe("observe", sdk.Descriptor{Summary: "Host calibration delivery", Effects: sdk.EffectsWriteLocal, Capabilities: []string{kvCapability}, Input: "schemas/observe_in.json", Output: "schemas/observe_out.json"})
	p.Handle("observe", func(c sdk.Call) (any, error) {
		e, err := t3.ParseEvent(c.Arguments)
		if err != nil {
			return nil, t3Failure(err)
		}
		if err = service.Observe(e); err != nil {
			return nil, t3Failure(err)
		}
		return map[string]any{"accepted": true, "stream_id": e.Stream, "sequence": e.Sequence}, nil
	})
	return p
}

func t3Failure(err error) error {
	var denied *sdk.Denied
	var operation *sdk.OperationError
	if errors.As(err, &denied) || errors.As(err, &operation) {
		return err
	}
	code := "OPERATION_ARGUMENT_INVALID"
	switch {
	case errors.Is(err, t3.ErrPending):
		code = "CALIBRATION_PENDING"
	case errors.Is(err, t3.ErrUnavailable):
		code = "CALIBRATION_UNAVAILABLE"
	case errors.Is(err, t3.ErrCapacity):
		code = "PLANNING_CAPACITY_EXHAUSTED"
	case errors.Is(err, t3.ErrArchive), errors.Is(err, t3.ErrArchiveConflict):
		code = "PLANNING_ARCHIVE_UNAVAILABLE"
	case errors.Is(err, t3.ErrSession):
		code = "CALIBRATION_SESSION_REFUSED"
	case errors.Is(err, t3.ErrInvalid), errors.Is(err, t3.ErrNotFound), errors.Is(err, t3.ErrExists), errors.Is(err, t3.ErrClosed), errors.Is(err, t3.ErrNoRegistration):
	default:
		code = "CALIBRATION_UNAVAILABLE"
	}
	return sdk.Fail(code, t3.Reason(err))
}

func currentWorkRequirement(reason string) string {
	if reason == "session_lookup_failed" {
		return "the host could not look up the acting work session; resolve that lookup failure before retrying; do not invent a session ID or start replacement work"
	}
	return "no acting work session: for new work use core work action=start description=... first, then retry; review can instead read an existing work_session_id; Planning begin does not open core work"
}
