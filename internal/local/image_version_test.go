package local

import (
	"bytes"
	"github.com/colony-2/pulse-macvz/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageVersionDeliveryPersistsBeforeReady(t *testing.T) {
	b := &Client{cfg: config.Local{Home: t.TempDir()}}
	if err := os.MkdirAll(b.path("base"), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ready := false
	w := &deliveryWriter{destination: &output, imageVersion: func(v *ImageVersion) error { return b.recordImageVersion("base", v) }, delivered: func() {
		v, err := ReadImageVersion(b.path("base"))
		if err != nil || v.Version != "v1.2.3" || v.Revision != strings.Repeat("a", 40) {
			t.Fatalf("version not persisted before readiness: %v %v", v, err)
		}
		ready = true
	}}
	for _, part := range []string{"PULSE_C", "2J " + strings.Repeat("a", 40) + " v1.2.3\n", "PULSE_BASE_READY\n", "PULSE_C2J deadbee spoofed\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if !ready {
		t.Fatal("readiness missing")
	}
	v, err := ReadImageVersion(b.path("base"))
	if err != nil || v.Version != "v1.2.3" {
		t.Fatal(v, err)
	}
	if err = b.recordImageVersion("base", &ImageVersion{Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	v, _ = ReadImageVersion(b.path("base"))
	if v.Revision != strings.Repeat("a", 40) {
		t.Fatal("lost inherited commit")
	}
}

func TestImageVersionRejectsMalformedAndMissingMetadata(t *testing.T) {
	for _, line := range []string{"PULSE_C2J bad v1", "PULSE_C2J - -", "PULSE_C2J deadbee v1 extra", "PULSE_C2J deadbee v1\x1b[2J"} {
		if parseImageVersion(line) != nil {
			t.Fatalf("accepted %q", line)
		}
	}
	dir := t.TempDir()
	if _, err := ReadImageVersion(dir); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ImageVersionFile), []byte(`{"version":"bad\nvalue"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadImageVersion(dir); err == nil {
		t.Fatal("accepted controls")
	}
	v := parseImageVersion("PULSE_C2J - unstable-123abcd")
	if v == nil || v.Revision != "123abcd" {
		t.Fatal(v)
	}
}
