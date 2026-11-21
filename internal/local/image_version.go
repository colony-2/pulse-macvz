package local

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ImageVersionFile travels with the disk when cloning, including suspended VMs.
const ImageVersionFile = "pulse.c2j.json"

type ImageVersion struct {
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
}

var versionToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,199}$`)
var revisionToken = regexp.MustCompile(`^[a-f0-9]{7,40}$`)

func parseImageVersion(line string) *ImageVersion {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "PULSE_C2J" || (f[1] != "-" && !revisionToken.MatchString(f[1])) || (f[2] != "-" && !versionToken.MatchString(f[2])) {
		return nil
	}
	v := &ImageVersion{Version: f[2], Revision: f[1]}
	if v.Version == "-" {
		v.Version = ""
	}
	if v.Revision == "-" {
		v.Revision = ""
	}
	if v.Revision == "" && strings.HasPrefix(v.Version, "unstable-") {
		if revision := strings.TrimPrefix(v.Version, "unstable-"); revisionToken.MatchString(revision) {
			v.Revision = revision
		}
	}
	if v.Version == "" && v.Revision == "" {
		return nil
	}
	return v
}

// ReadImageVersion never starts a guest or infers a version from configuration.
func ReadImageVersion(dir string) (*ImageVersion, error) {
	f, err := os.Open(filepath.Join(dir, ImageVersionFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var v ImageVersion
	if err = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&v); err != nil {
		return nil, err
	}
	version, revision := v.Version, v.Revision
	if version == "" {
		version = "-"
	}
	if revision == "" {
		revision = "-"
	}
	if parseImageVersion("PULSE_C2J "+revision+" "+version) == nil {
		return nil, Err
	}
	return &v, nil
}

func (b *Client) recordImageVersion(name string, v *ImageVersion) error {
	// A partial observation must not discard a known source revision.
	if old, err := ReadImageVersion(b.path(name)); err == nil && old.Version == v.Version && (v.Revision == "" || (len(old.Revision) > len(v.Revision) && strings.HasPrefix(old.Revision, v.Revision))) {
		v.Revision = old.Revision
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicPrivateFile(filepath.Join(b.path(name), ImageVersionFile), data)
}

// Executed before startup, and by the base observer after installation. Older
// images still supply their binary version even without the revision sidecar.
const observeImageVersion = `if test -x /opt/pulse-tart/bin/c2j; then
  pulse_c2j_version=$(/opt/pulse-tart/bin/c2j version 2>/dev/null | head -n 1)
  pulse_c2j_version=${pulse_c2j_version#c2j version }
  pulse_c2j_revision=$(cat /opt/pulse-tart-image/c2j-revision 2>/dev/null) || pulse_c2j_revision=-
  printf 'PULSE_C2J %s %s\n' "$pulse_c2j_revision" "$pulse_c2j_version"
fi
`
