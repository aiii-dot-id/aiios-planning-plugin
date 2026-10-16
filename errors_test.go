package main

import (
	"errors"
	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/t3"
	"testing"
)

func TestFailureClassificationPreservesHostDenials(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{{t3.ErrInvalid, "OPERATION_ARGUMENT_INVALID"}, {t3.ErrPending, "CALIBRATION_PENDING"}, {errors.New("storage unavailable"), "CALIBRATION_UNAVAILABLE"}} {
		var op *sdk.OperationError
		if !errors.As(t3Failure(tc.err), &op) || op.ReasonCode != tc.code {
			t.Fatal(tc, t3Failure(tc.err))
		}
	}
	denied := &sdk.Denied{Code: -32000, ReasonCode: "POLICY_DENY"}
	if t3Failure(denied) != denied {
		t.Fatal("host denial relabeled")
	}
	quota := sdk.Fail("KV_QUOTA_EXCEEDED", "full")
	if t3Failure(quota) != quota {
		t.Fatal("host storage refusal relabeled")
	}
}
