package tart

import (
	"context"
	"strings"
	"testing"

	"github.com/colony-2/pulse-tart/internal/backend"
)

func TestFixtureTunnelStopsOnNativeFailure(t *testing.T) {
	worker := backend.VM{Spec: backend.Spec{Name: "worker"}, UID: "original"}
	calls := 0
	_, err := attachFixtureTunnel(t.Context(), "tart", OrchardConfig{}, backend.VM{}, worker,
		func(context.Context, string) (backend.VM, error) {
			calls++
			v := worker
			v.Status, v.PowerState = "failed", "suspended"
			return v, nil
		})
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "terminated before SSH") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
