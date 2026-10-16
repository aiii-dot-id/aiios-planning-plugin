package assessment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

type fixture struct {
	descendants map[string]bool
	rows        []sdk.InteractionRecord
	raw         []byte
	q           Request
	capture     Capture
	queries     int
	changeAt    int
	denial      error
	pageSize    int
	fileReads   int
}

func ptr[T any](v T) *T { return &v }

func sample(finding string) []byte {
	testResult, pkgResult := "pass", "pass"
	if finding == "failed" {
		testResult, pkgResult = "fail", "fail"
	}
	if finding == "skipped" {
		testResult = "skip"
	}
	lines := []map[string]any{
		{"Action": "start", "Package": "example"},
		{"Action": "run", "Package": "example", "Test": "TestTruth"},
		{"Action": testResult, "Package": "example", "Test": "TestTruth"},
		{"Action": pkgResult, "Package": "example"},
	}
	var raw []byte
	for _, line := range lines {
		b, _ := json.Marshal(line)
		raw = append(raw, b...)
		raw = append(raw, '\n')
	}
	return raw
}

func newFixture(raw []byte, exit int) *fixture {
	f := &fixture{raw: raw}
	f.q = Request{Identity: "identity", WorkSession: "ws", Project: "project", SnapshotSHA256: Hash([]byte("snapshot")), Suite: Suite{Packages: map[string][]string{"example": {"TestTruth"}}, Runner: "go-test-json-count1", ChecksSHA256: Hash([]byte("checks")), EnvironmentSHA256: Hash([]byte("environment"))}, Limits: DefaultLimits()}
	f.capture = Capture{Path: "reports/captured.jsonl", SHA256: Hash(raw), SnapshotSHA256: f.q.SnapshotSHA256, SuiteSHA256: SuiteHash(f.q.Suite), CallID: "call", Complete: true, ExitCode: ptr(exit)}
	notice := func(id string, n uint64, w sdk.InteractionWorkChange) sdk.InteractionRecord {
		return sdk.InteractionRecord{ID: id, Sequence: n, Kind: sdk.InteractionNotice, ProjectID: "project", TurnID: "turn", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &w}}
	}
	f.rows = []sdk.InteractionRecord{
		notice("start", 1, sdk.InteractionWorkChange{Session: "ws"}),
		notice("declaration", 2, sdk.InteractionWorkChange{Session: "ws", Plan: ptr("Validate the captured source with the fixed suite")}),
		{ID: "call", Sequence: 3, Kind: sdk.InteractionToolCall, ProjectID: "project", TurnID: "turn", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell"}},
		notice("end", 4, sdk.InteractionWorkChange{Session: "ws", Result: ptr("served: validation attempted")}),
		notice("later-amendment", 5, sdk.InteractionWorkChange{Session: "ws", Plan: ptr("An unrelated later plan")}),
		{ID: "result", Sequence: 6, Kind: sdk.InteractionToolResult, ProjectID: "project", TurnID: "turn", RelatedID: "call", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Outcome: sdk.InteractionSucceeded, Details: sdk.InteractionDetails{WorkSession: "ws", Tool: "shell", DurationMS: ptr(int64(7)), ResultRecord: fmt.Sprintf("[%d chars, sha256=%s]", utf8.RuneCount(raw), Hash(raw))}},
	}
	if exit != 0 {
		f.rows[5].Outcome = sdk.InteractionFailed
	}
	f.q.Start = RecordRef(f.rows[0])
	f.q.Declaration = RecordRef(f.rows[1])
	f.q.End = RecordRef(f.rows[3])
	return f
}

func (f *fixture) Query(q sdk.InteractionQuery) (*sdk.InteractionPage, error) {
	f.queries++
	if f.denial != nil {
		return nil, f.denial
	}
	inc := "incarnation"
	if f.changeAt > 0 && f.queries >= f.changeAt {
		inc = "replacement"
	}
	before := ^uint64(0)
	if q.Cursor != "" {
		var err error
		before, err = strconv.ParseUint(q.Cursor, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	rows := []sdk.InteractionRecord{}
	for _, r := range f.rows {
		if r.Sequence >= before || q.Filter.WorkSession != "" && r.Details.WorkSession != q.Filter.WorkSession && !(q.Filter.IncludeDescendants && f.descendants[r.Details.WorkSession]) || q.Filter.RelatedID != "" && r.RelatedID != q.Filter.RelatedID {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Sequence > rows[j].Sequence })
	n := q.Limit
	if f.pageSize > 0 {
		n = min(n, f.pageSize)
	}
	next := ""
	if len(rows) > n {
		rows = rows[:n]
		next = strconv.FormatUint(rows[len(rows)-1].Sequence, 10)
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return &sdk.InteractionPage{Version: 1, Identity: "identity", Source: "recorded", Incarnation: inc, Filter: q.Filter, Cursor: q.Cursor, Rows: rows, NextCursor: next, Fingerprint: "fixture", Availability: "complete"}, nil
}

func (*fixture) Read(sdk.InteractionReadRequest) ([]byte, bool, int64, error) {
	return nil, false, 0, errors.New("unexpected detail read")
}

type fixtureFiles struct{ f *fixture }

func (fs fixtureFiles) Read(root, path string, offset int64, length int) ([]byte, bool, int64, error) {
	f := fs.f
	f.fileReads++
	if root != sdk.SandboxRoot || path != f.capture.Path || offset < 0 || offset > int64(len(f.raw)) {
		return nil, false, 0, errors.New("denied file")
	}
	end := min(int(offset)+length, len(f.raw))
	return f.raw[offset:end], end == len(f.raw), int64(len(f.raw)), nil
}
func (f *fixture) assess() (Result, error) { return Assess(f, fixtureFiles{f}, f.q, f.capture) }

func TestFindingsAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name            string
		modify          func(*fixture)
		finding, reason string
	}{
		{"pass", func(*fixture) {}, "passed", ""},
		{"wrong revision", func(f *fixture) { f.q.SnapshotSHA256 = Hash([]byte("B")) }, "insufficient_evidence", "capture_target_mismatch"},
		{"interrupted", func(f *fixture) { f.capture.Complete = false; f.capture.ExitCode = nil }, "insufficient_evidence", "validation_interrupted"},
		{"read budget", func(f *fixture) { f.q.Limits.ReadCalls = 1 }, "insufficient_evidence", "anchor_unavailable_or_changed"},
		{"report budget", func(f *fixture) { f.q.Limits.ReportBytes = 1 }, "insufficient_evidence", "report_unavailable"},
		{"missing result", func(f *fixture) { f.rows = f.rows[:5] }, "insufficient_evidence", "captured_execution_unresolved"},
		{"mismatched attribution", func(f *fixture) { f.rows[5].Details.WorkSession = "other" }, "insufficient_evidence", "captured_execution_unresolved"},
		{"changed declaration", func(f *fixture) { f.rows[1].Details.Work.Plan = ptr("changed") }, "insufficient_evidence", "anchor_unavailable_or_changed"},
		{"late declaration", func(f *fixture) {
			f.rows[1].Sequence = 4
			f.rows[3].Sequence = 5
			f.rows[4].Sequence = 7
			f.q.Declaration = RecordRef(f.rows[1])
			f.q.End = RecordRef(f.rows[3])
		}, "insufficient_evidence", "declaration_after_call"},
		{"explicit clear", func(f *fixture) { f.rows[1].Details.Work.Plan = ptr(""); f.q.Declaration = RecordRef(f.rows[1]) }, "insufficient_evidence", "declaration_or_outcome_missing"},
		{"mutable annotation", func(f *fixture) {
			f.rows[1].Annotations = map[string]json.RawMessage{"grade": json.RawMessage(`"later success"`)}
		}, "passed", ""},
		{"pagination", func(f *fixture) { f.pageSize = 2 }, "passed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(sample("passed"), 0)
			tc.modify(f)
			got, err := f.assess()
			if err != nil {
				t.Fatal(err)
			}
			if got.Finding != tc.finding || tc.reason != "" && !strings.Contains(strings.Join(got.Reasons, ","), tc.reason) {
				t.Fatalf("%+v", got)
			}
			if got.Finding == "passed" && (len(got.Attempts) != 1 || got.Attempts[0].ResultID != "result" || got.Coverage.Cutoff != 6) {
				t.Fatalf("late result or count lost: %+v", got)
			}
		})
	}
}

func TestFailuresAreNotUnknownOrZero(t *testing.T) {
	for _, complete := range []bool{true, false} {
		f := newFixture(sample("failed"), 1)
		f.capture.Complete = complete
		if !complete {
			f.capture.ExitCode = nil
		}
		got, err := f.assess()
		if err != nil || got.Finding != "failed" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	f := newFixture(sample("skipped"), 0)
	got, err := f.assess()
	if err != nil || got.Finding != "insufficient_evidence" {
		t.Fatalf("%+v %v", got, err)
	}
	f = newFixture(nil, 0)
	got, err = f.assess()
	if err != nil || got.Finding != "insufficient_evidence" {
		t.Fatalf("%+v %v", got, err)
	}
	f = newFixture([]byte(`{"Action":`), 1)
	got, err = f.assess()
	if err != nil || got.Finding != "insufficient_evidence" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTypedErrorsAndNoFileReachOnDenial(t *testing.T) {
	denial := &sdk.Denied{Code: -32000, ReasonCode: "POLICY_DENY"}
	f := newFixture(sample("passed"), 0)
	f.denial = denial
	if _, err := f.assess(); err != denial || f.fileReads != 0 {
		t.Fatalf("%v reads=%d", err, f.fileReads)
	}
	f = newFixture(sample("passed"), 0)
	f.changeAt = 2
	if _, err := f.assess(); !errors.Is(err, ErrSourceChanged) {
		t.Fatal(err)
	}
	f = newFixture(sample("passed"), 0)
	f.raw = append(f.raw, ' ')
	if _, err := f.assess(); !errors.Is(err, ErrSourceChanged) {
		t.Fatal(err)
	}
	f = newFixture(sample("passed"), 0)
	f.rows[5].Details.ResultRecord = "different output"
	if _, err := f.assess(); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	f = newFixture(sample("passed"), 0)
	duplicate := f.rows[5]
	duplicate.ID = "result2"
	duplicate.Sequence = 7
	f.rows = append(f.rows, duplicate)
	if _, err := f.assess(); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	f = newFixture(sample("passed"), 0)
	f.capture.Path = "../escape"
	if _, err := f.assess(); !errors.Is(err, ErrInvalid) || f.queries != 0 {
		t.Fatal(err)
	}
}

func TestStableFindingAndNoFutureDeclarationLeak(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	a, err := f.assess()
	if err != nil {
		t.Fatal(err)
	}
	f.queries = 0
	f.rows[1].Annotations = map[string]json.RawMessage{"grade": json.RawMessage(`"updated"`)}
	b, err := f.assess()
	if err != nil {
		t.Fatal(err)
	}
	if a.EvidenceSHA256 != b.EvidenceSHA256 || a.Finding != b.Finding {
		t.Fatal("mutable metadata changed grading")
	}
}

func TestRetryIsAnAttemptNotDuplicateEvidence(t *testing.T) {
	f := newFixture(sample("passed"), 0)
	f.rows[3].Sequence = 5
	f.q.End = RecordRef(f.rows[3])
	call := f.rows[2]
	call.ID = "retry"
	call.Sequence = 4
	call.Source = &sdk.InteractionSource{Kind: "tool", ID: "execution2"}
	result := f.rows[5]
	result.ID = "retry-result"
	result.Sequence = 7
	result.RelatedID = call.ID
	result.Source = call.Source
	f.rows = append(append(f.rows[:4], f.rows[5]), call, result)
	got, err := f.assess()
	if err != nil || got.Finding != "passed" || len(got.Attempts) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRealGoCapture(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			dir := t.TempDir()
			mod := []byte("module example\n\ngo 1.25\n")
			// The acceptance test never changes between broken and repaired
			// candidates. Only the implementation changes.
			checks := []byte("package example\nimport \"testing\"\nfunc TestTruth(t *testing.T) { if Add(2,3)!=5 { t.Fatal(\"wrong sum\") } }\n")
			body := "package example\nfunc Add(a,b int) int { return a+b }\n"
			if broken {
				body = "package example\nfunc Add(a,b int) int { return a-b }\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "truth_test.go"), checks, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "add.go"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(goTool, "test", "-json", "-count=1", ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
			raw, runErr := cmd.Output()
			exit := 0
			if runErr != nil {
				var e *exec.ExitError
				if !errors.As(runErr, &e) {
					t.Fatal(runErr)
				}
				exit = e.ExitCode()
			}
			f := newFixture(raw, exit)
			f.q.SnapshotSHA256 = Hash(append(append(mod, []byte(body)...), checks...))
			f.capture.SnapshotSHA256 = f.q.SnapshotSHA256
			f.q.Suite.ChecksSHA256 = Hash(checks)
			f.capture.SuiteSHA256 = SuiteHash(f.q.Suite)
			start := time.Now()
			got, err := f.assess()
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%v\n%s", err, raw)
			}
			want := "passed"
			if broken {
				want = "failed"
			}
			if got.Finding != want {
				t.Fatalf("%+v\n%s", got, raw)
			}
			t.Logf("real capture %s: %d bytes; assessment %s; %d reader operations", want, len(raw), elapsed, got.Coverage.ReadCalls)
		})
	}
}
