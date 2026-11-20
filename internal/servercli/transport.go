package servercli

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/colony-2/pulse-provider-api/plugin"
	"github.com/spf13/pflag"
)

type transportOptions struct {
	address, name, serverName, ca, cert, key string
	concurrency, messageBytes                int
	startup, shutdown                        time.Duration
	reconnect                                plugin.RestartPolicy
}

func (o *transportOptions) flags(f *pflag.FlagSet) {
	f.StringVarP(&o.address, "collector", "a", "", "Outbound collector address (host:port; required in outbound mode)")
	f.StringVarP(&o.name, "name", "n", "", "Certificate-authorized outbound provider identity (default: owner)")
	f.StringVar(&o.serverName, "tls-server-name", "", "Verified collector certificate name (default: collector host)")
	f.StringVar(&o.ca, "tls-ca", "", "Outbound collector CA certificate PEM file")
	f.StringVar(&o.cert, "tls-cert", "", "Outbound provider client certificate PEM file")
	f.StringVar(&o.key, "tls-key", "", "Outbound provider client private key PEM file")
	f.IntVar(&o.concurrency, "rpc-concurrency", 4, "Maximum concurrent plugin business RPCs")
	f.IntVar(&o.messageBytes, "max-message-bytes", plugin.DefaultMessageBytes, "Plugin message size limit in bytes")
	f.DurationVar(&o.startup, "startup-timeout", 30*time.Second, "Plugin connection and factory startup timeout")
	f.DurationVar(&o.shutdown, "shutdown-timeout", 5*time.Second, "Plugin cleanup timeout")
	f.IntVar(&o.reconnect.MaxAttempts, "reconnect-attempts", 5, "Outbound transient connection retries (0 disables; never replays submissions)")
	f.DurationVar(&o.reconnect.InitialDelay, "reconnect-initial-delay", time.Second, "Initial outbound reconnect delay")
	f.DurationVar(&o.reconnect.MaxDelay, "reconnect-max-delay", 30*time.Second, "Maximum outbound reconnect delay")
	f.DurationVar(&o.reconnect.HealthyResetInterval, "reconnect-reset-after", time.Minute, "Healthy connection duration that resets the reconnect budget (0 disables)")
	o.reconnect.Multiplier = 2
	o.reconnect.JitterFraction = 0.2
}

func (o *transportOptions) validate(mode, owner string) error {
	if o.concurrency < 1 || o.concurrency > 65536 {
		return fmt.Errorf("--rpc-concurrency must be between 1 and 65536")
	}
	if o.messageBytes < 1024 || o.messageBytes > plugin.MaxMessageBytes {
		return fmt.Errorf("--max-message-bytes must be between 1024 and %d", plugin.MaxMessageBytes)
	}
	if o.startup <= 0 || o.shutdown <= 0 {
		return fmt.Errorf("startup and shutdown timeouts must be positive")
	}
	if o.reconnect.MaxAttempts < 0 || o.reconnect.InitialDelay <= 0 || o.reconnect.MaxDelay < o.reconnect.InitialDelay || o.reconnect.HealthyResetInterval < 0 {
		return fmt.Errorf("invalid reconnect attempts or delay bounds")
	}
	if mode == "inbound" {
		if o.address != "" || o.name != "" || o.ca != "" || o.cert != "" || o.key != "" || o.serverName != "" {
			return fmt.Errorf("--collector, --name, and --tls-* options require outbound mode")
		}
		return nil
	}
	host, port, err := net.SplitHostPort(o.address)
	n, parseErr := strconv.Atoi(port)
	if err != nil || host == "" || parseErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("outbound mode requires --collector=host:port")
	}
	if o.ca == "" || o.cert == "" || o.key == "" {
		return fmt.Errorf("outbound mode requires --tls-ca, --tls-cert, and --tls-key")
	}
	if o.name == "" {
		o.name = owner
	}
	if len(o.name) > 256 || !utf8.ValidString(o.name) || strings.IndexFunc(o.name, unicode.IsControl) >= 0 {
		return fmt.Errorf("--name must be a valid provider identity (at most 256 bytes, no control characters)")
	}
	if o.serverName == "" {
		o.serverName = host
	}
	for _, value := range []*string{&o.ca, &o.cert, &o.key} {
		if *value, err = absolutePath(*value); err != nil {
			return err
		}
	}
	return nil
}

func (o transportOptions) serveOptions(version string) plugin.ServeOptions {
	return plugin.ServeOptions{Implementation: "pulse-tart", Version: version, Concurrency: o.concurrency, MaxMessageBytes: o.messageBytes, FactoryTimeout: o.startup, ShutdownTimeout: o.shutdown}
}

func (o transportOptions) tlsConfig() (*tls.Config, error) {
	ca, err := readFile(o.ca, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read --tls-ca: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("--tls-ca contains no CA certificates")
	}
	cert, err := readFile(o.cert, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read --tls-cert: %w", err)
	}
	key, err := readFile(o.key, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read --tls-key: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("invalid client certificate/key pair: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{plugin.ReverseALPN}, RootCAs: roots, ServerName: o.serverName, Certificates: []tls.Certificate{pair}}, nil
}

func readFile(name string, limit int64) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file must be regular")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}
