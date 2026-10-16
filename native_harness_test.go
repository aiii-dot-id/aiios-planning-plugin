//go:build !wasm_unknown

package main

// This harness runs the registered handlers over the public SDK frame transport.
// The broker is simulated and rejects undeclared operations; this does not
// replace integration testing through an installed host.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

// fakeKV is the broker's key-value store, guarded because the child and
// the parent both touch it across the frame boundary.
type fakeKV struct {
	readOnlyCall func(string, json.RawMessage, json.RawMessage) ([]byte, bool)
	mu           sync.Mutex
	m            map[string]string
	files        map[string][]byte
	puts         int
	failAt       int
	failAfter    bool
}

func newFakeKV() *fakeKV { return &fakeKV{m: map[string]string{}, files: map[string][]byte{}} }

// call answers one invoke.call. Returns (resultBody, isError).
func (k *fakeKV) call(operation string, target, arguments json.RawMessage) ([]byte, bool) {
	if k.readOnlyCall != nil {
		return k.readOnlyCall(operation, target, arguments)
	}
	if strings.HasPrefix(operation, "fs.") {
		return k.fileCall(operation, target, arguments)
	}
	switch operation {
	case "kv.put":
		// The KEY travels in target; only the VALUE travels in arguments
		// (hostcall.go:279-283 — InvokeCall("kv.put", {"key":k}, {"value":v})).
		var tgt struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(target, &tgt); err != nil {
			return failedBody("KV_ARGUMENT_INVALID", err.Error()), false
		}
		var args struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(arguments, &args); err != nil {
			return failedBody("KV_ARGUMENT_INVALID", err.Error()), false
		}
		key := tgt.Key
		k.mu.Lock()
		k.puts++
		fail := k.puts == k.failAt
		if fail && !k.failAfter {
			k.mu.Unlock()
			return failedBody("INJECTED_WRITE", "injected before commit"), false
		}
		k.m[key] = args.Value
		k.mu.Unlock()
		if fail {
			return failedBody("INJECTED_WRITE", "injected after commit"), false
		}
		body, _ := json.Marshal(map[string]any{
			"status": "succeeded",
			"operation_result": map[string]any{
				"stored": true, "key": key, "value_bytes": len(args.Value), "scope": "persistent",
			},
		})
		return body, false

	case "kv.get":
		// Key in target; arguments is nil (hostcall.go:267 —
		// InvokeCall("kv.get", {"key":key}, nil)).
		var args struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(target, &args); err != nil {
			return failedBody("KV_ARGUMENT_INVALID", err.Error()), false
		}
		k.mu.Lock()
		v, ok := k.m[args.Key]
		k.mu.Unlock()
		if !ok {
			return failedBody("KV_NOT_FOUND", "no such key: "+args.Key), false
		}
		body, _ := json.Marshal(map[string]any{
			"status":           "succeeded",
			"operation_result": map[string]any{"key": args.Key, "value": v},
		})
		return body, false

	case "kv.delete":
		// Key in target; arguments is nil (hostcall.go:289).
		var args struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(target, &args); err != nil {
			return failedBody("KV_ARGUMENT_INVALID", err.Error()), false
		}
		k.mu.Lock()
		_, ok := k.m[args.Key]
		delete(k.m, args.Key)
		k.mu.Unlock()
		body, _ := json.Marshal(map[string]any{
			"status":           "succeeded",
			"operation_result": map[string]any{"deleted": ok},
		})
		return body, false

	case "kv.list":
		// Prefix in target; limit in arguments (hostcall.go:302-308).
		var args struct {
			Prefix string `json:"prefix"`
		}
		if err := json.Unmarshal(target, &args); err != nil {
			return failedBody("KV_ARGUMENT_INVALID", err.Error()), false
		}
		k.mu.Lock()
		keys := []string{}
		for key := range k.m {
			if strings.HasPrefix(key, args.Prefix) {
				keys = append(keys, key)
			}
		}
		k.mu.Unlock()
		body, _ := json.Marshal(map[string]any{
			"status":           "succeeded",
			"operation_result": map[string]any{"keys": keys, "truncated": false},
		})
		return body, false
	}
	// Any other reach is a denial, so an undeclared capability cannot
	// pass unnoticed. A deny is a JSON-RPC error — unlike a missing key,
	// which is a failed result — and the audited shape
	// (cmd/aiisdk/harness.go:215-221) is code -32000 with
	// data.reasonCode, not the failed-result envelope.
	return denyBody("the harness broker grants kv only; " + operation + " is not granted"), true
}

// denyBody is the broker's capability denial: a JSON-RPC error whose
// data carries reasonCode (harness.go:215-221).
func denyBody(message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"code":    -32000,
		"message": message,
		"data":    map[string]string{"reasonCode": "POLICY_DENY", "denied_at": "capability_evaluation"},
	})
	return b
}

// failedBody is the broker's FAILED RESULT — a result body, never a
// JSON-RPC error. The audited shape (cmd/aiisdk/harness.go:228-234,
// exercised at pkg/aiiosdk/hostcall_test.go:93):
//
//	{"success":false,"ok":false,"status":"failed","reason":R,
//	 "reasonCode":R,"reason_code":R,"operation_result":null,...}
//
// The distinction is load-bearing, not cosmetic: decodeHostReply turns
// a failed RESULT into an *OperationError carrying reason_code, which
// is how KVClient.Get reports a missing key as (value "", found false,
// nil error) — hostcall.go:270. A JSON-RPC error instead surfaces as a
// transport fault, so the plugin sees a broken broker rather than an
// absent key.
func failedBody(code, reason string) []byte {
	q, _ := json.Marshal(code)
	r, _ := json.Marshal(reason)
	_ = r
	return []byte(`{"success":false,"ok":false,"status":"failed","reason":` + string(q) +
		`,"reasonCode":` + string(q) + `,"reason_code":` + string(q) +
		`,"operation_result":null,"external_receipt":{}}`)
}

// ─── The child: the real plugin, serving ───────────────────────────

func TestNativeHarnessChild(t *testing.T) {
	if os.Getenv("PLANNING_TEST_CHILD") != "1" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "CHILD_PANIC: %v\n%s", r, debug.Stack())
		}
	}()
	plugin := newPlanningPlugin()
	if raw := os.Getenv("PLANNING_ASSESSMENT_FIXTURE"); raw != "" {
		plugin = assessmentFixturePlugin(t, []byte(raw))
	}
	if err := plugin.Serve("planning-test-ready"); err != nil {
		fmt.Fprintf(os.Stderr, "CHILD_SERVE_ERROR: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "CHILD_SERVE_OK")
	os.Exit(0) // testing's PASS line must not enter the framed stream.
}

// harness starts one child and returns a call function.
type harness struct {
	t           *testing.T
	cmd         *exec.Cmd
	in          io.WriteCloser
	rd          io.Reader
	kv          *fakeKV
	nextID      int
	waited      bool
	workSession map[string]any
}

func startHarness(t *testing.T, kv *fakeKV, extraEnv ...string) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeHarnessChild$")
	cmd.Env = append(os.Environ(), "PLANNING_TEST_CHILD=1", "AIISDK_DESCRIBE=0", "GORACE=atexit_sleep_ms=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var childErr bytes.Buffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cmd: cmd, in: in, rd: out, kv: kv}
	t.Cleanup(func() {
		if h.waited {
			return
		}
		h.in.Close()
		cancel()
		_ = cmd.Wait()
		if childErr.Len() > 0 {
			t.Logf("child stderr: %s", childErr.String())
		}
	})
	return h
}

// call sends one invoke.call and returns the decoded response.
func (h *harness) call(operation string, args map[string]any) map[string]json.RawMessage {
	h.t.Helper()
	h.nextID++
	argBytes, err := json.Marshal(args)
	if err != nil {
		h.t.Fatal(err)
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      h.nextID,
		"method":  "invoke.call",
		"params": map[string]any{
			"operation": operation,
			"arguments": json.RawMessage(argBytes),
		},
	}
	if h.workSession != nil {
		req["params"].(map[string]any)["_host_work_session"] = h.workSession
	}
	frame, err := json.Marshal(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := sdk.WriteFrame(h.in, frame, sdk.MaxControlFrameBytes); err != nil {
		h.t.Fatal(err)
	}
	for {
		reply, err := sdk.ReadFrame(h.rd, sdk.MaxControlFrameBytes)
		if err != nil {
			h.t.Fatalf("native read after %s: %v", operation, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(reply, &obj); err != nil {
			h.t.Fatal(err)
		}
		// A hostcall: answer it from the fake broker and keep reading.
		if method, ok := obj["method"]; ok {
			if string(method) != `"invoke.call"` {
				h.t.Fatalf("unexpected method: %s", method)
			}
			var params struct {
				Operation string          `json:"operation"`
				Target    json.RawMessage `json:"target"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(obj["params"], &params); err != nil {
				h.t.Fatal(err)
			}
			body, isErr := h.kv.call(params.Operation, params.Target, params.Arguments)
			field := "result"
			if isErr {
				field = "error"
			}
			response, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": obj["id"], field: json.RawMessage(body),
			})
			if err != nil {
				h.t.Fatal(err)
			}
			if err := sdk.WriteFrame(h.in, response, sdk.MaxControlFrameBytes); err != nil {
				h.t.Fatal(err)
			}
			continue
		}
		// The response to our request.
		var wantID int
		if err := json.Unmarshal(obj["id"], &wantID); err != nil {
			h.t.Fatalf("response id not a number: %s", obj["id"])
		}
		if wantID != h.nextID {
			h.t.Fatalf("response id mismatch: want %d, got %d", h.nextID, wantID)
		}
		return obj
	}
}

// result decodes a successful operation_result, failing on an error reply.
func (h *harness) result(operation string, args map[string]any) map[string]any {
	h.t.Helper()
	obj := h.call(operation, args)
	if e, ok := obj["error"]; ok {
		h.t.Fatalf("%s returned an error: %s", operation, e)
	}
	raw, ok := obj["result"]
	if !ok {
		h.t.Fatalf("%s returned neither result nor error", operation)
	}
	var env struct {
		Status          string          `json:"status"`
		OperationResult json.RawMessage `json:"operation_result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("%s: %v (raw %s)", operation, err, raw)
	}
	if env.Status != "" && env.Status != "succeeded" {
		h.t.Fatalf("%s status=%s raw=%s", operation, env.Status, raw)
	}
	out := map[string]any{}
	if len(env.OperationResult) > 0 {
		if err := json.Unmarshal(env.OperationResult, &out); err != nil {
			h.t.Fatalf("%s: operation_result not an object: %s", operation, env.OperationResult)
		}
	}
	return out
}

// failure returns the refusal reason for an operation that should fail.
func (h *harness) failure(operation string, args map[string]any) string {
	h.t.Helper()
	obj := h.call(operation, args)
	if e, ok := obj["error"]; ok {
		var errObj struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(e, &errObj)
		return errObj.Reason
	}
	raw := obj["result"]
	var env struct {
		Status          string          `json:"status"`
		Reason          string          `json:"reason"`
		ReasonCode      string          `json:"reasonCode"`
		OperationResult json.RawMessage `json:"operation_result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("%s: %v", operation, err)
	}
	if env.ReasonCode != "" {
		return env.ReasonCode
	}
	if env.Reason != "" {
		return env.Reason
	}
	var or map[string]any
	if len(env.OperationResult) > 0 {
		_ = json.Unmarshal(env.OperationResult, &or)
	}
	if r, ok := or["reason"].(string); ok {
		return r
	}
	h.t.Fatalf("%s was expected to fail but answered: %s", operation, raw)
	return ""
}

// ─── The end-to-end journey ────────────────────────────────────────

func TestNativeCalibratedJourney(t *testing.T) {
	h := startHarness(t, newFakeKV())
	host := map[string]any{"version": 1, "available": true, "session_id": "ws", "stream_id": "gen1", "emitted": 0, "settled": 0, "loss_count": 0}
	call := func(mode string, extra map[string]any) map[string]any {
		a := map[string]any{"mode": mode, "plan_id": "p", "_host_calibration": host}
		for k, v := range extra {
			a[k] = v
		}
		return h.result("plan", a)
	}
	call("begin", map[string]any{"family": "test", "declared_steps": 1, "budget_calls": 10, "falsifier_class": "failed", "expected_evidence_class": "passed", "consequence_class": "local"})
	call("apply", map[string]any{"apply_id": "a", "step_id": "s1", "kind_class": "refinement", "from_class": "a", "to_class": "b", "count": 1})
	host["emitted"] = 1
	reason := h.failure("plan", map[string]any{"mode": "grade", "plan_id": "p", "result_class": "passed", "_host_calibration": host})
	if reason != "CALIBRATION_PENDING" {
		t.Fatal(reason)
	}
	h.result("observe", map[string]any{"stream_id": "gen1", "sequence": 1, "topic": "tool.called", "at": "2026-09-24T00:00:00Z", "payload": map[string]any{"tool": "read", "failed": false, "duration_ms": 0, "actor": "main", "session": "ws"}})
	host["settled"] = 1
	host["emitted"] = 100
	got := call("grade", map[string]any{"result_class": "passed"})
	if got["observed_calls"] != float64(1) || got["host_calibration"] != "complete" {
		t.Fatal(got)
	}
	host["stream_id"] = "gen2"
	got = call("status", nil)
	if got["host_calibration"] != "complete" {
		t.Fatal(got)
	}
	h.failure("plan", map[string]any{"mode": "begin", "observed_calls": 3})
}

func (k *fakeKV) fileCall(operation string, target, arguments json.RawMessage) ([]byte, bool) {
	var tgt struct{ Root, Path string }
	json.Unmarshal(target, &tgt)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.files == nil {
		k.files = map[string][]byte{}
	}
	success := func(v any) ([]byte, bool) {
		b, _ := json.Marshal(map[string]any{"status": "succeeded", "operation_result": v})
		return b, false
	}
	raw, exists := k.files[tgt.Path]
	sum := func(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
	switch operation {
	case "fs.read":
		if !exists {
			return failedBody("FS_NOT_FOUND", "missing"), false
		}
		return success(map[string]any{"data_b64": base64.StdEncoding.EncodeToString(raw), "size": len(raw), "eof": true, "sha256": sum(raw)})
	case "fs.publish":
		var a struct {
			Data     string `json:"data_b64"`
			Expected string `json:"expected_sha256"`
			Absent   bool   `json:"expected_absent"`
		}
		json.Unmarshal(arguments, &a)
		if a.Absent && exists || a.Expected != "" && (!exists || a.Expected != sum(raw)) {
			return failedBody("FS_CONFLICT", "conflict"), false
		}
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return failedBody("FS_ARGUMENT_INVALID", "bad bytes"), false
		}
		k.files[tgt.Path] = data
		return success(map[string]any{"size": len(data), "sha256": sum(data), "durable": true, "replaced": exists, "durability": "synced"})
	case "fs.list":
		entries := []map[string]any{}
		seen := map[string]bool{}
		prefix := strings.TrimSuffix(tgt.Path, "/") + "/"
		for name := range k.files {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			rest := strings.TrimPrefix(name, prefix)
			first := strings.Split(rest, "/")[0]
			if seen[first] {
				continue
			}
			seen[first] = true
			entries = append(entries, map[string]any{"name": path.Base(first), "dir": strings.Contains(rest, "/"), "symlink": false})
		}
		return success(map[string]any{"entries": entries, "truncated": false})
	}
	return denyBody("unknown file operation"), true
}

func TestNativeSharedWriteFailure(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			k := newFakeKV()
			h := startHarness(t, k)
			host := map[string]any{"version": 1, "available": true, "session_id": "ws", "stream_id": "g", "emitted": 0, "settled": 0, "loss_count": 0}
			args := map[string]any{"mode": "begin", "plan_id": "p", "family": "f", "declared_steps": 1, "budget_calls": 10, "falsifier_class": "failed", "expected_evidence_class": "passed", "consequence_class": "local", "_host_calibration": host}
			k.mu.Lock()
			k.failAt = 2
			k.failAfter = after
			k.mu.Unlock()
			if got := h.failure("plan", args); got != "INJECTED_WRITE" {
				t.Fatal(got)
			}
			k.mu.Lock()
			k.failAt = 0
			_, committed := k.m["t3/plan/p"]
			k.mu.Unlock()
			if committed != after {
				t.Fatal("unexpected commit boundary", committed)
			}
			if !committed {
				h.result("plan", args)
			}
			h.result("observe", map[string]any{"stream_id": "g", "sequence": 1, "topic": "tool.called", "at": "2026-09-24T00:00:00Z", "payload": map[string]any{"tool": "read", "failed": false, "duration_ms": 0, "actor": "main", "session": "ws"}})
			host["emitted"] = 1
			host["settled"] = 1
			got := h.result("plan", map[string]any{"mode": "status", "plan_id": "p", "_host_calibration": host})
			measured := got["measurement"].(map[string]any)
			if measured["count"] != float64(1) || measured["cursor"] != float64(1) {
				t.Fatal(got)
			}
		})
	}
}

func TestNativeAbandonOldSession(t *testing.T) {
	h := startHarness(t, newFakeKV())
	host := map[string]any{"version": 1, "available": true, "session_id": "old-session", "stream_id": "gen1", "emitted": 0, "settled": 0, "loss_count": 0}
	h.result("plan", map[string]any{"mode": "begin", "plan_id": "unfinished", "family": "test", "declared_steps": 1, "budget_calls": 10, "falsifier_class": "failed", "expected_evidence_class": "passed", "consequence_class": "local", "_host_calibration": host})
	host["session_id"] = "new-session"
	args := map[string]any{"mode": "abandon", "plan_id": "unfinished", "_host_calibration": host}
	got := h.result("plan", args)
	if got["state"] != "closed" || got["abandoned"] != true || got["host_calibration"] != "unresolved" || got["session_id"] != "old-session" {
		t.Fatal(got)
	}
	for _, key := range []string{"grade_class", "observed_calls", "step_count_ok", "falsifier_ok"} {
		if _, ok := got[key]; ok {
			t.Fatal("invented verdict", got)
		}
	}
	h.result("plan", args)
}
