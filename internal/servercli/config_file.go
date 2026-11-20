package servercli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	case "config", "timeout", "progress", "help", "version":
		return false
	}
	return true
}

func pathFlag(name string) bool {
	switch name {
	case "state-dir", "vm-home", "runner", "orchard-token-file", "tls-ca", "tls-cert", "tls-key":
		return true
	}
	return false
}

func loadConfiguration(cmd *cobra.Command, o *serverOptions) error {
	explicit := cmd.Flags().Changed("config")
	if o.configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.configPath = filepath.Join(home, ".pulse", "provider.json")
	}
	var err error
	o.configPath, err = absolutePath(o.configPath)
	if err != nil {
		return err
	}
	data, err := readFile(o.configPath, 1<<20)
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
		flag := cmd.Flags().Lookup(name)
		if flag == nil || !persistedFlag(name) {
			return fmt.Errorf("unsupported saved option --%s", name)
		}
		if flag.Changed {
			continue
		}
		if pathFlag(name) && value != "" && !filepath.IsAbs(value) && !strings.HasPrefix(value, "~") {
			value = filepath.Join(filepath.Dir(o.configPath), value)
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			return fmt.Errorf("invalid saved option --%s", name)
		}
	}
	return nil
}

func saveConfiguration(cmd *cobra.Command, path string) error {
	saved := savedConfiguration{Version: 1, Flags: map[string]string{}}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
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
