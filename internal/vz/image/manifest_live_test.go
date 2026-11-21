package image

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Fetch only pinned manifests: no VM startup, disk download, or pool mutation.
func TestLiveCatalogManifests(t *testing.T) {
	if os.Getenv("PULSE_TEST_VZ_REGISTRY") != "1" {
		t.Skip("set PULSE_TEST_VZ_REGISTRY=1 to check registry manifest compatibility")
	}
	data, err := os.ReadFile("../../../bases.json")
	if err != nil {
		t.Fatal(err)
	}
	var bases []struct{ Name, Image string }
	if err = json.Unmarshal(data, &bases); err != nil {
		t.Fatal(err)
	}
	for _, base := range bases {
		t.Run(base.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ref, err := name.NewDigest(base.Image, name.StrictValidation)
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
			if err != nil {
				t.Fatal(err)
			}
			if digestBytes(descriptor.Manifest).String() != ref.DigestStr() {
				t.Fatal("manifest digest mismatch")
			}
			plan, err := parseManifest(descriptor.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("registry manifest accepted: %d disk layers, %d uncompressed bytes", len(plan.disk), plan.size)
		})
	}
}
