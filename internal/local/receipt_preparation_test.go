package local

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTemplateReceiptRejectsWrongBootAndMAC(t *testing.T) {
	for _, mismatch := range []string{"", "boot", "mac", "missing"} {
		t.Run(mismatch, func(t *testing.T) {
			b, _, spec := fixture(t)
			if _, err := b.Create(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			b.Close()
			r := b.records[spec.Name]
			boot := "00000000-0000-0000-0000-000000000001"
			receiptBoot, mac := boot, r.MAC
			if mismatch == "boot" {
				receiptBoot = "00000000-0000-0000-0000-000000000002"
			}
			if mismatch == "mac" {
				mac = "02:00:00:00:00:ff"
			}
			data := fmt.Sprintf("PULSE_BASE_OBSERVATION %s ready 12\nPULSE_TEMPLATE_READY {\"boot_id\":%q,\"mac\":%q}\n", boot, receiptBoot, mac)
			if mismatch == "missing" {
				data = strings.Split(data, "\n")[0]
			}
			err := b.recordTemplateReady(r, []byte(data))
			if (err == nil) != (mismatch == "") || b.deliveryMarker(spec.Name) != (mismatch == "") {
				t.Fatal("invalid receipt accepted or valid receipt lost", err)
			}
		})
	}
}

func TestPreparationReceiptRecoveryNeverReplaysStartup(t *testing.T) {
	for _, kind := range []string{"template", "companion"} {
		for _, state := range []string{"ready", "failed", "preparing"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				b, _, spec := fixture(t)
				if _, err := b.Create(t.Context(), spec); err != nil {
					t.Fatal(err)
				}
				b.Close()
				r := b.records[spec.Name]
				b.closed, b.ctx = false, t.Context()
				s := &basePreparation{uid: r.UID, kind: kind}
				observe, shutdown := observeMacTemplate, ""
				if kind == "companion" {
					r.Spec.OS = "linux"
					observe, shutdown = observeCompanionBase, shutdownReadyCompanionBase
				}
				observations, shutdowns := 0, 0
				exec := func(_ context.Context, name, uid string, script []byte) ([]byte, error) {
					if name != spec.Name || uid != r.UID {
						t.Fatal("wrong VM")
					}
					if string(script) == "expected='26.1'\n"+observe+observeImageVersion {
						observations++
						data := "PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 " + state + " 12\n"
						if kind == "template" && state == "ready" {
							data += fmt.Sprintf("PULSE_TEMPLATE_READY {\"boot_id\":\"00000000-0000-0000-0000-000000000001\",\"mac\":%q}\n", r.MAC)
						}
						return []byte(data), nil
					}
					if shutdown != "" && string(script) == shutdown {
						shutdowns++
						if !b.deliveryMarker(name) {
							t.Fatal("shutdown before readiness persisted")
						}
						return nil, nil
					}
					t.Fatal("unexpected or replayed startup command")
					return nil, nil
				}
				b.wg.Add(1)
				b.monitorBase(spec.Name, r, s, "26.1", time.Now().Add(-time.Minute), exec)
				if observations != 1 || !s.done {
					t.Fatal("expired preparation not observed once", s)
				}
				if state == "ready" {
					if s.err != nil || !r.Delivered {
						t.Fatal("receipt recovery failed", s)
					}
					if (shutdowns == 1) != (kind == "companion") {
						t.Fatal("template shut down instead of left for suspension")
					}
				} else if s.err == nil || shutdowns != 0 || r.Delivered {
					t.Fatal("failed/unfinished preparation accepted", s)
				}
			})
		}
	}
}
