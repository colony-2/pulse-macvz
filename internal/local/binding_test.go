package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindingSurvivesDeviceRenumbering(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "uuid", true: "legacy"}[legacy], func(t *testing.T) {
			b, m, spec := fixture(t)
			b.volumeUUID = func(string) (string, error) { return "volume-one", nil }
			if err := os.MkdirAll(b.path(spec.Name), 0700); err != nil {
				t.Fatal(err)
			}
			r := &record{Spec: spec, UID: "owner", MAC: "02:00:00:00:00:42", Phase: "started"}
			m.vms[spec.Name] = hardware{CPU: spec.CPU, Memory: spec.Memory, MACAddress: r.MAC, State: "stopped"}
			if err := b.binding(r, true); err != nil {
				t.Fatal(err)
			}
			device := r.Device
			r.Device++ // Saved mount number from the previous host boot.
			if legacy {
				r.VolumeUUID = ""
			}
			b.records[spec.Name] = r
			props := json.RawMessage(`{"job":{"launch_id":"preserved"}}`)
			b.properties[spec.Name] = props
			if err := b.save(r); err != nil {
				t.Fatal(err)
			}
			b = reopenProperties(t, b, m, "")
			b.volumeUUID = func(string) (string, error) { return "volume-one", nil }
			all, err := b.ReadVMProperties()
			if err != nil {
				t.Fatal(err)
			}
			if err = b.WriteVMProperties(all); err != nil {
				t.Fatal(err)
			}
			data, err := privateFile(b.artifact(spec.Name, "json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved vmFile
			if err = json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Native.Device != device || saved.Native.VolumeUUID != "volume-one" || saved.Native.UID != "owner" || !bytes.Equal(saved.Properties, props) {
				t.Fatal("migration did not preserve ownership/properties and update volume identity")
			}
			if legacy && (len(m.calls) != 1 || m.calls[0][0] != "get") {
				t.Fatal("legacy upgrade must only inspect hardware", m.calls)
			}
			if !legacy && len(m.calls) != 0 {
				t.Fatal("UUID comparison unexpectedly called Tart", m.calls)
			}
		})
	}
}

func TestBindingRejectsChangedIdentityWithoutWriting(t *testing.T) {
	for _, kind := range []string{"owner", "inode", "volume", "volume-unavailable", "legacy-mac", "legacy-cpu", "legacy-memory", "legacy-unobservable", "legacy-partial", "legacy-no-volume"} {
		t.Run(kind, func(t *testing.T) {
			b, m, spec := fixture(t)
			b.volumeUUID = func(string) (string, error) { return "volume-one", nil }
			if err := os.MkdirAll(b.path(spec.Name), 0700); err != nil {
				t.Fatal(err)
			}
			r := &record{Spec: spec, UID: "owner", MAC: "02:00:00:00:00:42"}
			if err := b.binding(r, true); err != nil {
				t.Fatal(err)
			}
			h := hardware{CPU: spec.CPU, Memory: spec.Memory, MACAddress: r.MAC, State: "stopped"}
			r.Device++
			if strings.HasPrefix(kind, "legacy-") {
				r.VolumeUUID = ""
			}
			switch kind {
			case "owner":
				if err := os.WriteFile(filepath.Join(b.path(spec.Name), ".pulse-owner"), []byte("other"), 0600); err != nil {
					t.Fatal(err)
				}
			case "inode":
				r.Inode++
			case "volume":
				r.VolumeUUID = "other-volume"
			case "volume-unavailable":
				b.volumeUUID = func(string) (string, error) { return "", errors.New("unavailable") }
			case "legacy-mac":
				h.MACAddress = "02:00:00:00:00:99"
			case "legacy-cpu":
				h.CPU++
			case "legacy-memory":
				h.Memory++
			case "legacy-partial":
				r.MAC = ""
			case "legacy-no-volume":
				b.volumeUUID = func(string) (string, error) { return "", nil }
			}
			if kind != "legacy-unobservable" {
				m.vms[spec.Name] = h
			}
			b.records[spec.Name] = r
			if err := b.save(r); err != nil {
				t.Fatal(err)
			}
			before, err := privateFile(b.artifact(spec.Name, "json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = b.WriteVMProperties(nil); err == nil {
				t.Fatal("mismatched identity accepted")
			}
			after, err := privateFile(b.artifact(spec.Name, "json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected binding modified metadata", err)
			}
		})
	}
}
