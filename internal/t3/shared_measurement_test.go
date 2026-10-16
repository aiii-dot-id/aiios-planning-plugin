package t3

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestSharedMeasurementCodec(t *testing.T) {
	c := newShared("g1", 8)
	if err := c.reserve("Ws", 8); err != nil {
		t.Fatal(err)
	}
	if err := c.reserve("Ws", 12); err != nil {
		t.Fatal(err)
	}
	raw, err := c.encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeShared(string(raw))
	if err != nil || !reflect.DeepEqual(c, decoded) {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{`"version":2`, `"version":2,"version":2`}, {`"version":2`, `"version":3`},
		{`"floor":8`, `"floor":8.0`}, {`"floor":8`, `"floor":8e0`}, {`"floor":8`, `"floor":true`},
		{`"floor":8`, `"Floor":8`}, {`"current":[0,null]`, `"Current":[0,null]`},
		{`[8,0,null]`, `[8,0,0]`}, {`[12,null,null]`, `[12,0,null]`},
		{`[12,null,null]`, `[12,null,null,0]`}, {`[8,0,null]`, `[8,0,null],[8,0,null]`},
		{`"through":8`, `"through":9007199254740992`}, {`"current":[0,null]`, `"current":["0",null]`},
		{`"last":null`, `"last":[9,"bad"]`}, {`"sessions":{`, `"extra":0,"sessions":{`},
	} {
		bad := strings.Replace(string(raw), change[0], change[1], 1)
		if bad == string(raw) {
			t.Fatal("mutation missed", change)
		}
		if _, err := decodeShared(bad); !errors.Is(err, ErrEvidence) {
			t.Fatalf("accepted %s: %v", bad, err)
		}
	}
	if _, err := decodeShared(strings.Repeat(" ", MaxRecordBytes) + string(raw)); err == nil {
		t.Fatal("oversize")
	}
	// Conservative encoding envelope, not a claim that simultaneous maximum
	// counter values are reachable. Every variable-width token is at its bound.
	c = newShared(strings.Repeat("s", 128), MaxCounter)
	c.Last = &deliveryFingerprint{MaxCounter, strings.Repeat("f", 64)}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("%03d", i) + strings.Repeat("s", 125)
		c.Sessions[id] = &sessionMeasurement{Current: prefix{MaxCounter, uintPtr(MaxCounter)}, Checkpoints: []checkpoint{{MaxCounter - uint64(2*i) - 1, uintPtr(MaxCounter), uintPtr(MaxCounter)}, {MaxCounter - uint64(2*i), uintPtr(MaxCounter), uintPtr(MaxCounter)}}}
	}
	raw, err = json.Marshal(c)
	if err != nil || len(raw) != 60514 || len(raw) > MaxRecordBytes {
		t.Fatal(len(raw), err)
	}
	// A reachable full working set must pass validation too.
	c = newShared("g1", 0)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("s%d", i)
		if err := c.reserve(id, 0); err != nil {
			t.Fatal(err)
		}
		if err := c.reserve(id, 200); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		e := event(uint64(i+1), fmt.Sprintf("s%d", i))
		if err := c.advance(e.Sequence, &e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.encode(); err != nil {
		t.Fatal(err)
	}
	if err := c.reserve("s0", 201); err == nil {
		t.Fatal("401st checkpoint accepted")
	}
}

func TestSharedMeasurementOracle(t *testing.T) {
	// Fixed PCG generator and draw order. The oracle filters the original event
	// list; it never calls checkpoint arithmetic to obtain expected results.
	rng := rand.New(rand.NewPCG(92426, 2))
	intervals := 0
	for trial := 0; trial < 120; trial++ {
		c := newShared("gen1", 0)
		events := []Event{}
		plans := []*Plan{}
		pending := []int{}
		delivered := 0
		settle := func(n int) {
			t.Helper()
			if err := c.settle(HostContext{Version: 1, Available: true, Stream: "gen1", Emitted: uint64(n), Settled: uint64(delivered)}); err != nil {
				t.Fatal(trial, n, err)
			}
		}
		for n := 1; n <= 240; n++ {
			sessions := []string{"S0", "s1", "s2", "unused", ""}
			events = append(events, event(uint64(n), sessions[rng.IntN(len(sessions))]))
			if len(plans) < 80 && rng.IntN(100) < 30 {
				settle(n)
				who := sessions[rng.IntN(3)]
				if err := c.reserve(who, uint64(n)); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, &Plan{SessionID: who, Measurement: &Measurement{Stream: "gen1", Start: uint64(n)}})
				pending = append(pending, len(plans)-1)
			}
			if len(pending) > 0 && rng.IntN(100) < 27 {
				j := rng.IntN(len(pending))
				p := plans[pending[j]]
				pending = append(pending[:j], pending[j+1:]...)
				settle(n)
				p.Measurement.End = uintPtr(uint64(n))
				if err := c.reserve(p.SessionID, uint64(n)); err != nil {
					t.Fatal(err)
				}
			}
			if rng.IntN(100) < 38 {
				target := delivered + rng.IntN(n-delivered+1)
				for k := delivered; k < target; k++ {
					e := events[k]
					if e.Payload.Session != "" {
						if err := c.advance(e.Sequence, &e); err != nil {
							t.Fatal(err)
						}
					}
				}
				delivered = target
			}
		}
		for _, i := range pending {
			p := plans[i]
			p.Measurement.End = uintPtr(240)
			if err := c.reserve(p.SessionID, 240); err != nil {
				t.Fatal(err)
			}
		}
		for k := delivered; k < len(events); k++ {
			e := events[k]
			if e.Payload.Session != "" {
				if err := c.advance(e.Sequence, &e); err != nil {
					t.Fatal(err)
				}
			}
		}
		delivered = 240
		settle(240)
		for _, p := range plans {
			var count uint64
			cursor := p.Measurement.Start
			for _, e := range events {
				if e.Payload.Session == p.SessionID && e.Sequence > p.Measurement.Start && e.Sequence <= *p.Measurement.End {
					count++
					cursor = e.Sequence
				}
			}
			got, at, known, err := c.snapshot(p)
			if err != nil || !known || got != count || at != cursor {
				t.Fatalf("trial=%d plan=%+v got=%d/%d known=%v want=%d/%d err=%v", trial, p, got, at, known, count, cursor, err)
			}
			intervals++
		}
	}
	t.Logf("PCG(92426,2): 120 schedules; %d intervals matched count and cursor", intervals)
}

func TestSharedMeasurementEmptyTailAndCursor(t *testing.T) {
	s, k := setup(t)
	s.Context.Emitted = 5
	s.Context.Settled = 4
	begin(t, s, "p")
	apply(t, s)
	s.Context.Emitted = 8
	if _, err := s.Grade("p", "passed", 3); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	before := k.puts
	for n := uint64(6); n <= 8; n++ {
		if err := s.Observe(event(n, "")); err != nil {
			t.Fatal(err)
		}
	}
	if k.puts != before {
		t.Fatal("empty tail wrote")
	}
	s.Context.Settled = 8
	p, err := s.Grade("p", "passed", 4)
	if err != nil || p.ObservedCalls == nil || *p.ObservedCalls != 0 || p.Measurement.Cursor != 5 {
		t.Fatal(p, err)
	}
	for _, last := range []uint64{1, 2} {
		s, _ := setup(t)
		begin(t, s, "p")
		apply(t, s)
		for n := uint64(1); n <= 3; n++ {
			who := "other"
			if n == last {
				who = "ws"
			}
			if err := s.Observe(event(n, who)); err != nil {
				t.Fatal(err)
			}
		}
		s.Context.Emitted = 3
		s.Context.Settled = 3
		p, err := s.Grade("p", "passed", 4)
		if err != nil || *p.ObservedCalls != 1 || p.Measurement.Cursor != last {
			t.Fatal(last, p, err)
		}
	}
}

func TestSharedMeasurementReadiness(t *testing.T) {
	for _, broken := range []string{"missing", "corrupt"} {
		t.Run(broken, func(t *testing.T) {
			s, k := setup(t)
			begin(t, s, "p")
			if broken == "missing" {
				delete(k.rows, CalibrationKey)
			} else {
				k.rows[CalibrationKey] = `{"version":2}`
			}
			s = NewService(k)
			s.Context = HostContext{Version: 1, Available: true, Session: "ws", Stream: "gen1"}
			if err := s.Observe(event(1, "ws")); !errors.Is(err, ErrEvidence) {
				t.Fatal(err)
			}
			p, err := s.Material("p")
			if err != nil || p.Measurement.Known {
				t.Fatal(p, err)
			}
			apply(t, s)
			p, err = s.Grade("p", "passed", 3)
			if err != nil || p.HostCalibration != "unresolved" || p.ObservedCalls != nil || p.Measurement.Known {
				t.Fatal(p, err)
			}
			if broken == "corrupt" && k.rows[CalibrationKey] != `{"version":2}` {
				t.Fatal("corrupt source overwritten")
			}
			if _, err = s.Material("p"); err != nil {
				t.Fatal("closed result blocked", err)
			}
		})
	}
}

func TestSharedMeasurementLegacy(t *testing.T) {
	s, k := setup(t)
	// Actual legacy serialization with a matched-event fingerprint, rather than
	// removing version fields from a v2 shared record and calling it legacy.
	p := &Plan{ID: "old", Family: "f", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 10, State: "closed", OpenedMs: 1, ClosedMs: 3, HostCalibration: "complete", Steps: []Step{{ID: "s1", State: "applied", Applied: 1}}, ObservedCalls: uintPtr(1), Measurement: &Measurement{Stream: "old", Start: 0, Count: 1, Cursor: 1, LastHash: strings.Repeat("a", 64), End: uintPtr(1), Result: "passed"}}
	if err := Save(k, p); err != nil {
		t.Fatal(err)
	}
	raw := k.rows[key("old")]
	got, err := s.Material("old")
	if err != nil || got.Measurement.Version != 0 || *got.ObservedCalls != 1 {
		t.Fatal(got, err)
	}
	round, err := Encode(got)
	if err != nil || string(round) != raw {
		t.Fatal("legacy bytes changed", err)
	}
	begin(t, s, "p")
	apply(t, s)
	got, err = s.Grade("p", "passed", 4)
	if err != nil {
		t.Fatal(err)
	}
	raw = k.rows[key("p")]
	for _, bad := range []string{strings.Replace(raw, `"version":2`, `"version":null`, 1), strings.Replace(raw, `"version":2`, `"version":3`, 1), strings.Replace(raw, `"count":0`, `"count":null`, 1)} {
		if _, err := Decode(bad); err == nil {
			t.Fatal("invalid union accepted", bad)
		}
	}
}
