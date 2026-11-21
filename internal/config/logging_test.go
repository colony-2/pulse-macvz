package config

import (
	"testing"
	"time"
)

func TestLoggingDefaultsAndOwnership(t *testing.T) {
	c, e := (Logging{}).Normalize()
	if e != nil || c.MaxBytes != 1<<30 || c.MaxAge != 7*24*time.Hour || !*c.Compress {
		t.Fatal(c, e)
	}
	b := false
	c, e = (Logging{Directory: "/private/logs", Compress: &b}).Normalize()
	b = true
	if e != nil || *c.Compress {
		t.Fatal("configuration aliases caller")
	}
	for _, bad := range []Logging{{Directory: "relative"}, {MaxBytes: -1}, {MaxAge: -time.Second}} {
		if _, e := bad.Normalize(); e == nil {
			t.Fatal("bad configuration", bad)
		}
	}
}
