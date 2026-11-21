package servercli

import (
	"bytes"
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
	address, name, serverName, ca, cert, key     string
	auth, transport, sharedKeyFile, sharedKeyEnv string
	concurrency, messageBytes                    int
	startup, shutdown                            time.Duration
	reconnect                                    plugin.RestartPolicy
}

func (o *transportOptions) flags(f *pflag.FlagSet) {
	f.StringVarP(&o.address, "collector", "a", "", "Outbound collector address (host:port; required in outbound mode)")
	f.StringVarP(&o.name, "name", "n", "", "Authorized outbound provider identity (default: owner)")
	f.StringVar(&o.auth, "reverse-auth", "mtls", "Reverse authentication: mtls or mutual-key")
	f.StringVar(&o.transport, "reverse-transport", "tls", "Reverse transport: tls or http (plaintext; mutual-key only)")
	f.StringVar(&o.sharedKeyFile, "shared-key-file", "", "Shared-key file (one line; mutually exclusive with --shared-key-env)")
	f.StringVar(&o.sharedKeyEnv, "shared-key-env", "", "Name of the environment variable containing the exact shared key")
	f.StringVar(&o.serverName, "tls-server-name", "", "Verified collector certificate name (default: collector host)")
	f.StringVar(&o.ca, "tls-ca", "", "Collector CA PEM file (default: system roots)")
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
	if o.auth == "" {
		o.auth = "mtls"
	}
	if o.transport == "" {
		o.transport = "tls"
	}
	if _, _, err := plugin.ParseReverseModes(o.auth, o.transport); err != nil {
		return fmt.Errorf("--reverse-auth must be mtls or mutual-key; --reverse-transport must be tls or http (http requires mutual-key)")
	}
	if mode == "inbound" {
		if o.configured() {
			return fmt.Errorf("reverse connection, authentication, and TLS options require outbound mode")
		}
		return nil
	}
	host, port, err := net.SplitHostPort(o.address)
	n, parseErr := strconv.Atoi(port)
	if err != nil || host == "" || parseErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("outbound mode requires --collector=host:port")
	}
	if o.auth == "mtls" {
		if o.cert == "" || o.key == "" {
			return fmt.Errorf("mtls authentication requires --tls-cert and --tls-key")
		}
		if o.sharedKeyFile != "" || o.sharedKeyEnv != "" {
			return fmt.Errorf("shared-key options require --reverse-auth=mutual-key")
		}
	} else {
		if (o.sharedKeyFile == "") == (o.sharedKeyEnv == "") {
			return fmt.Errorf("mutual-key authentication requires exactly one of --shared-key-file or --shared-key-env")
		}
		if o.cert != "" || o.key != "" {
			return fmt.Errorf("--tls-cert and --tls-key require --reverse-auth=mtls")
		}
		if strings.ContainsAny(o.sharedKeyEnv, "=\x00") {
			return fmt.Errorf("invalid --shared-key-env name")
		}
	}
	if o.transport == "http" && (o.ca != "" || o.serverName != "") {
		return fmt.Errorf("--tls-* options cannot be used with --reverse-transport=http")
	}
	if o.name == "" {
		o.name = owner
	}
	if len(o.name) > 256 || !utf8.ValidString(o.name) || strings.IndexFunc(o.name, unicode.IsControl) >= 0 {
		return fmt.Errorf("--name must be a valid provider identity (at most 256 bytes, no control characters)")
	}
	if o.serverName == "" && o.transport == "tls" {
		o.serverName = host
	}
	for _, value := range []*string{&o.ca, &o.cert, &o.key, &o.sharedKeyFile} {
		if *value, err = absolutePath(*value); err != nil {
			return err
		}
	}
	return nil
}

func (o transportOptions) serveOptions(version string) plugin.ServeOptions {
	return plugin.ServeOptions{Implementation: "pulse-macvz", Version: version, Concurrency: o.concurrency, MaxMessageBytes: o.messageBytes, FactoryTimeout: o.startup, ShutdownTimeout: o.shutdown}
}

func (o transportOptions) tlsConfig() (*tls.Config, error) {
	if o.transport == "http" {
		return nil, nil
	}
	var roots *x509.CertPool
	if o.ca != "" {
		ca, err := readFile(o.ca, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("read --tls-ca: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("--tls-ca contains no CA certificates")
		}
	} else {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA roots: %w", err)
		}
	}
	if o.auth == "mutual-key" {
		return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{plugin.ReverseKeyALPN}, RootCAs: roots, ServerName: o.serverName}, nil
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

func (o transportOptions) configured() bool {
	return o.address != "" || o.name != "" || o.ca != "" || o.cert != "" || o.key != "" || o.serverName != "" || o.sharedKeyFile != "" || o.sharedKeyEnv != "" || (o.auth != "" && o.auth != "mtls") || (o.transport != "" && o.transport != "tls")
}

func (o transportOptions) mutualKey() ([]byte, error) {
	if o.auth != "mutual-key" {
		return nil, nil
	}
	source := plugin.MutualKeySource{EnvName: o.sharedKeyEnv, LookupEnv: os.LookupEnv}
	if o.sharedKeyFile != "" {
		data, err := readFile(o.sharedKeyFile, plugin.MaxMutualKeyBytes+2)
		if err != nil {
			return nil, fmt.Errorf("cannot read --shared-key-file")
		}
		// Strip only one conventional line ending; spaces and all other bytes
		// remain significant. Multiple lines are rejected by the SDK validator.
		if bytes.HasSuffix(data, []byte("\n")) {
			data = bytes.TrimSuffix(data, []byte("\n"))
			data = bytes.TrimSuffix(data, []byte("\r"))
		}
		source = plugin.MutualKeySource{EnvName: "file", LookupEnv: func(string) (string, bool) { return string(data), true }}
	}
	key, err := plugin.LoadMutualKey(source)
	if err != nil {
		return nil, fmt.Errorf("shared key must be 1–%d bytes with no embedded line endings; check the configured file or environment variable", plugin.MaxMutualKeyBytes)
	}
	return key, nil
}
