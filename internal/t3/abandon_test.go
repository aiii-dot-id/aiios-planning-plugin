package t3

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAbandonRecoversOldSessionCapacity(t *testing.T) {
	s, k := setup(t)
	s.Files = newArchive()
	for i := 0; i < MaxPlans; i++ {
		begin(t, s, fmt.Sprintf("p%d", i))
	}
	if _, err := s.Apply("p0", "a", "s1", "refinement", "before", "after", 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}
	before, err := s.Material("p0")
	if err != nil {
		t.Fatal(err)
	}
	s.Context.Emitted, s.Context.Settled = 1, 1
	s.Context.Session = "new-session"
	next := beginInput("next")
	next.SessionID = s.Context.Session
	if _, err = s.Begin(next, 3); !errors.Is(err, ErrCapacity) {
		t.Fatal("expected full capacity", err)
	}
	p, err := s.Abandon("p0", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Abandoned || p.State != "closed" || p.HostCalibration != "unresolved" || p.GradeClass != "" || p.ObservedCalls != nil {
		t.Fatalf("invented grade or lost closure: %+v", p)
	}
	if !reflect.DeepEqual(before.Steps, p.Steps) || !reflect.DeepEqual(before.Applies, p.Applies) || p.Measurement.Count != 1 || !p.Measurement.Known {
		t.Fatal("lost captured work or measurement")
	}
	if p.Measurement.Result != "" || p.Measurement.End != nil {
		t.Fatal("invented endpoint/result")
	}
	if _, err = s.Begin(next, 5); err != nil {
		t.Fatal("capacity not recovered", err)
	}
	if _, found, _ := k.Get(key("p0")); found {
		t.Fatal("closed record not archived")
	}
	reread, err := s.Material("p0")
	if err != nil || !reflect.DeepEqual(Map(p), Map(reread)) {
		t.Fatal("archive changed record", err)
	}
	again, err := s.Abandon("p0", 6)
	if err != nil || again.ClosedMs != 4 {
		t.Fatal("retry changed closure", err)
	}
	if _, err = s.Apply("p0", "b", "s1", "refinement", "x", "y", 1, 7); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestAbandonPreservesPendingEvidenceAndRefusesGrade(t *testing.T) {
	s, _ := setup(t)
	begin(t, s, "p")
	apply(t, s)
	s.Context.Emitted = 2
	if _, err := s.Grade("p", "passed", 3); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if err := s.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}
	p, err := s.Abandon("p", 4)
	if err != nil {
		t.Fatal(err)
	}
	if p.Measurement.End == nil || *p.Measurement.End != 2 || p.Measurement.Result != "passed" || p.Measurement.Count != 1 || p.GradeClass != "" {
		t.Fatalf("pending evidence changed: %+v", p)
	}
	if _, err = s.Grade("p", "passed", 5); !errors.Is(err, ErrClosed) {
		t.Fatal("abandonment became a grade", err)
	}
	if _, _, err = s.Advise("p", "control.advice_used"); err == nil {
		t.Fatal("abandoned result used for advice")
	}
}
func TestAbandonPersistenceAndRecovery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			s, k := setup(t)
			begin(t, s, "p")
			before := k.rows[key("p")]
			cut := &cutKV{memoryKV: k, at: 1, after: after}
			r := resume(cut, s.Context)
			if _, err := r.Abandon("p", 3); err == nil {
				t.Fatal("storage error hidden")
			}
			if !after && k.rows[key("p")] != before {
				t.Fatal("failed write changed source")
			}
			r = resume(k, s.Context)
			p, err := r.Abandon("p", 4)
			if err != nil || !p.Abandoned {
				t.Fatal("retry failed", err)
			}
			assertReferences(t, r)
		})
	}
}
func TestAbandonRequiresContextAndDoesNotRewriteGrade(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	before := k.rows[key("p")]
	s.Context = HostContext{}
	if _, err := s.Abandon("p", 3); !errors.Is(err, ErrSession) || k.rows[key("p")] != before {
		t.Fatal("missing context accepted", err)
	}
	s.Context = HostContext{Version: 1, Available: true, Session: "ws", Stream: "gen1"}
	apply(t, s)
	graded, err := s.Grade("p", "passed", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Abandon("p", 5); !errors.Is(err, ErrClosed) {
		t.Fatal("graded result rewritten", err)
	}
	got, _ := s.Material("p")
	if !reflect.DeepEqual(Map(graded), Map(got)) {
		t.Fatal("grade changed")
	}
	if err = ValidatePublic([]byte(`{"mode":"abandon","plan_id":"p"}`)); err != nil {
		t.Fatal(err)
	}
	if err = ValidatePublic([]byte(`{"mode":"abandon","plan_id":"p","result_class":"passed"}`)); err == nil {
		t.Fatal("injected result accepted")
	}
}

func TestAbandonRejectsNullMarker(t *testing.T) {
	s, _ := setup(t)
	begin(t, s, "p")
	p, err := s.Abandon("p", 4)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	malformed := strings.Replace(string(raw), `"abandoned":true`, `"abandoned":null`, 1)
	if malformed == string(raw) {
		t.Fatal("missing abandonment marker")
	}
	if _, err = Decode(malformed); err == nil {
		t.Fatal("null abandonment marker accepted")
	}
}
