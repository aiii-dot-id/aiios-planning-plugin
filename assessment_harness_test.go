//go:build !wasm_unknown

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/assessment"
)

type assessmentFixtureConfig struct {
	Request assessment.Request
	Capture assessment.Capture
}

func assessmentFixturePlugin(t *testing.T, raw []byte) *sdk.Plugin {
	var cfg assessmentFixtureConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	p := sdk.New("planning-assessment-fixture")
	p.Describe("assess", sdk.Descriptor{Summary: "Isolated captured-evidence fixture", Effects: sdk.EffectsReadInternal, Capabilities: []string{"interaction.read", "fs.sandbox"}})
	p.Handle("assess", func(c sdk.Call) (any, error) {
		result, err := assessment.Assess(sdk.Interactions, sdk.Files, cfg.Request, cfg.Capture)
		if err != nil {
			fmt.Fprintln(os.Stderr, "assessment fixture:", err)
			return nil, err
		}
		encoded, err := json.Marshal(result)
		return json.RawMessage(encoded), err
	})
	return p
}

func TestAssessmentSDKReadOnlyJourney(t *testing.T) {
	zero := 0

	dir := t.TempDir()
	mod := []byte("module example\n\ngo 1.25\n")
	code := []byte("package example\nimport \"testing\"\nfunc TestTruth(t *testing.T) {}\n")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth_test.go"), code, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-json", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	plan, result := "run the pinned suite", "served: runner finished"
	records := []sdk.InteractionRecord{
		{ID: "start", Sequence: 1, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws"}}},
		{ID: "declaration", Sequence: 2, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Plan: &plan}}},
		{ID: "call", Sequence: 3, Kind: sdk.InteractionToolCall, ProjectID: "project", TurnID: "turn", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Details: sdk.InteractionDetails{WorkSession: "ws"}},
		{ID: "end", Sequence: 4, Kind: sdk.InteractionNotice, ProjectID: "project", Details: sdk.InteractionDetails{WorkSession: "ws", Work: &sdk.InteractionWorkChange{Session: "ws", Result: &result}}},
		{ID: "result", Sequence: 5, Kind: sdk.InteractionToolResult, ProjectID: "project", TurnID: "turn", RelatedID: "call", Source: &sdk.InteractionSource{Kind: "tool", ID: "execution"}, Outcome: sdk.InteractionSucceeded, Details: sdk.InteractionDetails{WorkSession: "ws", ResultRecord: fmt.Sprintf("[%d chars, sha256=%s]", utf8.RuneCount(raw), assessment.Hash(raw))}},
	}
	q := assessment.Request{Identity: "fixture", WorkSession: "ws", Project: "project", Start: assessment.RecordRef(records[0]), Declaration: assessment.RecordRef(records[1]), End: assessment.RecordRef(records[3]), SnapshotSHA256: assessment.Hash(append(mod, code...)), Suite: assessment.Suite{Packages: map[string][]string{"example": {"TestTruth"}}, Runner: "go-test-json-count1", ChecksSHA256: assessment.Hash(code), EnvironmentSHA256: assessment.Hash([]byte("environment"))}, Limits: assessment.DefaultLimits()}
	cap := assessment.Capture{Path: "reports/capture.jsonl", SHA256: assessment.Hash(raw), SnapshotSHA256: q.SnapshotSHA256, SuiteSHA256: assessment.SuiteHash(q.Suite), CallID: "call", Complete: true, ExitCode: &zero}
	cfg, _ := json.Marshal(assessmentFixtureConfig{q, cap})
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			k := newFakeBroker()
			fileReads := 0
			k.readOnlyCall = func(op string, target, args json.RawMessage) ([]byte, bool) {
				success := func(v any) ([]byte, bool) {
					b, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": v})
					return b, false
				}
				if denied {
					return denyBody("interaction grant absent"), true
				}
				switch op {
				case "interaction.query":
					var in sdk.InteractionQuery
					if err := json.Unmarshal(args, &in); err != nil {
						t.Fatal(err)
					}
					rows := []sdk.InteractionRecord{}
					before := ^uint64(0)
					if in.Cursor != "" {
						before, _ = strconv.ParseUint(in.Cursor, 10, 64)
					}
					for _, r := range records {
						if r.Sequence < before && (in.Filter.WorkSession == "" || r.Details.WorkSession == in.Filter.WorkSession) && (in.Filter.RelatedID == "" || r.RelatedID == in.Filter.RelatedID) {
							rows = append(rows, r)
						}
					}
					sort.Slice(rows, func(i, j int) bool { return rows[i].Sequence > rows[j].Sequence })
					next := ""
					if len(rows) > in.Limit {
						rows = rows[:in.Limit]
						next = strconv.FormatUint(rows[len(rows)-1].Sequence, 10)
					}
					for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
						rows[i], rows[j] = rows[j], rows[i]
					}
					return success(sdk.InteractionPage{Version: 1, Identity: "fixture", Source: "recorded", Incarnation: "fixture-1", Filter: in.Filter, Cursor: in.Cursor, Rows: rows, NextCursor: next, Fingerprint: "fixture-fingerprint", Availability: "complete"})
				case "fs.read":
					fileReads++
					var tgt struct{ Root, Path string }
					var a struct {
						Offset int
						Length int
					}
					json.Unmarshal(target, &tgt)
					json.Unmarshal(args, &a)
					if tgt.Root != sdk.SandboxRoot || tgt.Path != cap.Path || a.Offset < 0 || a.Offset >= len(raw) {
						return denyBody("outside fixture file"), true
					}
					end := min(a.Offset+a.Length, len(raw))
					return success(map[string]any{"data_b64": base64.StdEncoding.EncodeToString(raw[a.Offset:end]), "size": len(raw), "eof": end == len(raw)})
				default:
					t.Errorf("unexpected reach: %s", op)
					return denyBody("only reads admitted"), true
				}
			}
			h := startHarness(t, k, "PLANNING_ASSESSMENT_FIXTURE="+string(cfg))
			if denied {
				if got := h.failure("assess", map[string]any{}); got != "POLICY_DENY" {
					t.Fatal(got)
				}
				if fileReads != 0 {
					t.Fatal("file read after denial")
				}
			} else {
				started := time.Now()
				got := h.result("assess", map[string]any{})
				if got["finding"] != "passed" || got["basis"] != "controlled_harness_capture" {
					t.Fatal(got)
				}
				if fileReads != 1 {
					t.Fatal("unexpected I/O", fileReads)
				}
				t.Logf("native SDK fixture assessment including child startup: %s; one report read; zero writes", time.Since(started))
			}
		})
	}
}
