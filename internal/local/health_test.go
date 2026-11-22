package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/workerhealth"
)

func TestHealthReadOnlyIdentityAndCompatibility(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "vms", "job")
	os.MkdirAll(dir, 0700)
	identity := []byte(`{"scope":"owner","native":{"uid":"uid"}}`)
	os.WriteFile(filepath.Join(dir, "pulse.json"), identity, 0600)
	calls := 0
	ip := func(context.Context, string) (string, error) { calls++; return "invalid", nil }
	for _, bad := range []struct{ name, uid, scope string }{{"../job", "uid", "owner"}, {"job", "wrong", "owner"}, {"job", "uid", "other"}} {
		if _, err := ProbeHealth(context.Background(), home, bad.scope, bad.name, bad.uid, "tenant", "job", ip); err != workerhealth.ErrIdentity {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(ImageVersion{Version: "old"})
	os.WriteFile(filepath.Join(dir, ImageVersionFile), data, 0600)
	o, err := ProbeHealth(context.Background(), home, "owner", "job", "uid", "tenant", "job", ip)
	if err != nil || o.Supported || calls != 0 {
		t.Fatal(o, err, calls)
	}
	data, _ = json.Marshal(ImageVersion{Version: "new", HealthProbe: true})
	os.WriteFile(filepath.Join(dir, ImageVersionFile), data, 0600)
	o, err = ProbeHealth(context.Background(), home, "owner", "job", "uid", "tenant", "job", ip)
	if err != nil || !o.Supported || o.Failure != "guest IP unavailable" || calls != 1 {
		t.Fatal(o, err, calls)
	}
	ip = func(context.Context, string) (string, error) {
		os.WriteFile(filepath.Join(dir, "pulse.json"), []byte(`{"scope":"owner","native":{"uid":"replacement"}}`), 0600)
		return "invalid", nil
	}
	if _, err = ProbeHealth(context.Background(), home, "owner", "job", "uid", "tenant", "job", ip); err != workerhealth.ErrIdentity {
		t.Fatal("changed native identity trusted", err)
	}
}
func TestHealthCapabilityVersionMarker(t *testing.T) {
	v := parseImageVersion("PULSE_C2J abcdef0 unstable-abcdef0 health-v1")
	if v == nil || !v.HealthProbe {
		t.Fatal(v)
	}
	if parseImageVersion("PULSE_C2J abcdef0 unstable-abcdef0 other") != nil {
		t.Fatal("invalid marker accepted")
	}
}
