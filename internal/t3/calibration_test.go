package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

type memoryKV struct {
	rows      map[string]string
	scope     string
	fail      bool
	truncated bool
	puts      int
}

func (k *memoryKV) Get(s string) (string, bool, error) { v, ok := k.rows[s]; return v, ok, nil }
func (k *memoryKV) Put(s, v string) (PutResult, error) {
	if k.fail {
		return PutResult{}, errors.New("disk unavailable")
	}
	k.rows[s] = v
	k.puts++
	return PutResult{Stored: true, Key: s, ValueBytes: len(v), Scope: k.scope}, nil
}
func (k *memoryKV) Delete(s string) (bool, error) {
	_, ok := k.rows[s]
	delete(k.rows, s)
	return ok, nil
}
func (k *memoryKV) List(prefix string, limit int) ([]string, bool, error) {
	var a []string
	for s := range k.rows {
		if strings.HasPrefix(s, prefix) {
			a = append(a, s)
		}
	}
	sort.Strings(a)
	more := k.truncated || len(a) > limit
	if len(a) > limit {
		a = a[:limit]
	}
	return a, more, nil
}
func setup(t *testing.T) (*Service, *memoryKV) {
	t.Helper()
	kv := &memoryKV{rows: map[string]string{}, scope: "persistent"}
	s := NewService(kv)
	s.Context = HostContext{Version: 1, Available: true, Session: "ws", Stream: "gen1"}
	return s, kv
}
func begin(t *testing.T, s *Service, id string) {
	t.Helper()
	_, err := s.Begin(Plan{ID: id, Family: "test", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 10}, 1)
	if err != nil {
		t.Fatal(err)
	}
}
func event(n uint64, session string) Event {
	return Event{Stream: "gen1", Sequence: n, Topic: "tool.called", At: "2026-09-24T00:00:00Z", Payload: EventPayload{Tool: "read", Actor: "main", Session: session}}
}
func apply(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.Apply("p", "a1", "s1", "refinement", "before", "after", 1, 2); err != nil {
		t.Fatal(err)
	}
}
func TestFrozenEndpointAndPersistentCount(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	apply(t, s)
	s.Context.Emitted = 3
	if _, e := s.Grade("p", "passed", 3); !errors.Is(e, ErrPending) {
		t.Fatal(e)
	}
	p, e := Load(k, "p")
	if e != nil || p.Measurement.End == nil || *p.Measurement.End != 3 {
		t.Fatal(p, e)
	}
	if _, e = s.Apply("p", "a2", "s1", "deviation", "a", "b", 1, 3); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	for _, v := range []Event{event(1, "ws"), event(2, "other"), event(3, "ws"), event(4, "ws")} {
		if e = s.Observe(v); e != nil {
			t.Fatal(e)
		}
	}
	s.Context.Emitted = 100
	s.Context.Settled = 3
	p, e = s.Grade("p", "passed", 4)
	if e != nil || p.ObservedCalls == nil || *p.ObservedCalls != 2 || *p.Measurement.End != 3 {
		t.Fatal(p, e)
	}
	s.Context.Stream = "gen2"
	p, e = s.Grade("p", "passed", 5)
	if e != nil || p.HostCalibration != "complete" {
		t.Fatal(p, e)
	}
	if _, e = s.Grade("p", "changed", 5); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	s.Context.Session = "other"
	if _, e = s.Grade("p", "passed", 5); !errors.Is(e, ErrSession) {
		t.Fatal(e)
	}
	p, e = s.Material("p")
	if e != nil || p.HostCalibration != "complete" {
		t.Fatal(p, e)
	}
}
func TestLossBeforeTailSettlesClosesUnresolved(t *testing.T) {
	for _, mode := range []string{"loss", "restart", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := setup(t)
			begin(t, s, "p")
			apply(t, s)
			s.Context.Emitted = 2
			if _, e := s.Grade("p", "passed", 3); !errors.Is(e, ErrPending) {
				t.Fatal(e)
			}
			switch mode {
			case "loss":
				s.Context.Loss++
			case "restart":
				s.Context.Stream = "gen2"
			case "unavailable":
				s.Context.Available = false
				s.Context.Reason = "generation_changed"
			}
			p, e := s.Grade("p", "passed", 4)
			if e != nil || p.HostCalibration != "unresolved" || p.ObservedCalls != nil || p.State != "closed" {
				t.Fatal(p, e)
			}
		})
	}
}
func TestObservationDuplicateConflictAndFailure(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	v := event(1, "ws")
	if e := s.Observe(v); e != nil {
		t.Fatal(e)
	}
	puts := k.puts
	if e := s.Observe(v); e != nil || k.puts != puts {
		t.Fatal(e, k.puts)
	}
	v.Payload.Tool = "ls"
	if e := s.Observe(v); e == nil {
		t.Fatal("conflict accepted")
	}
	k.fail = true
	if e := s.Observe(event(2, "ws")); e == nil {
		t.Fatal("write failure acknowledged")
	}
	k.fail = false
	k.truncated = true
	if e := s.Observe(event(2, "ws")); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
}
func TestBeginValidatesBeforeAllocationAndRequiresDurability(t *testing.T) {
	s, k := setup(t)
	for _, n := range []int{-1, 257, 1 << 30} {
		_, e := s.Begin(Plan{DeclaredSteps: n, SessionID: "ws"}, 1)
		if e == nil {
			t.Fatal(n)
		}
	}
	k.scope = "temp"
	_, e := s.Begin(Plan{ID: "p", Family: "f", SessionID: "ws", FalsifierClass: "f", ExpectedEvidenceClass: "e", ConsequenceClass: "c", DeclaredSteps: 1, BudgetCalls: 1}, 1)
	if !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestCalibrationJSONBounds(t *testing.T) {
	good := fmt.Sprintf(`{"version":1,"available":true,"session_id":"ws","stream_id":"g","emitted":%d,"settled":0,"loss_count":0}`, MaxCounter)
	if _, e := ParseContext([]byte(good)); e != nil {
		t.Fatal(e)
	}
	for _, number := range []string{"9007199254740992", "-1", "1.5"} {
		raw := fmt.Sprintf(`{"version":1,"available":true,"session_id":"ws","stream_id":"g","emitted":%s,"settled":0,"loss_count":0}`, number)
		if _, e := ParseContext([]byte(raw)); e == nil {
			t.Fatal(raw)
		}
	}
	c, e := ParseContext([]byte(`{"version":1,"available":false,"reason":"generation_changed","session_id":"ws"}`))
	if e != nil || c.Session != "ws" {
		t.Fatal(c, e)
	}
}
func TestMeasuredZeroAndLegacyRoundTrip(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	apply(t, s)
	p, e := s.Grade("p", "passed", 3)
	if e != nil || p.ObservedCalls == nil || *p.ObservedCalls != 0 {
		t.Fatal(p, e)
	}
	p.Measurement = nil
	p.ObservedCalls = nil
	p.HostCalibration = "unavailable"
	if e = Save(k, p); e != nil {
		t.Fatal(e)
	}
	p, e = Load(k, "p")
	if e != nil || p.HostCalibration != "unavailable" {
		t.Fatal(p, e)
	}
}

func TestInvalidJSONAndConflictingClosingInputs(t *testing.T) {
	for _, raw := range []string{`{"version":1,"version":1}`, `{"mode":"begin","observed_calls":1}`, `{"mode":"status","sequence":1}`} {
		if ValidatePublic([]byte(raw)) == nil {
			t.Fatal(raw)
		}
	}
	if _, err := ParseEvent([]byte(`{"stream_id":"g","sequence":1,"topic":"tool.called","at":"2026-09-24T00:00:00Z","payload":{"tool":"read","failed":null,"duration_ms":1,"actor":"main","session":"ws"}}`)); err == nil {
		t.Fatal("null required payload field")
	}
	s, _ := setup(t)
	begin(t, s, "p")
	s.Context.Emitted = 2
	if _, err := s.Grade("p", "passed", 2); !errors.Is(err, ErrNoRegistration) {
		t.Fatal(err)
	}
	apply(t, s)
	if _, err := s.Grade("p", "passed", 2); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if _, err := s.Grade("p", "failed", 3); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

type countedKV struct {
	*memoryKV
	reads, lists          int
	readBytes, writeBytes int
}

func (k *countedKV) Get(key string) (string, bool, error) {
	k.reads++
	value, found, err := k.memoryKV.Get(key)
	k.readBytes += len(value)
	return value, found, err
}
func (k *countedKV) Put(key, value string) (PutResult, error) {
	k.writeBytes += len(value)
	return k.memoryKV.Put(key, value)
}
func (k *countedKV) List(prefix string, limit int) ([]string, bool, error) {
	k.lists++
	return k.memoryKV.List(prefix, limit)
}

func TestObservationWorkAtCapacity(t *testing.T) {
	for _, n := range []int{0, 1, MaxPlans} {
		for _, closedCount := range []int{0, n / 2, n} {
			for _, session := range []string{"ws", ""} {
				t.Run(fmt.Sprintf("plans=%d/closed=%d/session=%q", n, closedCount, session), func(t *testing.T) {
					s, k := setup(t)
					for i := 0; i < n; i++ {
						id := fmt.Sprintf("p%d", i)
						if i < closedCount {
							closed(t, s, id, 3)
						} else {
							begin(t, s, id)
						}
					}
					counted := &countedKV{memoryKV: k}
					s.KV = counted
					before := k.puts
					for i := uint64(1); i <= 64; i++ {
						if err := s.Observe(event(i, session)); err != nil {
							t.Fatal(err)
						}
					}
					reads, writes, lists := 64, 0, 0
					if n > closedCount {
						writes = 64
					}
					if n == 0 {
						reads = 1
						lists = 1
					} else if n == closedCount {
						reads = 0
					}
					if session == "" {
						reads, writes, lists = 0, 0, 0
					}
					if counted.reads != reads || k.puts-before != writes || counted.lists != lists {
						t.Fatalf("reads=%d writes=%d lists=%d; want %d/%d/%d", counted.reads, k.puts-before, counted.lists, reads, writes, lists)
					}
					t.Logf("events=64 reads=%d writes=%d lists=%d logical_read_bytes=%d logical_write_bytes=%d; no timer or background flush", reads, writes, lists, counted.readBytes, counted.writeBytes)
				})
			}
		}
	}
}

func TestRequiredMeasurementFieldsCannotBecomeZero(t *testing.T) {
	for _, value := range []string{"null", "\"0\"", "false"} {
		raw := fmt.Sprintf(`{"version":1,"available":true,"session_id":"ws","stream_id":"g","emitted":%s,"settled":0,"loss_count":0}`, value)
		if _, err := ParseContext([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	s, k := setup(t)
	begin(t, s, "p")
	apply(t, s)
	if _, err := s.Grade("p", "passed", 3); err != nil {
		t.Fatal(err)
	}
	original := k.rows[key("p")]
	for _, name := range []string{"start", "loss_count", "count", "cursor"} {
		var row map[string]any
		json.Unmarshal([]byte(original), &row)
		m := row["measurement"].(map[string]any)
		delete(m, name)
		raw, _ := json.Marshal(row)
		if _, err := Decode(string(raw)); err == nil {
			t.Fatal("missing", name)
		}
	}
	var row map[string]any
	json.Unmarshal([]byte(original), &row)
	delete(row, "measurement")
	delete(row, "observed_calls")
	raw, _ := json.Marshal(row)
	if _, err := Decode(string(raw)); err == nil {
		t.Fatal("legacy retained complete claim")
	}
	row["host_calibration"] = "unavailable"
	raw, _ = json.Marshal(row)
	if _, err := Decode(string(raw)); err != nil {
		t.Fatal("valid legacy", err)
	}
}

type noReadKV struct{ memoryKV }

func (k *noReadKV) Get(string) (string, bool, error)         { panic("unexpected read") }
func (k *noReadKV) List(string, int) ([]string, bool, error) { panic("unexpected list") }
func TestNoSessionObservationHasNoKVActivity(t *testing.T) {
	s := NewService(&noReadKV{})
	if err := s.Observe(event(1, "")); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedFieldNamesAreCaseSensitive(t *testing.T) {
	s, k := setup(t)
	begin(t, s, "p")
	var row map[string]any
	json.Unmarshal([]byte(k.rows[key("p")]), &row)
	row["STATE"] = "open"
	raw, _ := json.Marshal(row)
	if _, err := Decode(string(raw)); err == nil {
		t.Fatal("case-folded duplicate field accepted")
	}
}
