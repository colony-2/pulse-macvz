package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Only the provider's verified, unused macOS base builders use this observer.
// It never replays StartupScript: preparation survives SSH in a guest service,
// whereas a job with an uncertain execution outcome must never be replayed.
const observeMacBase = `set -eu
boot=$(/usr/sbin/sysctl -n kern.bootsessionuuid)
state=absent
if [[ $(/usr/bin/sw_vers -productVersion) != "$expected" ]]; then state=failed
elif sudo -n test -f /var/db/pulse-tart-base/failed; then state=failed
elif sudo -n test -f /var/db/pulse-tart-base/ready; then
  state=ready
  for tool in c2j pulse-tart-guest git node pnpm uv python3; do
    sudo -n test -x "/opt/pulse-tart/bin/$tool" || state=failed
  done
  sudo -n test -f /opt/pulse-tart-image/bootstrap-guest.sh || state=failed
  sudo -n test -f /opt/pulse-tart-image/prepare-template.sh || state=failed
elif sudo -n test -d /var/db/pulse-tart-base/setup; then state=preparing
fi
bytes=$(sudo -n /usr/bin/stat -f %z /var/log/pulse-tart-base.log 2>/dev/null || echo 0)
printf 'PULSE_BASE_OBSERVATION %s %s %s\n' "$boot" "$state" "$bytes"
`

const shutdownReadyMacBase = `set -eu
sudo -n test -f /var/db/pulse-tart-base/ready
sudo -n test ! -f /var/db/pulse-tart-base/failed
sync
sudo -n /sbin/shutdown -h now
`

type basePreparation struct {
	uid, boot, stage, cause string
	reboots                 int
	logBytes                int64
	observed                time.Time
	done                    bool
	err                     error
}

func parseBaseObservation(data []byte) (boot, state string, size int64, err error) {
	f := strings.Fields(string(data))
	if len(f) != 4 || f[0] != "PULSE_BASE_OBSERVATION" || len(f[1]) != 36 {
		return "", "", 0, errors.New("invalid base observation")
	}
	for i, c := range f[1] {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c == '-' {
				continue
			}
		} else if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			continue
		}
		return "", "", 0, errors.New("invalid base boot identity")
	}
	switch f[2] {
	case "absent", "preparing", "ready", "failed":
	default:
		return "", "", 0, errors.New("invalid base observation state")
	}
	size, err = strconv.ParseInt(f[3], 10, 64)
	if err != nil || size < 0 {
		return "", "", 0, errors.New("invalid base log size")
	}
	return f[1], f[2], size, nil
}

func (s *basePreparation) observe(data []byte, now time.Time) error {
	boot, state, size, err := parseBaseObservation(data)
	if err != nil {
		return err
	}
	if s.boot != "" && s.boot != boot {
		s.reboots++
	}
	s.boot, s.logBytes, s.observed = boot, size, now
	s.stage, s.cause = "base_"+state, ""
	if state == "failed" {
		s.err = errors.New("guest preparation service reported failure; inspect /var/log/pulse-tart-base.log in the builder")
	} else if state != "ready" && s.reboots >= 3 {
		s.stage, s.cause = "base_failed", "guest_reboot_loop"
		s.err = fmt.Errorf("guest rebooted %d times during base preparation; preparation cannot complete while the guest keeps rebooting", s.reboots)
	}
	return nil
}

// ObserveBasePreparation is called only after the provider has checked the
// image record's UID and full spec. SSH observation runs outside reconciliation.
func (b *Client) ObserveBasePreparation(ctx context.Context, name, uid, version string) error {
	if version == "" || strings.Trim(version, "0123456789.") != "" {
		return Err
	}
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	r := b.records[name]
	if b.closed || r == nil || r.UID != uid || r.Spec.OS != "darwin" || r.Reusable {
		return Err
	}
	if r.Delivery == nil || !r.Delivery.ExecutionPossible {
		return nil
	}
	if b.basePreparations == nil {
		b.basePreparations = map[string]*basePreparation{}
	}
	s := b.basePreparations[name]
	if s != nil && s.uid == uid {
		if s.err != nil {
			return fmt.Errorf("base %s: %w", name, s.err)
		}
		if !s.done {
			return nil
		}
	} else {
		s = &basePreparation{uid: uid, stage: "base_observing"}
		b.basePreparations[name] = s
	}
	deadline := r.Created.Add(b.timeout)
	if time.Now().After(deadline) {
		s.stage, s.cause = "base_failed", "preparation_timeout"
		s.err = fmt.Errorf("base preparation exceeded %s since VM creation", b.timeout)
		return fmt.Errorf("base %s: %w", name, s.err)
	}
	s.done = false
	b.wg.Add(1)
	go b.monitorBase(name, r, s, version, deadline)
	return nil
}

func (b *Client) monitorBase(name string, r *record, s *basePreparation, version string, deadline time.Time) {
	defer b.wg.Done()
	defer func() { b.mu.Lock(); s.done = true; b.mu.Unlock() }()
	ctx, cancel := context.WithDeadline(b.ctx, deadline)
	defer cancel()
	for ctx.Err() == nil {
		call, done := context.WithTimeout(ctx, 15*time.Second)
		data, err := b.Exec(call, name, s.uid, []byte("expected='"+version+"'\n"+observeMacBase))
		done()
		b.mu.Lock()
		if b.closed || b.records[name] != r {
			b.mu.Unlock()
			return
		}
		if err == nil {
			err = s.observe(data, time.Now().UTC())
		}
		if err != nil {
			s.cause = deliveryCause(err)
		} else if s.stage == "base_ready" {
			// Save actual guest readiness before requesting shutdown. If SSH
			// drops during shutdown, the next controller still has the marker.
			err = b.recordBaseReady(r)
			if err != nil {
				s.err = err
				s.stage = "base_failed"
			}
			b.mu.Unlock()
			if err == nil {
				call, done := context.WithTimeout(ctx, 15*time.Second)
				_, _ = b.Exec(call, name, s.uid, []byte(shutdownReadyMacBase))
				done()
			}
			return
		}
		failed := s.err != nil
		b.mu.Unlock()
		if failed {
			return
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	b.mu.Lock()
	if !b.closed && b.ctx.Err() == nil {
		s.stage, s.cause = "base_failed", "preparation_timeout"
		s.err = errors.New("base preparation deadline expired before readiness was observed")
	}
	b.mu.Unlock()
}

// Caller holds b.mu and has validated the current record and guest observation.
func (b *Client) recordBaseReady(r *record) error {
	f, err := os.OpenFile(b.artifact(r.Spec.Name, "events"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString("PULSE_BASE_READY\n")
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		b.deliverySucceeded(r)
		if !r.Delivered {
			return errors.New("could not persist observed base readiness")
		}
	}
	return err
}
