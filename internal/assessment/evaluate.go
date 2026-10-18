package assessment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

func Assess(reader Reader, files Files, q Request, capture Capture) (Result, error) {
	if err := q.validate(); err != nil {
		return Result{}, err
	}
	if reader == nil || files == nil || capture.CallID == "" || capture.Path == "" || path.Clean(capture.Path) != capture.Path || strings.HasPrefix(capture.Path, "/") || strings.HasPrefix(capture.Path, "../") || capture.Path == ".." || capture.Path == "." || strings.ContainsAny(capture.Path, "\\\x00") || !validHash(capture.SHA256) || !validHash(capture.SnapshotSHA256) || !validHash(capture.SuiteSHA256) {
		return Result{}, ErrInvalid
	}
	c := &collection{
		q: q, r: reader,
		seen: map[string]sdk.InteractionRecord{}, seq: map[uint64]string{},
		out: Result{
			Evaluator: Version, Finding: "insufficient_evidence",
			Basis:          "controlled_harness_capture",
			SnapshotSHA256: q.SnapshotSHA256, SuiteSHA256: SuiteHash(q.Suite),
			Coverage: Coverage{Complete: true, Reasons: []string{}},
			Evidence: []Ref{}, Attempts: []Attempt{}, Reasons: []string{},
		},
	}
	if err := c.collect(); err != nil {
		return Result{}, err
	}
	for _, ref := range []Ref{q.Start, q.Declaration, q.End} {
		if !c.anchor(ref) {
			addReason(&c.out.Reasons, "anchor_unavailable_or_changed")
			return c.finish(capture), nil
		}
	}

	var plan *string
	var planSeq uint64
	for _, r := range c.seen {
		w := r.Details.Work
		if r.Sequence >= q.Start.Sequence && r.Sequence <= q.Declaration.Sequence && r.Kind == sdk.InteractionNotice && w != nil && w.Session == q.WorkSession && w.Plan != nil && r.Sequence >= planSeq {
			plan, planSeq = w.Plan, r.Sequence
		}
	}
	if plan == nil || *plan == "" || c.seen[q.End.ID].Details.Work.Result == nil || *c.seen[q.End.ID].Details.Work.Result == "" {
		addReason(&c.out.Reasons, "declaration_or_outcome_missing")
		return c.finish(capture), nil
	}
	var result *sdk.InteractionRecord
	for _, a := range c.out.Attempts {
		if a.ID == capture.CallID && a.ResultID != "" && (a.Outcome == "succeeded" || a.Outcome == "failed") {
			r := c.seen[a.ResultID]
			result = &r
		}
	}
	if result == nil {
		addReason(&c.out.Reasons, "captured_execution_unresolved")
		return c.finish(capture), nil
	}
	if c.seen[capture.CallID].Sequence <= q.Declaration.Sequence {
		addReason(&c.out.Reasons, "declaration_after_call")
		return c.finish(capture), nil
	}
	if capture.SnapshotSHA256 != q.SnapshotSHA256 || capture.SuiteSHA256 != SuiteHash(q.Suite) {
		addReason(&c.out.Reasons, "capture_target_mismatch")
		return c.finish(capture), nil
	}
	raw, ok, err := c.report(files, capture)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		addReason(&c.out.Reasons, "report_unavailable")
		return c.finish(capture), nil
	}
	if !utf8.Valid(raw) || result.Details.ResultRecord != fmt.Sprintf("[%d chars, sha256=%s]", utf8.RuneCount(raw), Hash(raw)) {
		return Result{}, ErrConflict
	}
	finding, reasons, err := evaluateGo(raw, q.Suite, capture)
	if err != nil {
		return Result{}, err
	}
	c.out.Reasons = reasons
	if finding == "passed" && (!c.out.Coverage.Complete || result.Outcome != sdk.InteractionSucceeded) {
		finding = "insufficient_evidence"
		addReason(&c.out.Reasons, "execution_or_coverage_incomplete")
	}
	c.out.Finding = finding
	return c.finish(capture), nil
}

func (c *collection) report(files Files, capture Capture) ([]byte, bool, error) {
	var raw []byte
	var size int64 = -1
	for {
		if !c.permit() {
			return nil, false, nil
		}
		remaining := c.q.Limits.ReportBytes - len(raw)
		if remaining <= 0 {
			c.missing("report_budget")
			return nil, false, nil
		}
		n := min(16384, remaining)
		b, eof, total, err := files.Read(sdk.SandboxRoot, capture.Path, int64(len(raw)), n)
		if err != nil {
			var op *sdk.OperationError
			if errors.As(err, &op) && op.ReasonCode == "FS_NOT_FOUND" {
				if len(raw) > 0 {
					return nil, false, ErrSourceChanged
				}
				c.missing("report_missing")
				return nil, false, nil
			}
			return nil, false, err
		}
		if total < 0 || len(b) > n || size >= 0 && total != size || int64(len(raw)+len(b)) > total || eof != (int64(len(raw)+len(b)) == total) || !eof && len(b) == 0 {
			return nil, false, protocol("invalid report chunk")
		}
		if total > int64(c.q.Limits.ReportBytes) {
			c.missing("report_budget")
			return nil, false, nil
		}
		size = total
		raw = append(raw, b...)
		c.out.Coverage.ReportBytes += len(b)
		if eof {
			if Hash(raw) != capture.SHA256 {
				return nil, false, ErrSourceChanged
			}
			return raw, true, nil
		}
	}
}

type goEvent struct {
	Time        string
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	OutputType  string
	FailedBuild string
}

func evaluateGo(raw []byte, suite Suite, capture Capture) (string, []string, error) {
	reasons := []string{}
	packages := map[string]string{}
	started := map[string]bool{}
	tests := map[string]map[string]string{}
	failure := false
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var msg json.RawMessage
		err := dec.Decode(&msg)
		if err == io.EOF {
			break
		}
		if err != nil || sdk.ValidateStrict(msg) != nil {
			return "insufficient_evidence", []string{"malformed_report"}, nil
		}
		var ev goEvent
		d := json.NewDecoder(bytes.NewReader(msg))
		d.DisallowUnknownFields()
		if d.Decode(&ev) != nil || ev.Elapsed < 0 {
			return "insufficient_evidence", []string{"malformed_report"}, nil
		}
		if _, ok := suite.Packages[ev.Package]; !ok {
			return "insufficient_evidence", []string{"unexpected_package"}, nil
		}
		if tests[ev.Package] == nil {
			tests[ev.Package] = map[string]string{}
		}
		if packages[ev.Package] != "" {
			return "", nil, ErrConflict
		}
		if ev.Test == "" {
			switch ev.Action {
			case "start":
				if started[ev.Package] {
					return "", nil, ErrConflict
				}
				started[ev.Package] = true
			case "output", "build-output":
			case "pass", "fail", "skip":
				if ev.Action == "pass" {
					for _, state := range tests[ev.Package] {
						if state == "fail" {
							return "", nil, ErrConflict
						}
						if state == "run" {
							addReason(&reasons, "unfinished_test")
						}
					}
				}
				packages[ev.Package] = ev.Action
				if ev.Action == "fail" {
					failure = true
				}
			default:
				return "insufficient_evidence", []string{"unsupported_event"}, nil
			}
		} else {
			old := tests[ev.Package][ev.Test]
			switch ev.Action {
			case "run":
				if old != "" {
					return "", nil, ErrConflict
				}
				tests[ev.Package][ev.Test] = "run"
			case "output", "pause", "cont":
				if old != "run" {
					return "insufficient_evidence", []string{"test_start_missing"}, nil
				}
			case "pass", "fail", "skip":
				if old != "run" {
					return "", nil, ErrConflict
				}
				tests[ev.Package][ev.Test] = ev.Action
				if ev.Action == "fail" {
					failure = true
				}
			default:
				return "insufficient_evidence", []string{"unsupported_event"}, nil
			}
		}
	}
	if failure {
		if capture.ExitCode != nil && *capture.ExitCode == 0 {
			return "", nil, ErrConflict
		}
		if !capture.Complete {
			addReason(&reasons, "validation_interrupted")
		}
		return "failed", reasons, nil
	}
	if !capture.Complete || capture.ExitCode == nil {
		addReason(&reasons, "validation_interrupted")
	} else if *capture.ExitCode != 0 {
		addReason(&reasons, "runner_failed_without_test_verdict")
	}
	for p, required := range suite.Packages {
		if !started[p] {
			addReason(&reasons, "package_start_missing")
		}
		if packages[p] != "pass" {
			addReason(&reasons, "package_not_passed")
		}
		for _, t := range required {
			if tests[p][t] != "pass" {
				addReason(&reasons, "required_test_not_passed")
			}
		}
	}
	if len(reasons) > 0 {
		return "insufficient_evidence", reasons, nil
	}
	return "passed", reasons, nil
}
