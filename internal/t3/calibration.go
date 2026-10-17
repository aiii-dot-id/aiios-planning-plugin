package t3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const MaxCounter uint64 = 1<<53 - 1

var (
	ErrPending     = errors.New("calibration: delivery pending for the frozen interval")
	ErrUnavailable = errors.New("calibration: persistent measurement unavailable")
	ErrSession     = errors.New("calibration: acting session does not match the bound session")
)

type HostContext struct {
	Version   int    `json:"version"`
	Available bool   `json:"available"`
	Session   string `json:"session_id,omitempty"`
	Stream    string `json:"stream_id,omitempty"`
	Emitted   uint64 `json:"emitted,omitempty"`
	Settled   uint64 `json:"settled,omitempty"`
	Loss      uint64 `json:"loss_count,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func ParseContext(raw []byte) (HostContext, error) {
	var c HostContext
	if strict(raw, &c) != nil || c.Version != 1 || requiredFields(raw, "version", "available") != nil {
		return c, ErrUnavailable
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if c.Available {
		if requiredFields(raw, "session_id", "stream_id", "emitted", "settled", "loss_count") != nil {
			return c, ErrUnavailable
		}
		for _, k := range []string{"version", "available", "session_id", "stream_id", "emitted", "settled", "loss_count"} {
			if _, ok := fields[k]; !ok {
				return c, ErrUnavailable
			}
		}
		if len(fields) != 7 || !validToken(c.Session, MaxIDBytes) || !validToken(c.Stream, MaxIDBytes) || c.Settled > c.Emitted || c.Emitted > MaxCounter || c.Loss > MaxCounter {
			return c, ErrUnavailable
		}
	} else {
		if _, ok := fields["available"]; !ok {
			return c, ErrUnavailable
		}
		switch c.Reason {
		case "no_session", "session_lookup_failed":
			if c.Session != "" {
				return c, ErrUnavailable
			}
		case "not_ready", "generation_changed", "unsupported_runtime", "counter_exhausted":
			if !validToken(c.Session, MaxIDBytes) {
				return c, ErrUnavailable
			}
		default:
			return c, ErrUnavailable
		}
		for k := range fields {
			if k != "version" && k != "available" && k != "session_id" && k != "reason" {
				return c, ErrUnavailable
			}
		}
	}
	return c, nil
}

type Measurement struct {
	Version  int     `json:"-"`
	Storage  string  `json:"-"`
	Known    bool    `json:"-"`
	Stream   string  `json:"stream_id"`
	Start    uint64  `json:"start"`
	Loss     uint64  `json:"loss_count"`
	Count    uint64  `json:"count"`
	Cursor   uint64  `json:"cursor"`
	LastHash string  `json:"last_hash,omitempty"`
	End      *uint64 `json:"end,omitempty"`
	Result   string  `json:"result_class,omitempty"`
}

func (m *Measurement) valid(p *Plan) error {
	if m.Version != 0 {
		return m.validV2(p)
	}
	if !validToken(m.Stream, MaxIDBytes) || m.Start > MaxCounter || m.Loss > MaxCounter || m.Cursor > MaxCounter || m.Cursor < m.Start || m.Count > m.Cursor-m.Start {
		return ErrInvalid
	}
	if (m.Cursor == m.Start && (m.LastHash != "" || m.Count != 0)) || (m.Cursor > m.Start && len(m.LastHash) != 64) {
		return ErrInvalid
	}
	if m.Cursor > m.Start && m.Count == 0 {
		return ErrInvalid
	}
	if m.LastHash != "" {
		if _, err := hex.DecodeString(m.LastHash); err != nil {
			return ErrInvalid
		}
	}
	if m.End != nil {
		if *m.End < m.Start || *m.End > MaxCounter || m.Cursor > *m.End || !validToken(m.Result, MaxClassBytes) {
			return ErrInvalid
		}
	} else if m.Result != "" && p.State != "closed" {
		return ErrInvalid
	}
	if p.HostCalibration == "complete" {
		if p.State != "closed" || m.End == nil || p.ObservedCalls == nil || *p.ObservedCalls != m.Count {
			return ErrInvalid
		}
	} else if p.ObservedCalls != nil {
		return ErrInvalid
	}
	if p.State == "open" {
		if (m.End == nil && p.HostCalibration != "provisional") || (m.End != nil && p.HostCalibration != "pending") {
			return ErrInvalid
		}
	} else {
		if p.HostCalibration != "complete" && p.HostCalibration != "unresolved" {
			return ErrInvalid
		}
		if !p.Abandoned && !validToken(m.Result, MaxClassBytes) {
			return ErrInvalid
		}
	}
	switch p.HostCalibration {
	case "provisional", "pending", "unresolved", "complete":
	default:
		return ErrInvalid
	}
	return nil
}

type Event struct {
	Stream   string          `json:"stream_id"`
	Sequence uint64          `json:"sequence"`
	Topic    string          `json:"topic"`
	At       string          `json:"at"`
	Payload  EventPayload    `json:"payload"`
	HostNow  json.RawMessage `json:"_host_now_ms,omitempty"`
}
type EventPayload struct {
	Tool     string `json:"tool"`
	Failed   bool   `json:"failed"`
	Duration int64  `json:"duration_ms"`
	Actor    string `json:"actor"`
	Session  string `json:"session"`
	Act      string `json:"act,omitempty"`
}

func ParseEvent(raw []byte) (Event, error) {
	var e Event
	if strict(raw, &e) != nil {
		return e, ErrInvalid
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(raw, &root)
	for _, k := range []string{"stream_id", "sequence", "topic", "at", "payload"} {
		if v, ok := root[k]; !ok || string(v) == "null" {
			return e, ErrInvalid
		}
	}
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(root["payload"], &payload)
	for _, k := range []string{"tool", "failed", "duration_ms", "actor", "session"} {
		if v, ok := payload[k]; !ok || string(v) == "null" {
			return e, ErrInvalid
		}
	}
	return e, e.valid()
}
func (e Event) valid() error {
	if !validToken(e.Stream, MaxIDBytes) || e.Sequence == 0 || e.Sequence > MaxCounter || e.Topic != "tool.called" || len(e.At) > 64 {
		return ErrInvalid
	}
	if _, err := time.Parse(time.RFC3339Nano, e.At); err != nil {
		return ErrInvalid
	}
	p := e.Payload
	if !validToken(p.Tool, 256) || p.Duration < 0 || uint64(p.Duration) > MaxCounter || (p.Session != "" && !validToken(p.Session, MaxIDBytes)) || (p.Act != "" && !validToken(p.Act, MaxIDBytes)) {
		return ErrInvalid
	}
	switch p.Actor {
	case "main", "safe", "subagent", "operator":
	default:
		return ErrInvalid
	}
	return nil
}
func (e Event) digest() string {
	e.HostNow = nil
	b, _ := json.Marshal(e)
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}
func (s *Service) session(p *Plan) error {
	if s.Context.Version != 1 || s.Context.Session == "" || s.Context.Session != p.SessionID {
		return ErrSession
	}
	return nil
}

func ValidatePublic(raw []byte) error {
	if uniqueJSON(raw) != nil {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ErrInvalid
	}
	for key, value := range fields {
		if len(key) < 5 || key[:5] != "_host" {
			if string(value) == "null" {
				return fmt.Errorf("%w: %s must not be null", ErrInvalid, key)
			}
		}
	}
	if value, present := fields["plan_id"]; present {
		var id string
		if json.Unmarshal(value, &id) != nil || !validToken(id, MaxIDBytes) {
			return fmt.Errorf("%w: plan_id must be 1..128 ASCII letters, digits, or ._:-", ErrInvalid)
		}
	}
	mode := "material"
	if value, present := fields["mode"]; present {
		if json.Unmarshal(value, &mode) != nil {
			return ErrInvalid
		}
	}
	allowed := map[string]bool{"mode": true, "plan_id": true, "_host_calibration": true, "_host_now_ms": true}
	var keys []string
	switch mode {
	case "begin":
		keys = []string{"family", "session_id", "falsifier_class", "expected_evidence_class", "consequence_class", "declared_steps", "budget_calls"}
	case "apply":
		keys = []string{"apply_id", "step_id", "kind_class", "from_class", "to_class", "count"}
	case "grade":
		keys = []string{"result_class"}
	case "advise":
		keys = []string{"control_class", "history_cursor"}
	case "material", "status":
		if _, hasID := fields["plan_id"]; !hasID {
			keys = []string{"view"}
			if raw, present := fields["view"]; present {
				var view string
				if json.Unmarshal(raw, &view) != nil || (view != "compact" && view != "full") {
					return ErrInvalid
				}
			}
		}
	case "abandon":
	default:
		return ErrInvalid
	}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range fields {
		if !allowed[k] {
			return fmt.Errorf("%w: field %q is not accepted by mode %q; use only that mode's documented inputs", ErrInvalid, k, mode)
		}
	}
	return nil
}
