package servercli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type savedConfiguration struct {
	Version int               `json:"version"`
	Flags   map[string]string `json:"flags"`
}

func persistedFlag(name string) bool {
	switch name {
	case "config", "timeout", "progress", "help", "version", "json", "reconnect-attempts":
		return false
	}
	return true
}

func pathFlag(name string) bool {
	switch name {
	case "log-directory", "state-dir", "vm-home", "runner", "tls-ca", "tls-cert", "tls-key", "shared-key-file":
		return true
	}
	return false
}

func loadConfiguration(cmd *cobra.Command, o *serverOptions) error {
	explicit := cmd.Flags().Changed("config")
	defaultPath := o.configPath == ""
	var legacyPath string
	if o.configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.configPath = filepath.Join(home, ".pulse", "vz", "provider.json")
		legacyPath = filepath.Join(home, ".pulse", "provider.json")
	}
	var err error
	o.configPath, err = absolutePath(o.configPath)
	if err != nil {
		return err
	}
	loadedPath := o.configPath
	data, err := readFile(loadedPath, 1<<20)
	if defaultPath && os.IsNotExist(err) {
		loadedPath = legacyPath
		data, err = readFile(legacyPath, 1<<20)
		if err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: using legacy provider configuration %s because %s does not exist; successful setup/upgrade will save configuration to the new location.\n", legacyPath, o.configPath)
		}
	}
	if os.IsNotExist(err) && (!explicit || cmd.Name() == "setup") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	var saved savedConfiguration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&saved) != nil || saved.Version != 1 || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("configuration must contain a single version 1 flags object")
	}
	for name, value := range saved.Flags {
		// Older VZ configurations persisted unused defaults for every backend.
		// Accept those defaults while dropping them on the next save.
		if obsoleteDefault(name, value) {
			continue
		}
		// Upgrade the old saved default cap alongside removal of its finite
		// attempt budget. Explicit CLI caps and other saved caps still win.
		if name == "reconnect-max-delay" && value == "30s" && !cmd.Flags().Changed(name) && saved.Flags["reconnect-attempts"] != "" {
			continue
		}
		flag := cmd.LocalNonPersistentFlags().Lookup(name)
		if flag == nil || !persistedFlag(name) {
			return fmt.Errorf("unsupported saved option --%s", name)
		}
		if flag.Changed {
			continue
		}
		if pathFlag(name) && value != "" && !filepath.IsAbs(value) && !strings.HasPrefix(value, "~") {
			value = filepath.Join(filepath.Dir(loadedPath), value)
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			return fmt.Errorf("invalid saved option --%s", name)
		}
	}
	return nil
}

func obsoleteDefault(name, value string) bool {
	switch name {
	case "reconnect-attempts":
		_, err := strconv.Atoi(value)
		return err == nil
	case "tart-binary":
		return value == "tart"
	case "orchard-url", "orchard-worker", "orchard-username", "orchard-token-file":
		return value == ""
	case "orchard-allow-http":
		return value == "false"
	}
	return false
}

func saveConfiguration(cmd *cobra.Command, path string) error {
	saved := savedConfiguration{Version: 1, Flags: map[string]string{}}
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if !persistedFlag(f.Name) || f.Name == "total-capacity" && !f.Changed {
			return
		}
		saved.Flags[f.Name] = f.Value.String()
	})
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".provider-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Configuration saved:", path)
	return nil
}
