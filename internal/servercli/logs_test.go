package servercli

import (
	"bytes"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
)

func TestLogsReadsRetainedOutputWithoutController(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	id := "launch"
	scope := (config.Settings{Config: config.Config{OwnerID: "server"}, Kind: "vz", Endpoint: home}).Scope()
	payload := []byte{0, 255, 'x'}
	p := logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: id, End: 3, Closed: true}, Frames: []logstore.Frame{{Data: payload, CRC: crc32.ChecksumIEEE(payload)}}, Next: 3}
	if e := logstore.Mirror(filepath.Join(root, scope), id, p, true); e != nil {
		t.Fatal(e)
	}
	cmd := NewLogsCommand()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", filepath.Join(home, "absent.json"), "--vm-home", home, "--log-directory", root, "--launch-id", id})
	// Explicit missing config is an error; use an empty valid file.
	if e := os.WriteFile(filepath.Join(home, "absent.json"), []byte(`{"version":1,"flags":{}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := cmd.Execute(); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(out.Bytes(), payload) || stderr.Len() != 0 {
		t.Fatal(out.Bytes(), stderr.String())
	}
}
