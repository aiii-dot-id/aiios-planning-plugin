package assessment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

type readerOverride struct {
	query func(sdk.InteractionQuery) (*sdk.InteractionPage, error)
	read  func(sdk.InteractionReadRequest) ([]byte, bool, int64, error)
}

type errorFiles struct{ err error }

func (f errorFiles) Read(string, string, int64, int) ([]byte, bool, int64, error) {
	return nil, false, 0, f.err
}

func TestMissingReportIsNotPermissionDenial(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	got, err := Assess(f, errorFiles{sdk.Fail("FS_NOT_FOUND", "missing")}, f.q, f.capture)
	if err != nil || got.Finding != "insufficient_evidence" || got.Coverage.Complete {
		t.Fatalf("%+v %v", got, err)
	}
	denied := &sdk.Denied{Code: -32000, ReasonCode: "POLICY_DENY"}
	f = newFixture(sample("passed"), 0)
	if _, err := Assess(f, errorFiles{denied}, f.q, f.capture); err != denied {
		t.Fatal(err)
	}
}

func (r readerOverride) Query(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
	return r.query(q)
}
func (r readerOverride) Read(q sdk.InteractionReadRequest) ([]byte, bool, int64, error) {
	return r.read(q)
}

func TestExpandedDetailsAndBounds(t *testing.T) {
	for _, mode := range []string{"complete", "budget", "hash changed", "zero progress"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(sample("passed"), 0)
			f.rows[1].Content = strings.Repeat("context ", 3000)
			f.q.Declaration = RecordRef(f.rows[1])
			raw, _ := json.Marshal(f.rows[1])
			r := readerOverride{query: func(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
				p, err := f.Query(q)
				if err != nil {
					return p, err
				}
				for i, v := range p.Rows {
					if v.ID == "declaration" {
						p.Rows[i] = sdk.InteractionRecord{ID: v.ID, Sequence: v.Sequence, ContentRef: &sdk.InteractionContentRef{ID: v.ID, Source: "recorded", Incarnation: "incarnation", SHA256: Hash(raw), Bytes: len(raw)}}
					}
				}
				return p, nil
			}, read: func(q sdk.InteractionReadRequest) ([]byte, bool, int64, error) {
				if mode == "zero progress" {
					return nil, false, int64(len(raw)), nil
				}
				end := min(int(q.Offset)+q.Length, len(raw))
				b := append([]byte(nil), raw[q.Offset:end]...)
				if mode == "hash changed" {
					b[0] ^= 1
				}
				return b, end == len(raw), int64(len(raw)), nil
			}}
			if mode == "budget" {
				f.q.Limits.DetailBytes = 1
			}
			got, err := Assess(r, fixtureFiles{f}, f.q, f.capture)
			switch mode {
			case "complete":
				if err != nil || got.Finding != "passed" || got.Coverage.DetailBytes != len(raw) {
					t.Fatalf("%+v %v", got, err)
				}
			case "budget":
				if err != nil || got.Finding != "insufficient_evidence" || got.Coverage.Complete {
					t.Fatalf("%+v %v", got, err)
				}
			case "hash changed":
				if !errors.Is(err, ErrSourceChanged) {
					t.Fatal(err)
				}
			case "zero progress":
				if !errors.Is(err, ErrProtocol) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCutoffAndPagination(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	late := f.rows[5]
	f.rows = f.rows[:5]
	r := readerOverride{query: func(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
		if f.queries == 1 {
			f.rows = append(f.rows, late)
		}
		return f.Query(q)
	}, read: f.Read}
	got, err := Assess(r, fixtureFiles{f}, f.q, f.capture)
	if err != nil || got.Finding != "insufficient_evidence" || got.Coverage.Cutoff != 5 || len(got.Attempts) != 1 || got.Attempts[0].ResultID != "" {
		t.Fatalf("future result leaked: %+v %v", got, err)
	}

	f = newFixture(sample("passed"), 0)
	f.pageSize = 2
	r.query = func(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
		actual := q
		actual.Cursor = ""
		p, err := f.Query(actual)
		p.Cursor = q.Cursor
		return p, err
	}
	if _, err := Assess(r, fixtureFiles{f}, f.q, f.capture); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

func TestBadGoReportsCannotPass(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"Action":"start","Action":"pass","Package":"example"}`),
		[]byte(`{"Action":"start","Package":"example"}`),
		[]byte("{\"Action\":\"start\",\"Package\":\"example\"}\n{\"Action\":\"run\",\"Package\":\"example\",\"Test\":\"TestTruth\"}\n{\"Action\":\"pass\",\"Package\":\"example\"}\n"),
	} {
		f := newFixture(raw, 0)
		got, err := f.assess()
		if err == nil && got.Finding == "passed" {
			t.Fatalf("bad report passed: %s", raw)
		}
	}
	f := newFixture(sample("failed"), 0)
	if _, err := f.assess(); !errors.Is(err, ErrConflict) {
		t.Fatal("zero exit contradicted failing test", err)
	}
}
