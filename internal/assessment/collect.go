package assessment

import (
	"encoding/json"
	"reflect"
	"sort"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

type collection struct {
	sessionOnly        bool
	includeDescendants bool
	excludeCallID      string
	selected           map[string]bool
	q                  Request
	r                  Reader
	out                Result
	seen               map[string]sdk.InteractionRecord
	seq                map[uint64]string
}

func (c *collection) missing(reason string) {
	c.out.Coverage.Complete = false
	addReason(&c.out.Coverage.Reasons, reason)
}

func (c *collection) permit() bool {
	if c.out.Coverage.ReadCalls >= c.q.Limits.ReadCalls {
		c.missing("read_budget")
		return false
	}
	c.out.Coverage.ReadCalls++
	return true
}

func sameFilter(a, b sdk.InteractionFilter) bool {
	if len(a.Kinds) == 0 {
		a.Kinds = nil
	}
	if len(b.Kinds) == 0 {
		b.Kinds = nil
	}
	return reflect.DeepEqual(a, b)
}

func (c *collection) query(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
	if !c.permit() {
		return nil, nil
	}
	p, err := c.r.Query(q)
	if err != nil {
		return nil, err
	}
	if p == nil || p.Version != 1 || p.Source != "recorded" || p.Cursor != q.Cursor || p.Fingerprint == "" || p.Rows == nil || len(p.Rows) > q.Limit || !sameFilter(p.Filter, q.Filter) {
		return nil, protocol("page does not match request")
	}
	if c.sessionOnly && c.q.Identity == "" {
		c.q.Identity = p.Identity
	}
	if p.Identity == "" || p.Identity != c.q.Identity || p.Incarnation == "" {
		return nil, ErrSourceChanged
	}
	if c.out.SourceIncarnation == "" {
		c.out.SourceIncarnation = p.Incarnation
	}
	if c.out.SourceIncarnation != p.Incarnation {
		return nil, ErrSourceChanged
	}
	if p.Availability != "complete" || p.Lost != 0 {
		c.missing("source_incomplete")
		if p.Availability == "ancestry_incomplete" {
			c.missing("ancestry_incomplete")
		}
	}
	for i, r := range p.Rows {
		if r.ID == "" || r.Sequence == 0 || i > 0 && r.Sequence <= p.Rows[i-1].Sequence {
			return nil, protocol("unordered page")
		}
	}
	return p, nil
}

func (c *collection) expand(r sdk.InteractionRecord) (sdk.InteractionRecord, bool, error) {
	ref := r.ContentRef
	if ref == nil {
		return r, true, nil
	}
	if ref.ID != r.ID || ref.Source != "recorded" || ref.Incarnation != c.out.SourceIncarnation || !validHash(ref.SHA256) || ref.Bytes < 1 {
		return r, false, protocol("invalid detail reference")
	}
	if ref.Bytes > c.q.Limits.DetailBytes-c.out.Coverage.DetailBytes {
		c.missing("detail_budget")
		return r, false, nil
	}
	var raw []byte
	for len(raw) < ref.Bytes {
		if !c.permit() {
			return r, false, nil
		}
		n := min(sdk.InteractionReadLimit, ref.Bytes-len(raw))
		b, eof, size, err := c.r.Read(sdk.InteractionReadRequest{Version: 1, Source: "recorded", ID: ref.ID, Incarnation: ref.Incarnation, SHA256: ref.SHA256, Offset: int64(len(raw)), Length: n})
		if err != nil {
			return r, false, err
		}
		if len(b) == 0 || len(b) > n || size != int64(ref.Bytes) || eof != (len(raw)+len(b) == ref.Bytes) {
			return r, false, protocol("invalid detail chunk")
		}
		raw = append(raw, b...)
		c.out.Coverage.DetailBytes += len(b)
	}
	if Hash(raw) != ref.SHA256 {
		return r, false, ErrSourceChanged
	}
	var full sdk.InteractionRecord
	if sdk.ValidateStrict(raw) != nil || json.Unmarshal(raw, &full) != nil || full.ContentRef != nil || full.ID != r.ID || full.Sequence != r.Sequence {
		return r, false, protocol("invalid expanded record")
	}
	return full, true, nil
}

func (c *collection) scan(f sdk.InteractionFilter) error {
	q := sdk.InteractionQuery{Version: 1, Source: "recorded", Filter: f, Limit: min(50, c.q.Limits.Records)}
	cursors := map[string]bool{}
	var previousMin uint64
	for {
		if c.out.Coverage.Records >= c.q.Limits.Records {
			c.missing("record_budget")
			return nil
		}
		if cursors[q.Cursor] {
			return protocol("cursor cycle")
		}
		cursors[q.Cursor] = true
		p, err := c.query(q)
		if err != nil || p == nil {
			return err
		}
		if len(p.Rows) > 0 {
			if previousMin != 0 && p.Rows[len(p.Rows)-1].Sequence >= previousMin {
				return protocol("pagination did not move backwards")
			}
			previousMin = p.Rows[0].Sequence
		} else if p.NextCursor != "" {
			return protocol("empty page has a continuation")
		}

		for i := len(p.Rows) - 1; i >= 0; i-- {
			row := p.Rows[i]
			if row.Sequence > c.out.Coverage.Cutoff || c.excludeCallID != "" && (row.ID == c.excludeCallID || row.RelatedID == c.excludeCallID) {
				continue
			}
			if c.out.Coverage.Records >= c.q.Limits.Records {
				c.missing("record_budget")
				return nil
			}
			c.out.Coverage.Records++
			r, ok, err := c.expand(row)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if f.WorkSession != "" && (r.Details.WorkSession == "" || !f.IncludeDescendants && r.Details.WorkSession != f.WorkSession) || f.RelatedID != "" && r.RelatedID != f.RelatedID {
				return protocol("record outside filter")
			}
			if f.WorkSession != "" && c.selected != nil {
				c.selected[r.ID] = true
			}
			if old, ok := c.seen[r.ID]; ok {
				if RecordRef(old) != RecordRef(r) {
					return ErrConflict
				}
				continue
			}
			if id := c.seq[r.Sequence]; id != "" && id != r.ID {
				return ErrConflict
			}
			c.seq[r.Sequence] = r.ID
			c.seen[r.ID] = r
		}
		if p.NextCursor == "" {
			return nil
		}
		q.Cursor = p.NextCursor
	}
}

func (c *collection) anchor(ref Ref) bool {
	r, ok := c.seen[ref.ID]
	return ok && RecordRef(r) == ref && r.Kind == sdk.InteractionNotice && r.ProjectID == c.q.Project && r.Details.WorkSession == c.q.WorkSession && r.Details.Work != nil && r.Details.Work.Session == c.q.WorkSession
}

func (c *collection) collect() error {

	p, err := c.query(sdk.InteractionQuery{Version: 1, Source: "recorded", Limit: 1})
	if err != nil || p == nil {
		return err
	}
	if len(p.Rows) == 0 {
		c.missing("empty_source")
		return nil
	}
	c.out.Coverage.Cutoff = p.Rows[0].Sequence
	if err := c.scan(sdk.InteractionFilter{WorkSession: c.q.WorkSession, IncludeDescendants: c.includeDescendants}); err != nil {
		return err
	}
	var calls []sdk.InteractionRecord
	for _, r := range c.seen {
		if r.Kind == sdk.InteractionToolCall && r.Sequence > c.q.Start.Sequence && r.Sequence <= c.q.End.Sequence {
			calls = append(calls, r)
		}
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].Sequence < calls[j].Sequence })
	for _, call := range calls {
		if call.Source == nil || call.Source.Kind != "tool" || call.Source.ID == "" || call.TurnID == "" {
			return protocol("call has no execution identity")
		}

		hasResult := false
		for _, r := range c.seen {
			if r.Kind == sdk.InteractionToolResult && r.RelatedID == call.ID {
				hasResult = true
				break
			}
		}
		if !hasResult {
			if err := c.scan(sdk.InteractionFilter{RelatedID: call.ID}); err != nil {
				return err
			}
		}
		a := Attempt{ID: call.ID, Tool: call.Details.Tool, ProjectID: call.ProjectID, Outcome: "unknown"}
		if c.sessionOnly {
			ref := RecordRef(call)
			a.RequestRef = &ref
			a.WorkSession = call.Details.WorkSession
		}
		var result *sdk.InteractionRecord
		for _, r := range c.seen {
			if r.Kind != sdk.InteractionToolResult || r.RelatedID != call.ID {
				continue
			}
			if result != nil {
				return ErrConflict
			}
			v := r
			result = &v
		}
		if result == nil {
			c.missing("unmatched_call")
		} else {
			r := *result
			switch r.Outcome {
			case sdk.InteractionSucceeded, sdk.InteractionFailed, sdk.InteractionUnknown, sdk.InteractionCancelled, sdk.InteractionRefused:
			default:
				return protocol("invalid tool outcome")
			}
			if r.Sequence <= call.Sequence || r.TurnID != call.TurnID || r.Source == nil || call.Source == nil || *r.Source != *call.Source {
				return ErrConflict
			}
			a.ResultID = r.ID

			if r.Details.WorkSession != call.Details.WorkSession || r.ProjectID != call.ProjectID || !c.sessionOnly && call.ProjectID != c.q.Project {
				c.missing("attribution_conflict")
			} else {
				if c.sessionOnly {
					ref := RecordRef(r)
					a.ResultRef = &ref
					a.ResultExcerpt = r.Content
					if runes := []rune(r.Content); len(runes) > 256 {
						a.ResultExcerpt = string(runes[:256])
						a.ResultExcerptTruncated = true
						a.ResultDetailRequest = &ResultSelection{WorkSession: a.WorkSession, SourceIncarnation: c.out.SourceIncarnation, ResultRef: ref}
					}
					a.ResultRecord = r.Details.ResultRecord
					a.ResultTruncated = r.Details.Truncated
				}
				a.Outcome = string(r.Outcome)
				a.DurationMS = r.Details.DurationMS
				if a.DurationMS != nil && *a.DurationMS < 0 {
					return protocol("negative duration")
				}
			}
		}
		c.out.Attempts = append(c.out.Attempts, a)
	}
	return nil
}

func (c *collection) finish(receipt Capture) Result {
	c.out.Selection = c.q
	c.out.Capture = receipt
	for _, r := range c.seen {
		c.out.Evidence = append(c.out.Evidence, RecordRef(r))
	}
	sort.Slice(c.out.Evidence, func(i, j int) bool { return c.out.Evidence[i].Sequence < c.out.Evidence[j].Sequence })
	c.out.EvidenceSHA256 = digest(struct {
		Request Request
		Capture Capture
		Refs    []Ref
		Cutoff  uint64
	}{c.q, receipt, c.out.Evidence, c.out.Coverage.Cutoff})
	return c.out
}
