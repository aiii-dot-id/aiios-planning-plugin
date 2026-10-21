package assessment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

const Version = "go-suite-v1"

var (
	ErrInvalid       = errors.New("assessment: invalid request")
	ErrSourceChanged = errors.New("assessment: source changed")
	ErrConflict      = errors.New("assessment: conflicting evidence")
	ErrProtocol      = errors.New("assessment: invalid reader response")
)

type Limits struct {
	ReadCalls   int `json:"read_calls"`
	Records     int `json:"records"`
	DetailBytes int `json:"detail_bytes"`
	ReportBytes int `json:"report_bytes"`
}

func DefaultLimits() Limits { return Limits{32, 512, 256 << 10, 1 << 20} }

type Ref struct {
	ID          string `json:"id"`
	Sequence    uint64 `json:"sequence,string"`
	SHA256      string `json:"sha256"`
	Incarnation string `json:"incarnation,omitempty"`
}

type Suite struct {
	Packages          map[string][]string `json:"packages"`
	Runner            string              `json:"runner"`
	ChecksSHA256      string              `json:"checks_sha256"`
	EnvironmentSHA256 string              `json:"environment_sha256"`
}

type Request struct {
	Identity       string `json:"identity"`
	WorkSession    string `json:"work_session_id"`
	Project        string `json:"project_id"`
	Start          Ref    `json:"start"`
	Declaration    Ref    `json:"declaration"`
	End            Ref    `json:"end"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Suite          Suite  `json:"suite"`
	Limits         Limits `json:"limits"`
}

type Capture struct {
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	SuiteSHA256    string `json:"suite_sha256"`
	CallID         string `json:"call_id"`
	ExitCode       *int   `json:"exit_code"`
	Complete       bool   `json:"complete"`
}

type Reader interface {
	Query(sdk.InteractionQuery) (*sdk.InteractionPage, error)
	Read(sdk.InteractionReadRequest) ([]byte, bool, int64, error)
}

type Files interface {
	Read(root, path string, offset int64, length int) ([]byte, bool, int64, error)
}

type Coverage struct {
	Complete    bool     `json:"complete"`
	Cutoff      uint64   `json:"cutoff,string"`
	ReadCalls   int      `json:"read_calls"`
	Records     int      `json:"records"`
	DetailBytes int      `json:"detail_bytes"`
	ReportBytes int      `json:"report_bytes"`
	Reasons     []string `json:"reasons"`
}

type Attempt struct {
	WorkSession            string           `json:"work_session_id,omitempty"`
	RequestRef             *Ref             `json:"request_ref,omitempty"`
	ResultRef              *Ref             `json:"result_ref,omitempty"`
	ResultExcerpt          string           `json:"result_excerpt,omitempty"`
	ResultRecord           string           `json:"result_record,omitempty"`
	ResultExcerptTruncated bool             `json:"result_excerpt_truncated,omitempty"`
	ResultDetailRequest    *ResultSelection `json:"result_detail_request,omitempty"`
	ResultTruncated        bool             `json:"result_truncated,omitempty"`
	ID                     string           `json:"id"`
	Tool                   string           `json:"tool"`
	ProjectID              string           `json:"project_id"`
	ResultID               string           `json:"result_id,omitempty"`
	Outcome                string           `json:"outcome"`
	ReasonCode             string           `json:"reason_code,omitempty"`
	DurationMS             *int64           `json:"duration_ms,omitempty"`
}

type Result struct {
	Selection         Request   `json:"selection"`
	Capture           Capture   `json:"capture"`
	Evaluator         string    `json:"evaluator"`
	Finding           string    `json:"finding"`
	Basis             string    `json:"basis"`
	SnapshotSHA256    string    `json:"snapshot_sha256"`
	SuiteSHA256       string    `json:"suite_sha256"`
	EvidenceSHA256    string    `json:"evidence_sha256"`
	SourceIncarnation string    `json:"source_incarnation"`
	Coverage          Coverage  `json:"coverage"`
	Evidence          []Ref     `json:"evidence"`
	Attempts          []Attempt `json:"attempts"`
	Reasons           []string  `json:"reasons"`
}

func Hash(b []byte) string     { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func digest(v any) string      { b, _ := json.Marshal(v); return Hash(b) }
func SuiteHash(s Suite) string { return digest(s) }

func RecordRef(r sdk.InteractionRecord) Ref {
	r.Annotations = nil
	r.Deliveries = nil
	r.ContentRef = nil
	return Ref{ID: r.ID, Sequence: r.Sequence, SHA256: digest(r)}
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func (q Request) validate() error {
	if q.Identity == "" || q.WorkSession == "" || !validHash(q.SnapshotSHA256) || q.Suite.Runner == "" || !validHash(q.Suite.ChecksSHA256) || !validHash(q.Suite.EnvironmentSHA256) {
		return ErrInvalid
	}
	for _, r := range []Ref{q.Start, q.Declaration, q.End} {
		if r.ID == "" || r.Sequence == 0 || !validHash(r.SHA256) {
			return ErrInvalid
		}
	}
	if q.Start.Sequence > q.Declaration.Sequence || q.Declaration.Sequence >= q.End.Sequence {
		return ErrInvalid
	}
	l := q.Limits
	if l.ReadCalls < 1 || l.ReadCalls > 256 || l.Records < 1 || l.Records > 4096 || l.DetailBytes < 1 || l.DetailBytes > 4<<20 || l.ReportBytes < 1 || l.ReportBytes > 4<<20 {
		return ErrInvalid
	}
	if len(q.Suite.Packages) == 0 || len(q.Suite.Packages) > 64 {
		return ErrInvalid
	}
	for p, ts := range q.Suite.Packages {
		if p == "" || len(ts) == 0 || len(ts) > 256 {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, t := range ts {
			if t == "" || seen[t] {
				return ErrInvalid
			}
			seen[t] = true
		}
	}
	return nil
}

func addReason(dst *[]string, s string) {
	for _, old := range *dst {
		if old == s {
			return
		}
	}
	*dst = append(*dst, s)
	sort.Strings(*dst)
}

func protocol(s string) error { return fmt.Errorf("%w: %s", ErrProtocol, s) }
