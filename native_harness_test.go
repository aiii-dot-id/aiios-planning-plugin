//go:build !wasm_unknown

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
	"time"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
)

type fakeBroker struct {
	readOnlyCall func(string, json.RawMessage, json.RawMessage) ([]byte, bool)
}

func newFakeBroker() *fakeBroker { return &fakeBroker{} }

func (k *fakeBroker) call(operation string, target, arguments json.RawMessage) ([]byte, bool) {
	if k.readOnlyCall != nil {
		return k.readOnlyCall(operation, target, arguments)
	}
	return denyBody("the harness broker grants nothing; " + operation + " is not granted"), true
}

func denyBody(message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"code":    -32000,
		"message": message,
		"data":    map[string]string{"reasonCode": "POLICY_DENY", "denied_at": "capability_evaluation"},
	})
	return b
}

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
	os.Exit(0)
}

type harness struct {
	t           *testing.T
	cmd         *exec.Cmd
	in          io.WriteCloser
	rd          io.Reader
	broker      *fakeBroker
	nextID      int
	waited      bool
	workSession map[string]any
}

func startHarness(t *testing.T, broker *fakeBroker, extraEnv ...string) *harness {
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
	h := &harness{t: t, cmd: cmd, in: in, rd: out, broker: broker}
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
			body, isErr := h.broker.call(params.Operation, params.Target, params.Arguments)
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
