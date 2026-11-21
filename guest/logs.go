package main

import (
	"encoding/json"
	"github.com/colony-2/pulse-macvz/guest/logstore"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A read cannot cross an assignment boundary, even if a VM name is reused.
func readLogs(dir string, in io.Reader, out io.Writer) error {
	var q logstore.Request
	d := json.NewDecoder(io.LimitReader(in, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&q) != nil || !launchPattern.MatchString(q.LaunchID) {
		return invalid
	}
	intent, e := os.ReadFile(filepath.Join(dir, "intent"))
	if os.IsNotExist(e) {
		f, err := os.Open(filepath.Join(dir, "launch.json"))
		if os.IsNotExist(err) {
			return json.NewEncoder(out).Encode(logstore.Page{Pending: true, Next: q.Offset})
		}
		if err != nil {
			return invalid
		}
		envelope, err := decode(f)
		f.Close()
		if err != nil || envelope.LaunchID != q.LaunchID {
			return invalid
		}
	} else if e != nil || string(intent) != q.LaunchID {
		return invalid
	}
	p, e := logstore.Read(filepath.Join(dir, "logs"), q.LaunchID, q.Offset)
	if e != nil {
		return e
	}
	return json.NewEncoder(out).Encode(p)
}

func ackLogs(dir string, in io.Reader) error {
	var q logstore.Request
	if json.NewDecoder(io.LimitReader(in, 4096)).Decode(&q) != nil {
		return invalid
	}
	intent, err := os.ReadFile(filepath.Join(dir, "intent"))
	if err != nil || string(intent) != q.LaunchID {
		return invalid
	}
	m, err := logstore.Load(logstore.Dir(filepath.Join(dir, "logs"), q.LaunchID), q.LaunchID)
	if err != nil || !m.Closed || m.End != q.Offset {
		return invalid
	}
	return writeAtomic(dir, "logs-ack", []byte(q.LaunchID))
}
func waitLogAck(dir string, limit time.Duration) {
	intent, err := os.ReadFile(filepath.Join(dir, "intent"))
	if err != nil {
		return
	}
	m, err := logstore.Load(logstore.Dir(filepath.Join(dir, "logs"), string(intent)), string(intent))
	if err != nil || !m.Closed {
		return
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dir, "logs-ack"))
		if err == nil && string(b) == string(intent) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func captureOptions(env map[string]string) (int, int, bool) {
	size, backups, compress := 16, 3, true
	if env["PULSE_LOG_CONFIG_VERSION"] != "1" {
		return size, backups, compress
	}
	if n, err := strconv.Atoi(env["PULSE_LOG_SEGMENT_MIB"]); err == nil && n >= 1 && n <= 64 {
		size = n
	}
	if n, err := strconv.Atoi(env["PULSE_LOG_BACKUPS"]); err == nil && n >= 1 && n <= 16 {
		backups = n
	}
	if b, err := strconv.ParseBool(env["PULSE_LOG_COMPRESS"]); err == nil {
		compress = b
	}
	return size, backups, compress
}
