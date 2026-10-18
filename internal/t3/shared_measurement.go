package t3

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const CalibrationKey = "t3/calibration"

var ErrEvidence = errors.New("calibration: required measurement evidence is missing or invalid")

func evidenceError() error { return errors.Join(ErrUnavailable, ErrEvidence) }
func number(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
func copyNumber(p *uint64) *uint64 {
	if p == nil {
		return nil
	}
	n := *p
	return &n
}
func sameNumber(a, b *uint64) bool { return (a == nil) == (b == nil) && number(a) == number(b) }
func uintPtr(n uint64) *uint64     { return &n }

type prefix struct {
	Count uint64
	Last  *uint64
}

func (p prefix) MarshalJSON() ([]byte, error) { return json.Marshal([2]any{p.Count, p.Last}) }
func (p *prefix) UnmarshalJSON(raw []byte) error {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil || len(a) != 2 {
		return ErrInvalid
	}
	if err := unsigned(a[0], &p.Count); err != nil {
		return err
	}
	return nullableUnsigned(a[1], &p.Last)
}

type checkpoint struct {
	Boundary    uint64
	Count, Last *uint64
}

func (p checkpoint) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]any{p.Boundary, p.Count, p.Last})
}
func (p *checkpoint) UnmarshalJSON(raw []byte) error {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil || len(a) != 3 {
		return ErrInvalid
	}
	if err := unsigned(a[0], &p.Boundary); err != nil {
		return err
	}
	if err := nullableUnsigned(a[1], &p.Count); err != nil {
		return err
	}
	return nullableUnsigned(a[2], &p.Last)
}

type deliveryFingerprint struct {
	Sequence uint64
	Hash     string
}

func (p deliveryFingerprint) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{p.Sequence, p.Hash})
}
func (p *deliveryFingerprint) UnmarshalJSON(raw []byte) error {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil || len(a) != 2 {
		return ErrInvalid
	}
	if err := unsigned(a[0], &p.Sequence); err != nil {
		return err
	}
	if json.Unmarshal(a[1], &p.Hash) != nil {
		return ErrInvalid
	}
	return nil
}

type sessionMeasurement struct {
	Current     prefix       `json:"current"`
	Checkpoints []checkpoint `json:"checkpoints"`
}
type sharedMeasurement struct {
	Version  uint64                         `json:"version"`
	Stream   string                         `json:"stream_id"`
	Floor    uint64                         `json:"floor"`
	Through  uint64                         `json:"through"`
	Last     *deliveryFingerprint           `json:"last"`
	Sessions map[string]*sessionMeasurement `json:"sessions"`
}

func unsigned(raw []byte, out *uint64) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, out) != nil || *out > MaxCounter {
		return ErrInvalid
	}
	return nil
}
func nullableUnsigned(raw []byte, out **uint64) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*out = nil
		return nil
	}
	var n uint64
	if err := unsigned(raw, &n); err != nil {
		return err
	}
	*out = &n
	return nil
}
func exactObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil || v == nil || len(v) != len(names) {
		return nil, ErrInvalid
	}
	for _, n := range names {
		if _, ok := v[n]; !ok {
			return nil, ErrInvalid
		}
	}
	return v, nil
}
func decodeShared(raw string) (*sharedMeasurement, error) {
	if len(raw) > MaxRecordBytes || uniqueJSONKeys([]byte(raw), false) != nil {
		return nil, evidenceError()
	}
	fields, err := exactObject([]byte(raw), "version", "stream_id", "floor", "through", "last", "sessions")
	if err != nil {
		return nil, evidenceError()
	}
	if requiredFields([]byte(raw), "version", "stream_id", "floor", "through", "sessions") != nil {
		return nil, evidenceError()
	}
	var sessions map[string]json.RawMessage
	if json.Unmarshal(fields["sessions"], &sessions) != nil {
		return nil, evidenceError()
	}
	for _, v := range sessions {
		if _, err := exactObject(v, "current", "checkpoints"); err != nil {
			return nil, evidenceError()
		}
	}
	var s sharedMeasurement
	if json.Unmarshal([]byte(raw), &s) != nil || s.valid() != nil {
		return nil, evidenceError()
	}
	return &s, nil
}
func newShared(stream string, floor uint64) *sharedMeasurement {
	return &sharedMeasurement{Version: 2, Stream: stream, Floor: floor, Through: floor, Sessions: map[string]*sessionMeasurement{}}
}
func (p prefix) valid(floor, through uint64) bool {
	if p.Count == 0 {
		return p.Last == nil
	}
	return p.Count <= MaxCounter && p.Last != nil && *p.Last > floor && *p.Last <= through && p.Count <= *p.Last-floor
}
func (s *sharedMeasurement) valid() error {
	if s.Version != 2 || !validToken(s.Stream, MaxIDBytes) || s.Floor > s.Through || s.Through > MaxCounter || s.Sessions == nil || len(s.Sessions) > MaxPlans {
		return ErrInvalid
	}
	if s.Last != nil {
		if s.Last.Sequence <= s.Floor || s.Last.Sequence > s.Through || len(s.Last.Hash) != 64 {
			return ErrInvalid
		}
		for _, c := range s.Last.Hash {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return ErrInvalid
			}
		}
		if _, err := hex.DecodeString(s.Last.Hash); err != nil {
			return ErrInvalid
		}
	}
	pins := 0
	var sum uint64
	for id, v := range s.Sessions {
		if !validToken(id, MaxIDBytes) || v == nil || !v.Current.valid(s.Floor, s.Through) || len(v.Checkpoints) == 0 {
			return ErrInvalid
		}
		if v.Current.Count > 0 && (s.Last == nil || *v.Current.Last > s.Last.Sequence) {
			return ErrInvalid
		}
		if v.Current.Count > s.Through-s.Floor-sum {
			return ErrInvalid
		}
		sum += v.Current.Count
		var previous *prefix
		for i, c := range v.Checkpoints {
			pins++
			if c.Boundary < s.Floor || c.Boundary > MaxCounter || (i > 0 && c.Boundary <= v.Checkpoints[i-1].Boundary) {
				return ErrInvalid
			}
			if c.Boundary > s.Through {
				if c.Count != nil || c.Last != nil {
					return ErrInvalid
				}
				continue
			}
			if c.Count == nil {
				return ErrInvalid
			}
			p := prefix{*c.Count, c.Last}
			if !p.valid(s.Floor, c.Boundary) || p.Count > v.Current.Count {
				return ErrInvalid
			}
			if p.Count == v.Current.Count && !sameNumber(p.Last, v.Current.Last) {
				return ErrInvalid
			}
			if p.Count > 0 && p.Count < v.Current.Count && *p.Last >= *v.Current.Last {
				return ErrInvalid
			}
			if previous != nil && (previous.Count > p.Count || previous.Count == p.Count && !sameNumber(previous.Last, p.Last) || previous.Count > 0 && previous.Count < p.Count && *previous.Last >= *p.Last) {
				return ErrInvalid
			}
			previous = &p
		}
	}
	if pins > 2*MaxPlans {
		return ErrInvalid
	}
	return nil
}
func (s *sharedMeasurement) encode() ([]byte, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if len(raw) > MaxRecordBytes {
		return nil, ErrCapacity
	}
	return raw, err
}
func (s *sharedMeasurement) point(session string, b uint64) (*checkpoint, error) {
	v := s.Sessions[session]
	if v == nil {
		return nil, evidenceError()
	}
	i := sort.Search(len(v.Checkpoints), func(i int) bool { return v.Checkpoints[i].Boundary >= b })
	if i == len(v.Checkpoints) || v.Checkpoints[i].Boundary != b {
		return nil, evidenceError()
	}
	return &v.Checkpoints[i], nil
}
func (s *sharedMeasurement) reserve(session string, b uint64) error {
	if p, err := s.point(session, b); err == nil && p != nil {
		return nil
	}
	if !validToken(session, MaxIDBytes) || b < s.Through || b > MaxCounter {
		return ErrInvalid
	}
	v := s.Sessions[session]
	if v == nil {
		v = &sessionMeasurement{Checkpoints: []checkpoint{}}
		s.Sessions[session] = v
	}
	p := checkpoint{Boundary: b}
	if b == s.Through {
		p.Count = uintPtr(v.Current.Count)
		p.Last = copyNumber(v.Current.Last)
	}
	v.Checkpoints = append(v.Checkpoints, p)
	sort.Slice(v.Checkpoints, func(i, j int) bool { return v.Checkpoints[i].Boundary < v.Checkpoints[j].Boundary })
	return s.valid()
}
func setCheckpoint(c *checkpoint, p prefix) { c.Count = uintPtr(p.Count); c.Last = copyNumber(p.Last) }
func (s *sharedMeasurement) advance(n uint64, e *Event) error {
	if n <= s.Through || n > MaxCounter {
		return ErrInvalid
	}
	for _, v := range s.Sessions {
		for i := range v.Checkpoints {
			p := &v.Checkpoints[i]
			if p.Count == nil && p.Boundary < n {
				setCheckpoint(p, v.Current)
			}
		}
	}
	if e != nil {
		if v := s.Sessions[e.Payload.Session]; v != nil {
			if v.Current.Count == MaxCounter {
				return ErrUnavailable
			}
			v.Current.Count++
			v.Current.Last = uintPtr(n)
		}
		s.Last = &deliveryFingerprint{n, e.digest()}
	}
	for _, v := range s.Sessions {
		for i := range v.Checkpoints {
			p := &v.Checkpoints[i]
			if p.Count == nil && p.Boundary == n {
				setCheckpoint(p, v.Current)
			}
		}
	}
	s.Through = n
	return s.valid()
}
func (s *sharedMeasurement) settle(c HostContext) error {
	if !c.Available || c.Stream != s.Stream || c.Settled > c.Emitted || c.Emitted > MaxCounter || s.Through > c.Emitted {
		return evidenceError()
	}
	if c.Settled > s.Through {
		return s.advance(c.Settled, nil)
	}
	return nil
}
func (s *sharedMeasurement) snapshot(p *Plan) (uint64, uint64, bool, error) {
	m := p.Measurement
	if m == nil || m.Stream != s.Stream {
		return 0, 0, false, evidenceError()
	}
	a, err := s.point(p.SessionID, m.Start)
	if err != nil {
		return 0, 0, false, err
	}
	if a.Count == nil {
		return 0, 0, false, nil
	}
	v := s.Sessions[p.SessionID]
	end := prefix{v.Current.Count, v.Current.Last}
	if m.End != nil {
		b, err := s.point(p.SessionID, *m.End)
		if err != nil {
			return 0, 0, false, err
		}
		if b.Count == nil {
			return 0, 0, false, nil
		}
		end = prefix{*b.Count, b.Last}
	}
	if end.Count < *a.Count {
		return 0, 0, false, evidenceError()
	}
	n := end.Count - *a.Count
	cursor := m.Start
	if n > 0 {
		if end.Last == nil || *end.Last <= m.Start {
			return 0, 0, false, evidenceError()
		}
		cursor = *end.Last
	}
	if n > cursor-m.Start || (m.End != nil && cursor > *m.End) {
		return 0, 0, false, evidenceError()
	}
	return n, cursor, true, nil
}
