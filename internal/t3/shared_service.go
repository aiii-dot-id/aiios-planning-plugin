package t3

import (
	"errors"
)

type measurementRef struct {
	Session, Stream string
	Start           uint64
	End             *uint64
}

func reference(p *Plan) (measurementRef, bool) {
	m := p.Measurement
	if m == nil || m.Version != 2 || m.Storage != "shared" {
		return measurementRef{}, false
	}
	return measurementRef{p.SessionID, m.Stream, m.Start, copyNumber(m.End)}, true
}
func (s *Service) remember(p *Plan) {
	if r, ok := reference(p); ok {
		s.refs[p.ID] = r
	} else {
		delete(s.refs, p.ID)
	}
}
func (s *Service) loadShared() (*sharedMeasurement, error) {
	raw, found, err := s.KV.Get(CalibrationKey)
	if err != nil {
		s.refs = nil
		return nil, err
	}
	if !found {
		return nil, nil
	}
	c, err := decodeShared(raw)
	if err != nil {
		s.refs = nil
	}
	return c, err
}
func (s *Service) saveShared(c *sharedMeasurement) error {
	raw, err := c.encode()
	if err != nil {
		s.refs = nil
		return err
	}
	r, err := s.KV.Put(CalibrationKey, string(raw))
	if err != nil {
		s.refs = nil
		return err
	}
	if !r.Stored || r.Key != CalibrationKey || r.ValueBytes != len(raw) || r.Scope != "persistent" {
		s.refs = nil
		return ErrUnavailable
	}
	return nil
}

func (s *Service) saveSharedPublic(c *sharedMeasurement) error {
	for i := 0; i <= MaxPlans; i++ {
		err := s.saveShared(c)
		if !errors.Is(err, ErrKVQuota) {
			return err
		}
		if err = s.archiveOne(""); err != nil {
			return err
		}
	}
	return ErrCapacity
}
func (s *Service) hotPlans() ([]*Plan, error) {
	keys, more, err := s.KV.List(KeyPrefix, MaxPlans)
	if err != nil {
		return nil, err
	}
	if more {
		return nil, ErrCapacity
	}
	plans := make([]*Plan, 0, len(keys))
	for _, k := range keys {
		raw, found, err := s.KV.Get(k)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, evidenceError()
		}
		p, err := decodeKey(k, raw)
		if err != nil {
			return nil, errors.Join(evidenceError(), err)
		}
		plans = append(plans, p)
	}
	return plans, nil
}
func checkReferences(c *sharedMeasurement, refs map[string]measurementRef) error {
	for _, r := range refs {
		if c == nil || r.Stream != c.Stream {
			return evidenceError()
		}
		if _, err := c.point(r.Session, r.Start); err != nil {
			return err
		}
		if r.End != nil {
			if _, err := c.point(r.Session, *r.End); err != nil {
				return err
			}
		}
	}
	return nil
}
func pruneReferences(c *sharedMeasurement, refs map[string]measurementRef) bool {
	needed := map[string]map[uint64]bool{}
	for _, r := range refs {
		if needed[r.Session] == nil {
			needed[r.Session] = map[uint64]bool{}
		}
		needed[r.Session][r.Start] = true
		if r.End != nil {
			needed[r.Session][*r.End] = true
		}
	}
	changed := false
	for id, v := range c.Sessions {
		kept := v.Checkpoints[:0]
		for _, p := range v.Checkpoints {
			if needed[id][p.Boundary] {
				kept = append(kept, p)
			} else {
				changed = true
			}
		}
		v.Checkpoints = kept
		if len(kept) == 0 {
			delete(c.Sessions, id)
			changed = true
		}
	}
	return changed
}
func (s *Service) reconciled(c *sharedMeasurement) (*sharedMeasurement, error) {
	if s.refs != nil && ((c == nil && s.readyStream == "") || (c != nil && c.Stream == s.readyStream)) {
		if err := checkReferences(c, s.refs); err != nil {
			s.refs = nil
			return nil, err
		}
		return c, nil
	}
	s.refs = nil
	plans, err := s.hotPlans()
	if err != nil {
		return nil, err
	}
	refs := map[string]measurementRef{}
	for _, p := range plans {
		if r, ok := reference(p); ok {
			refs[p.ID] = r
		}
	}
	if err = checkReferences(c, refs); err != nil {
		return nil, err
	}
	if c != nil && pruneReferences(c, refs) {
		if err = s.saveShared(c); err != nil {
			return nil, err
		}
	}
	s.refs = refs
	s.readyStream = ""
	if c != nil {
		s.readyStream = c.Stream
	}
	return c, nil
}
func (s *Service) sharedForBegin() (*sharedMeasurement, error) {
	c, err := s.loadShared()
	if err != nil {
		return nil, err
	}
	c, err = s.reconciled(c)
	if err != nil {
		return nil, err
	}
	if c != nil && c.Stream != s.Context.Stream {
		plans, err := s.hotPlans()
		if err != nil {
			return nil, err
		}
		refs := s.refs
		s.refs = nil
		for _, p := range plans {
			if _, ok := reference(p); !ok {
				continue
			}
			if err := inlineSnapshot(p, c); err != nil {
				return nil, err
			}
			p.HostCalibration = "unresolved"
			if err := s.save(p); err != nil {
				return nil, err
			}
			delete(refs, p.ID)
			if pruneReferences(c, refs) {
				if err := s.saveSharedPublic(c); err != nil {
					return nil, err
				}
			}
		}
		s.refs = refs
	}
	if c == nil || len(s.refs) == 0 {
		c = newShared(s.Context.Stream, s.Context.Emitted)
	}
	if err := c.settle(s.Context); err != nil {
		s.refs = nil
		return nil, err
	}
	return c, nil
}
func inlineSnapshot(p *Plan, c *sharedMeasurement) error {
	m := p.Measurement
	n, at, known, err := c.snapshot(p)
	if err != nil {
		return err
	}
	m.Storage = "inline"
	m.Count = n
	m.Cursor = at
	m.Known = known
	return nil
}
func (s *Service) finishReferences(p *Plan, c *sharedMeasurement, refs map[string]measurementRef) error {
	s.refs = refs
	s.remember(p)
	if c != nil && pruneReferences(c, s.refs) {
		if err := s.saveSharedPublic(c); err != nil {
			return err
		}
	}
	s.refs = refs
	if c != nil {
		s.readyStream = c.Stream
	}
	return nil
}
func (s *Service) Observe(e Event) error {
	if err := e.valid(); err != nil {
		return err
	}
	if e.Payload.Session == "" {
		return nil
	}

	if s.refs != nil && len(s.refs) == 0 {
		return nil
	}
	c, err := s.loadShared()
	if err != nil {
		return err
	}
	c, err = s.reconciled(c)
	if err != nil {
		return err
	}

	if c == nil || c.Stream != e.Stream || len(s.refs) == 0 {
		return nil
	}
	if e.Sequence <= c.Floor {
		return nil
	}
	if c.Last != nil && e.Sequence == c.Last.Sequence {
		if c.Last.Hash != e.digest() {
			return ErrInvalid
		}
		return nil
	}
	if e.Sequence <= c.Through {
		return ErrInvalid
	}
	if err = c.advance(e.Sequence, &e); err != nil {
		return err
	}
	return s.saveShared(c)
}
func (s *Service) viewMeasurement(p *Plan) {
	m := p.Measurement
	if m == nil {
		return
	}
	if m.Version != 2 || m.Storage != "shared" {
		return
	}
	m.Known = false
	m.Count = 0
	m.Cursor = 0
	c, err := s.loadShared()
	if err != nil || c == nil || c.Stream != m.Stream {
		p.HostCalibration = "unresolved"
		return
	}
	if s.Context.Available && c.Stream == s.Context.Stream {
		if c.settle(s.Context) != nil {
			p.HostCalibration = "unresolved"
			return
		}
	}
	n, at, known, err := c.snapshot(p)
	if err != nil {
		p.HostCalibration = "unresolved"
		return
	}
	m.Count = n
	m.Cursor = at
	m.Known = known
}

func (s *Service) gradeMeasurement(p *Plan, result string) (*sharedMeasurement, error) {
	m := p.Measurement
	healthy := m != nil && m.Version == 2 && m.Storage == "shared" && s.Context.Available && m.Stream == s.Context.Stream && m.Loss == s.Context.Loss
	var c *sharedMeasurement
	var err error
	if m != nil && m.Version == 2 && m.Storage == "shared" {
		c, err = s.loadShared()
		if err == nil {
			_, err = s.reconciled(c)
		}
		if err != nil && !errors.Is(err, ErrEvidence) {
			return c, err
		}
		if err != nil || c == nil || c.Stream != m.Stream {
			healthy = false
		}
	}
	if healthy {
		if err = c.settle(s.Context); err != nil {
			return c, err
		}
		if m.End == nil {
			end := s.Context.Emitted
			if err = c.reserve(p.SessionID, end); err != nil {
				return c, err
			}
			m.End = &end
			m.Result = result
			p.HostCalibration = "pending"
			refs := s.refs
			s.refs = nil
			if err = s.saveSharedPublic(c); err != nil {
				return c, err
			}
			if err = s.save(p); err != nil {
				return c, err
			}
			s.refs = refs
			s.remember(p)
		}
		if s.Context.Settled < *m.End {
			return c, ErrPending
		}
		if err = inlineSnapshot(p, c); err != nil {
			return c, err
		}
		if !m.Known {
			return c, evidenceError()
		}
		p.ObservedCalls = uintPtr(m.Count)
		p.HostCalibration = "complete"
	} else {
		p.ObservedCalls = nil
		p.HostCalibration = "unresolved"
		if m != nil {
			if m.Version == 2 && m.Storage == "shared" {
				m.Known = false
				m.Count = 0
				m.Cursor = 0
				if c != nil && c.Stream == m.Stream {
					if err := inlineSnapshot(p, c); err != nil && !errors.Is(err, ErrEvidence) {
						return c, err
					}
				}
				m.Storage = "inline"
			}
			m.Result = result
		}
	}

	return c, nil
}
