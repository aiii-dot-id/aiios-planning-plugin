package t3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	KeyPrefix      = "t3/plan/"
	MaxIDBytes     = 128
	MaxClassBytes  = 96
	MaxPlans       = 200
	MaxApplies     = 512
	MaxAdvice      = 50
	MaxRecordBytes = 60 * 1024
)

var (
	ErrInvalid        = errors.New("t3: invalid argument")
	ErrNotFound       = errors.New("t3: plan not found")
	ErrExists         = errors.New("t3: plan already exists")
	ErrClosed         = errors.New("t3: plan is closed")
	ErrNoRegistration = errors.New("t3: pre-registration is incomplete")
)

type KV interface {
	Get(key string) (string, bool, error)
	Put(key, value string) (PutResult, error)
	Delete(key string) (bool, error)
	List(prefix string, limit int) ([]string, bool, error)
}

func Save(kv KV, p *Plan) error {
	raw, err := Encode(p)
	if err != nil {
		return err
	}
	result, err := kv.Put(key(p.ID), string(raw))
	if err != nil {
		return err
	}
	if result.Scope != "persistent" || !result.Stored || result.Key != key(p.ID) || result.ValueBytes != len(raw) {
		return ErrUnavailable
	}
	return nil
}
func Load(kv KV, id string) (*Plan, error) {
	if !validToken(id, MaxIDBytes) {
		return nil, ErrInvalid
	}
	raw, found, err := kv.Get(key(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return decodeKey(key(id), raw)
}
func Exists(kv KV, id string) (bool, error) {
	if !validToken(id, MaxIDBytes) {
		return false, ErrInvalid
	}
	_, found, err := kv.Get(key(id))
	return found, err
}
func ByFamily(kv KV, family string, limit int) ([]*Plan, error) {
	if !validToken(family, MaxIDBytes) || limit < 1 {
		return nil, ErrInvalid
	}
	keys, truncated, err := kv.List(KeyPrefix, MaxPlans)
	if truncated {
		return nil, ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Plan, 0, len(keys))
	for _, k := range keys {
		raw, found, err := kv.Get(k)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		p, err := decodeKey(k, raw)
		if err != nil {
			return nil, err
		}
		if p.Family == family {
			out = append(out, p)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type PutResult struct {
	Stored     bool
	Key        string
	ValueBytes int
	Scope      string
}

func (r PutResult) GetScope() string { return r.Scope }

type Step struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Applied int    `json:"applied"`
}
type Apply struct {
	ID        string `json:"id"`
	StepID    string `json:"step_id"`
	KindClass string `json:"kind_class"`
	FromClass string `json:"from_class"`
	ToClass   string `json:"to_class"`
	Count     int    `json:"count"`
	AtMs      int64  `json:"at_ms"`
}
type Advice struct {
	HistoryTruncated  bool     `json:"history_truncated,omitempty"`
	HistoryNextCursor string   `json:"history_next_cursor,omitempty"`
	ID                string   `json:"id"`
	Family            string   `json:"family"`
	ProposalClass     string   `json:"proposal_class"`
	SourceRows        []string `json:"source_rows"`
	SampleSize        int      `json:"sample_size"`
	CounterCaseClass  string   `json:"counter_case_class"`
	ControlClass      string   `json:"control_class"`
}
type Plan struct {
	archiveHash           string
	Measurement           *Measurement `json:"measurement,omitempty"`
	ID                    string       `json:"id"`
	Family                string       `json:"family"`
	SessionID             string       `json:"session_id"`
	FalsifierClass        string       `json:"falsifier_class"`
	ExpectedEvidenceClass string       `json:"expected_evidence_class"`
	ConsequenceClass      string       `json:"consequence_class"`
	DeclaredSteps         int          `json:"declared_steps"`
	BudgetCalls           int          `json:"budget_calls"`
	ObservedCalls         *uint64      `json:"observed_calls,omitempty"`
	Steps                 []Step       `json:"steps"`
	Applies               []Apply      `json:"applies"`
	OpenedMs              int64        `json:"opened_ms"`
	ClosedMs              int64        `json:"closed_ms,omitempty"`
	State                 string       `json:"state"`
	Abandoned             bool         `json:"abandoned,omitempty"`
	GradeClass            string       `json:"grade_class,omitempty"`
	StepCountOK           *bool        `json:"step_count_ok,omitempty"`
	FalsifierOK           *bool        `json:"falsifier_ok,omitempty"`
	HostCalibration       string       `json:"host_calibration"`
	Advice                []Advice     `json:"advice,omitempty"`
}

func validToken(v string, max int) bool {
	if len(v) == 0 || len(v) > max {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return false
	}
	return true
}
func Count(v int, min, max int) bool { return v >= min && v <= max }

func ValidBegin(p *Plan) error {
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"plan_id", p.ID, MaxIDBytes}, {"family", p.Family, MaxIDBytes}, {"session_id", p.SessionID, MaxIDBytes},
		{"falsifier_class", p.FalsifierClass, MaxClassBytes}, {"expected_evidence_class", p.ExpectedEvidenceClass, MaxClassBytes}, {"consequence_class", p.ConsequenceClass, MaxClassBytes},
	} {
		if !validToken(field.value, field.max) {
			return fmt.Errorf("%w: %s must be 1..%d ASCII letters, digits, or ._:-; use a short label, not prose", ErrInvalid, field.name, field.max)
		}
	}
	if !Count(p.DeclaredSteps, 1, 256) {
		return fmt.Errorf("%w: declared_steps must be an integer from 1 to 256", ErrInvalid)
	}
	if !Count(p.BudgetCalls, 1, 100000) {
		return fmt.Errorf("%w: budget_calls must be an integer from 1 to 100000", ErrInvalid)
	}
	if p.ObservedCalls != nil {
		return fmt.Errorf("%w: observed_calls is host-owned and cannot be supplied to begin", ErrInvalid)
	}
	return nil
}

func ValidStep(s Step) bool {
	return validToken(s.ID, MaxIDBytes) && (s.State == "planned" || s.State == "applied") && s.Applied >= 0
}
func ValidApply(a Apply) bool {
	return validToken(a.ID, MaxIDBytes) && validToken(a.StepID, MaxIDBytes) && validToken(a.KindClass, MaxClassBytes) && validToken(a.FromClass, MaxClassBytes) && validToken(a.ToClass, MaxClassBytes) && Count(a.Count, 1, 100000)
}
func ValidAdvice(a Advice) bool {
	if !validToken(a.ID, MaxIDBytes) || !validToken(a.Family, MaxIDBytes) || !validToken(a.ProposalClass, MaxClassBytes) || !validToken(a.CounterCaseClass, MaxClassBytes) || !validToken(a.ControlClass, MaxClassBytes) || a.SampleSize < 0 || a.SampleSize > MaxPlans || a.HistoryTruncated != (a.HistoryNextCursor != "") || !validHistoryCursor(a.HistoryNextCursor) || len(a.HistoryNextCursor) > 256 || len(a.SourceRows) > 16 {
		return false
	}
	for _, id := range a.SourceRows {
		if !validToken(id, MaxIDBytes) {
			return false
		}
	}
	return true
}

func key(id string) string { return KeyPrefix + id }
func Encode(p *Plan) ([]byte, error) {
	if err := ValidRecord(p); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if len(b) > MaxRecordBytes {
		return nil, ErrInvalid
	}
	return b, err
}
func Decode(raw string) (*Plan, error) {
	if len(raw) > MaxRecordBytes {
		return nil, ErrInvalid
	}
	var p Plan
	if err := strict([]byte(raw), &p); err != nil {
		return nil, err
	}
	if err := requiredFields([]byte(raw), "id", "family", "session_id", "falsifier_class", "expected_evidence_class", "consequence_class", "declared_steps", "budget_calls", "steps", "opened_ms", "state", "host_calibration"); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &fields)
	for _, name := range []string{"measurement", "observed_calls", "step_count_ok", "falsifier_ok", "closed_ms", "abandoned"} {
		if value, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrInvalid
		}
	}
	if value, ok := fields["measurement"]; ok && p.Measurement.Version == 0 {
		if err := requiredFields(value, "stream_id", "start", "loss_count", "count", "cursor"); err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(value, &m)
		for _, name := range []string{"end", "result_class", "last_hash"} {
			if v, ok := m[name]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return nil, ErrInvalid
			}
		}
	}
	if err := ValidRecord(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
func strict(raw []byte, out any) error {
	if err := uniqueJSON(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func ValidRecord(p *Plan) error {
	base := *p
	base.ObservedCalls = nil
	if err := ValidBegin(&base); err != nil {
		return err
	}
	if p.State != "open" && p.State != "closed" {
		return ErrInvalid
	}
	if p.Abandoned && (p.State != "closed" || p.HostCalibration != "unresolved" || p.GradeClass != "" || p.ObservedCalls != nil) {
		return ErrInvalid
	}
	if len(p.Steps) != p.DeclaredSteps || len(p.Applies) > MaxApplies || len(p.Advice) > MaxAdvice {
		return ErrInvalid
	}
	for _, v := range p.Steps {
		if !ValidStep(v) {
			return ErrInvalid
		}
	}
	for _, v := range p.Applies {
		if !ValidApply(v) {
			return ErrInvalid
		}
	}
	for _, v := range p.Advice {
		if !ValidAdvice(v) {
			return ErrInvalid
		}
	}
	if p.Measurement != nil {
		if err := p.Measurement.valid(p); err != nil {
			return err
		}
	} else if p.ObservedCalls != nil || (p.HostCalibration != "unavailable" && p.HostCalibration != "unresolved") {
		return ErrInvalid
	}
	return nil
}

func uniqueJSON(raw []byte) error { return uniqueJSONKeys(raw, true) }
func uniqueJSONKeys(raw []byte, lower bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return ErrInvalid
		}
		tok, err := d.Token()
		if err != nil {
			return ErrInvalid
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return ErrInvalid
				}
				key, ok := k.(string)
				if !ok || seen[key] || (lower && key != strings.ToLower(key)) {
					return ErrInvalid
				}
				seen[key] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func requiredFields(raw []byte, names ...string) error {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return ErrInvalid
	}
	for _, name := range names {
		value, ok := values[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalid
		}
	}
	return nil
}

func decodeKey(k, raw string) (*Plan, error) {
	p, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	if key(p.ID) != k {
		return nil, ErrInvalid
	}
	return p, nil
}
