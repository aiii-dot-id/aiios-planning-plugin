package t3

import (
	"bytes"
	"encoding/json"
)

type legacyMeasurement Measurement

type measurementV2 struct {
	Version int     `json:"version"`
	Storage string  `json:"storage"`
	Stream  string  `json:"stream_id"`
	Start   uint64  `json:"start"`
	End     *uint64 `json:"end"`
	Loss    uint64  `json:"loss_count"`
	Result  *string `json:"result_class"`
}
type inlineMeasurement struct {
	measurementV2
	Count  *uint64 `json:"count"`
	Cursor *uint64 `json:"cursor"`
}

func (m Measurement) MarshalJSON() ([]byte, error) {
	if m.Version == 0 {
		return json.Marshal(legacyMeasurement(m))
	}
	var result *string
	if m.Result != "" {
		result = &m.Result
	}
	v := measurementV2{m.Version, m.Storage, m.Stream, m.Start, m.End, m.Loss, result}
	if m.Storage == "shared" {
		return json.Marshal(v)
	}
	var count, cursor *uint64
	if m.Known {
		count = &m.Count
		cursor = &m.Cursor
	}
	return json.Marshal(inlineMeasurement{v, count, cursor})
}
func (m *Measurement) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return ErrInvalid
	}
	if _, ok := fields["version"]; !ok {
		var old legacyMeasurement
		if strict(raw, &old) != nil {
			return ErrInvalid
		}
		*m = Measurement(old)
		return nil
	}
	common := []string{"version", "storage", "stream_id", "start", "end", "loss_count", "result_class"}
	if requiredFields(raw, "version", "storage", "stream_id", "start", "loss_count") != nil {
		return ErrInvalid
	}
	var v inlineMeasurement

	for _, k := range common {
		if _, ok := fields[k]; !ok {
			return ErrInvalid
		}
	}
	if strict(raw, &v) != nil || v.Version != 2 {
		return ErrInvalid
	}
	if v.Storage == "shared" {
		if len(fields) != len(common) {
			return ErrInvalid
		}
	} else if v.Storage == "inline" {
		if len(fields) != len(common)+2 {
			return ErrInvalid
		}
		for _, k := range []string{"count", "cursor"} {
			if _, ok := fields[k]; !ok {
				return ErrInvalid
			}
		}
		if (v.Count == nil) != (v.Cursor == nil) {
			return ErrInvalid
		}
	} else {
		return ErrInvalid
	}
	*m = Measurement{Version: 2, Storage: v.Storage, Stream: v.Stream, Start: v.Start, End: v.End, Loss: v.Loss}
	if v.Result != nil {
		if *v.Result == "" {
			return ErrInvalid
		}
		m.Result = *v.Result
	}
	if v.Count != nil {
		m.Count = *v.Count
		m.Cursor = *v.Cursor
		m.Known = true
	}

	for _, k := range []string{"version", "start", "loss_count"} {
		if bytes.Equal(bytes.TrimSpace(fields[k]), []byte("null")) {
			return ErrInvalid
		}
	}
	return nil
}
func (m *Measurement) validV2(p *Plan) error {
	if m.Version != 2 || !validToken(m.Stream, MaxIDBytes) || m.Start > MaxCounter || m.Loss > MaxCounter || m.LastHash != "" {
		return ErrInvalid
	}
	if m.End != nil && (*m.End < m.Start || *m.End > MaxCounter || !validToken(m.Result, MaxClassBytes)) {
		return ErrInvalid
	}
	if m.Result != "" && !validToken(m.Result, MaxClassBytes) {
		return ErrInvalid
	}
	if m.Storage == "shared" {
		if p.State != "open" || p.ObservedCalls != nil {
			return ErrInvalid
		}
		if m.End == nil {
			if m.Result != "" || p.HostCalibration != "provisional" {
				return ErrInvalid
			}
		} else if p.HostCalibration != "pending" {
			return ErrInvalid
		}
		return nil
	}
	if m.Storage != "inline" {
		return ErrInvalid
	}
	if m.Known {
		if m.Cursor < m.Start || m.Cursor > MaxCounter || m.Count > m.Cursor-m.Start || (m.Count == 0 && m.Cursor != m.Start) || (m.Count > 0 && m.Cursor == m.Start) || (m.End != nil && m.Cursor > *m.End) {
			return ErrInvalid
		}
	} else if m.Count != 0 || m.Cursor != 0 {
		return ErrInvalid
	}
	if p.HostCalibration == "complete" {
		if p.State != "closed" || m.End == nil || !m.Known || p.ObservedCalls == nil || *p.ObservedCalls != m.Count {
			return ErrInvalid
		}
	} else if p.HostCalibration != "unresolved" || p.ObservedCalls != nil {
		return ErrInvalid
	}
	if p.State == "closed" && !p.Abandoned && !validToken(m.Result, MaxClassBytes) {
		return ErrInvalid
	}
	if p.State == "open" && m.End == nil && m.Result != "" {
		return ErrInvalid
	}
	return nil
}
