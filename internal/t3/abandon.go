package t3

import "errors"

func (s *Service) Abandon(id string, nowMs int64) (*Plan, error) {
	if s.Context.Version != 1 || !validToken(s.Context.Session, MaxIDBytes) {
		return nil, ErrSession
	}
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if p.State == "closed" {
		if p.Abandoned {
			return p, nil
		}
		return nil, ErrClosed
	}
	m := p.Measurement
	if m != nil && m.Version == 2 && m.Storage == "shared" {
		c, err := s.loadShared()
		if err != nil {
			return nil, err
		}
		m.Known, m.Count, m.Cursor = false, 0, 0
		if c != nil && c.Stream == m.Stream {
			if err = inlineSnapshot(p, c); err != nil && !errors.Is(err, ErrEvidence) {
				return nil, err
			}

			if !m.Known && m.End != nil && c.Through < *m.End {
				partial, partialMeasurement := *p, *m
				partialMeasurement.End = nil
				partial.Measurement = &partialMeasurement
				if err = inlineSnapshot(&partial, c); err != nil && !errors.Is(err, ErrEvidence) {
					return nil, err
				}
				m.Known, m.Count, m.Cursor = partialMeasurement.Known, partialMeasurement.Count, partialMeasurement.Cursor
			}
		}
		m.Storage = "inline"
	}
	p.State, p.HostCalibration, p.ClosedMs, p.Abandoned = "closed", "unresolved", nowMs, true
	p.GradeClass, p.ObservedCalls = "", nil

	s.refs = nil
	if err := s.save(p); err != nil {
		return nil, err
	}
	return p, nil
}
