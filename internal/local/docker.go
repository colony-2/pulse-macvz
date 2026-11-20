package local

import (
	"context"
	"github.com/colony-2/pulse-tart/internal/guestconn"
	"net"
	"strings"
)

func (b *Client) Exec(ctx context.Context, name, uid string, script []byte) ([]byte, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid || b.records[name] == nil {
		b.mu.Unlock()
		return nil, failure("Exec Get or UID mismatch", e)
	}
	key := append([]byte{}, b.records[name].HostKey...)
	b.mu.Unlock()
	data, e := b.hypervisor().IP(ctx, name)
	if e != nil {
		return nil, failure("Exec IP command", e)
	}
	ip := strings.TrimSpace(string(data))
	if net.ParseIP(ip) == nil {
		return nil, failure("Exec IP is invalid", nil)
	}
	out, e := guestconn.Exec(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: b.cfg.SSHTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "22"))
	}, v.Username, v.Password, key, script)
	if e != nil {
		return nil, failure("Exec management SSH", e)
	}
	return out, nil
}
func (b *Client) Stop(ctx context.Context, name, uid string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid {
		return failure("Stop Get or UID mismatch", e)
	}
	if v.Stopped() {
		return nil
	}
	e = b.hypervisor().Stop(ctx, name)
	return e
}
