package t3

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

var injectedWrite = errors.New("injected uncertain storage write")

type cutKV struct {
	*memoryKV
	at, writes int
	after      bool
}

func (k *cutKV) Put(key, value string) (PutResult, error) {
	k.writes++
	if k.writes == k.at && !k.after {
		return PutResult{}, injectedWrite
	}
	r, err := k.memoryKV.Put(key, value)
	if k.writes == k.at && k.after {
		return r, injectedWrite
	}
	return r, err
}
func cloneKV(k *memoryKV) *memoryKV {
	out := &memoryKV{rows: map[string]string{}, scope: "persistent"}
	for key, value := range k.rows {
		out.rows[key] = value
	}
	return out
}
func resume(k KV, c HostContext) *Service { s := NewService(k); s.Context = c; return s }
func beginInput(id string) Plan {
	return Plan{ID: id, Family: "test", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 10}
}
func assertReferences(t *testing.T, s *Service) {
	t.Helper()
	c, err := s.loadShared()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.reconciled(c); err != nil {
		t.Fatal(err)
	}
	plans, err := s.hotPlans()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		raw, err := Encode(p)
		if err != nil {
			t.Fatal(p, err)
		}
		if _, err = Decode(string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestSharedMeasurementWriteCuts(t *testing.T) {
	seed, k := setup(t)
	begin(t, seed, "p")
	apply(t, seed)
	if err := seed.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}
	seed.Context.Emitted = 1
	seed.Context.Settled = 1
	for _, operation := range []string{"begin", "grade", "rollover"} {
		base := cloneKV(k)
		ctx := seed.Context
		if operation == "rollover" {
			s := resume(base, ctx)
			begin(t, s, "q")
			ctx.Stream = "gen2"
			ctx.Emitted = 0
			ctx.Settled = 0
		}
		invoke := func(s *Service) error {
			if operation == "grade" {
				_, err := s.Grade("p", "passed", 5)
				return err
			}
			id := "q"
			if operation == "rollover" {
				id = "r"
			}
			_, err := s.Begin(beginInput(id), 5)
			return err
		}
		dry := &cutKV{memoryKV: cloneKV(base)}
		if err := invoke(resume(dry, ctx)); err != nil {
			t.Fatal(err)
		}
		for cut := 1; cut <= dry.writes; cut++ {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/after=%v", operation, cut, after), func(t *testing.T) {
					fault := &cutKV{memoryKV: cloneKV(base), at: cut, after: after}
					s := resume(fault, ctx)
					if err := invoke(s); !errors.Is(err, injectedWrite) {
						t.Fatal("cut not reached", err)
					}
					if s.refs != nil {
						t.Fatal("uncertain write left readiness valid")
					}
					persisted, err := Load(fault, "p")
					if err != nil {
						t.Fatal(err)
					}
					fixed := copyNumber(persisted.Measurement.End)
					fault.at = 0
					s = resume(fault, ctx)
					if operation == "grade" {
						if err := s.Observe(event(2, "ws")); err != nil {
							t.Fatal(err)
						}
						s.Context.Emitted = 2
						s.Context.Settled = 2
						got, err := s.Grade("p", "passed", 8)
						if err != nil {
							t.Fatal(err)
						}
						want := uint64(2)
						if fixed != nil {
							want = *fixed
						}
						if *got.Measurement.End != want || *got.ObservedCalls != want {
							t.Fatal("endpoint changed", got)
						}
					} else {
						id := "q"
						if operation == "rollover" {
							id = "r"
						}
						exists, err := s.exists(id)
						if err != nil {
							t.Fatal(err)
						}
						if !exists {
							if err := invoke(s); err != nil {
								t.Fatal(err)
							}
						}
						if operation == "rollover" {
							for _, id := range []string{"p", "q"} {
								p, err := Load(fault, id)
								if err != nil || p.State != "open" || p.HostCalibration != "unresolved" || p.Measurement.Storage != "inline" {
									t.Fatal(p, err)
								}
							}
						}
					}
					assertReferences(t, s)
				})
			}
		}
	}
}
func TestSharedMeasurementObservationCommitReplyLost(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	apply(t, s)
	fault := &cutKV{memoryKV: k, at: 1, after: true}
	s.KV = fault
	if err := s.Observe(event(1, "ws")); !errors.Is(err, injectedWrite) {
		t.Fatal(err)
	}
	fault.at = 0
	s = resume(fault, HostContext{Version: 1, Available: true, Stream: "gen1", Session: "ws", Emitted: 1, Settled: 1, Loss: 1})
	if err := s.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}
	p, err := s.Material("p")
	if err != nil || p.Measurement.Count != 1 {
		t.Fatal(p, err)
	}
	p, err = s.Grade("p", "passed", 4)
	if err != nil || p.HostCalibration != "unresolved" || p.ObservedCalls != nil {
		t.Fatal("retry erased host loss", p, err)
	}
}
func TestSharedMeasurementFailedReservationsBounded(t *testing.T) {
	s, _ := setup(t)
	begin(t, s, "p")
	for i := 1; i <= 10000; i++ {
		c, err := s.loadShared()
		if err != nil {
			t.Fatal(err)
		}
		c, err = s.reconciled(c)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.reserve("ws", uint64(i)); err != nil {
			t.Fatal(err)
		}
		s.refs = nil
		if err = s.saveShared(c); err != nil {
			t.Fatal(err)
		}
		if len(c.Sessions["ws"].Checkpoints) > 2 {
			t.Fatal("orphan growth")
		}
	}
	assertReferences(t, s)
	c, err := s.loadShared()
	if err != nil || len(c.Sessions["ws"].Checkpoints) != 1 {
		t.Fatal(c, err)
	}
}
func TestSharedMeasurementCapacityAndRollover(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	begin(t, s, "q")
	if err := s.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}

	apply(t, s)
	s.Context.Emitted = 1
	s.Context.Settled = 1
	if _, err := s.Grade("p", "passed", 3); err != nil {
		t.Fatal(err)
	}
	c, err := s.loadShared()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.point("ws", 0); err != nil {
		t.Fatal("shared start removed", err)
	}
	before := cloneKV(k)

	used := 0
	for _, v := range k.rows {
		used += len(v)
	}
	limited := &quotaKV{memoryKV: k, limit: used}
	s = resume(limited, HostContext{Version: 1, Available: true, Stream: "gen2", Session: "ws"})
	_, err = s.Begin(beginInput("new"), 4)
	if err == nil {
		t.Fatal("quota unexpectedly admitted")
	}
	for key, raw := range before.rows {
		if key == CalibrationKey {
			if _, err := decodeShared(k.rows[key]); err != nil {
				t.Fatal("lost source", err)
			}
		} else if _, found := k.rows[key]; !found {
			t.Fatal("deleted plan", raw)
		}
	}
	limited.limit = 1 << 20
	s = resume(limited, s.Context)
	begin(t, s, "new")
	assertReferences(t, s)
	old, err := Load(k, "q")
	if err != nil || old.Measurement.Count != 1 || !old.Measurement.Known || old.HostCalibration != "unresolved" {
		t.Fatal(old, err)
	}
	if k.rows[key("p")] != before.rows[key("p")] {
		t.Fatal("closed history changed during rollover")
	}
}
func TestSharedMeasurementTemporaryIOIsNotLostEvidence(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	apply(t, s)
	before := cloneKV(k)
	s.KV = &readFailureKV{memoryKV: k}
	if _, err := s.Grade("p", "passed", 4); err == nil {
		t.Fatal("read failure converted to grade")
	}
	if !reflect.DeepEqual(before.rows, k.rows) {
		t.Fatal("uncertain read changed history")
	}
}

type readFailureKV struct{ *memoryKV }

func (k *readFailureKV) Get(key string) (string, bool, error) {
	if key == CalibrationKey {
		return "", false, injectedWrite
	}
	return k.memoryKV.Get(key)
}

type alteredProofKV struct {
	*memoryKV
	field string
}

func (k *alteredProofKV) Put(key, value string) (PutResult, error) {
	r, err := k.memoryKV.Put(key, value)
	if key == CalibrationKey {
		switch k.field {
		case "stored":
			r.Stored = false
		case "key":
			r.Key = "other"
		case "size":
			r.ValueBytes++
		case "scope":
			r.Scope = "temp"
		}
	}
	return r, err
}
func TestSharedMeasurementWriteProof(t *testing.T) {
	for _, field := range []string{"stored", "key", "size", "scope"} {
		t.Run(field, func(t *testing.T) {
			s, k := setup(t)
			begin(t, s, "p")
			s.KV = &alteredProofKV{k, field}
			if err := s.Observe(event(1, "ws")); !errors.Is(err, ErrUnavailable) {
				t.Fatal("invalid write proof acknowledged", err)
			}
			if s.refs != nil {
				t.Fatal("invalid proof left ready references")
			}
		})
	}
}

type headroomKV struct {
	*memoryKV
	limit, peak int
}

func namespaceBytes(k *memoryKV) int {
	n := 0
	for key, value := range k.rows {
		n += len(key) + len(value)
	}
	return n
}
func (k *headroomKV) Put(key, value string) (PutResult, error) {
	next := namespaceBytes(k.memoryKV) + len(key) + len(value)
	if old, ok := k.rows[key]; ok {
		next -= len(key) + len(old)
	}
	if next > k.limit {
		return PutResult{}, ErrKVQuota
	}
	if next > k.peak {
		k.peak = next
	}
	return k.memoryKV.Put(key, value)
}
func TestSharedMeasurementRolloverHeadroom(t *testing.T) {
	seed, k := setup(t)
	begin(t, seed, "p")
	begin(t, seed, "q")
	if err := seed.Observe(event(1, "ws")); err != nil {
		t.Fatal(err)
	}
	ctx := HostContext{Version: 1, Available: true, Session: "ws", Stream: "gen2"}
	rollover := func(s *Service) error {
		c, err := s.sharedForBegin()
		if err != nil {
			return err
		}
		return s.saveShared(c)
	}
	initial := namespaceBytes(k)
	dry := &headroomKV{memoryKV: cloneKV(k), limit: 1 << 20, peak: initial}
	if err := rollover(resume(dry, ctx)); err != nil {
		t.Fatal(err)
	}
	final := namespaceBytes(dry.memoryKV)
	if final >= initial || dry.peak <= initial {
		t.Fatalf("fixture needs a smaller final state and larger intermediate state: %d -> %d -> %d", initial, dry.peak, final)
	}
	refused := 0
	for limit := initial; limit <= dry.peak; limit++ {
		trial := &headroomKV{memoryKV: cloneKV(k), limit: limit, peak: initial}
		s := resume(trial, ctx)
		err := rollover(s)
		if limit < dry.peak {
			if !errors.Is(err, ErrCapacity) && !errors.Is(err, ErrKVQuota) {
				t.Fatal(limit, err)
			}
			refused++
		} else if err != nil {
			t.Fatal(limit, err)
		}

		trial.limit = 1 << 20
		s = resume(trial, ctx)
		if err := rollover(s); err != nil {
			t.Fatal(limit, err)
		}
		assertReferences(t, s)
		for _, id := range []string{"p", "q"} {
			p, err := Load(trial, id)
			if err != nil || p.State != "open" || p.HostCalibration != "unresolved" || !p.Measurement.Known || p.Measurement.Count != 1 || p.Measurement.Cursor != 1 {
				t.Fatal(limit, p, err)
			}
		}
	}
	t.Logf("exact key+value bytes: before=%d peak=%d after=%d; %d safe capacity refusals; every case resumed", initial, dry.peak, final, refused)
}
