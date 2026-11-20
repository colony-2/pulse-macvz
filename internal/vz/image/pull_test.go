package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func digestBytes(data []byte) v1.Hash {
	sum := sha256.Sum256(data)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func TestPullVerifiedImage(t *testing.T) {
	for _, failure := range []string{"", "blob digest", "decoded digest", "decoded size", "truncated stream", "unknown layer", "manifest digest", "cancelled", "existing destination"} {
		t.Run(failure, func(t *testing.T) {
			raw := append(make([]byte, 128*1024), []byte("disk tail")...)
			encoded, err := CompressAppleLZ4(raw)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "truncated stream" {
				encoded = encoded[:len(encoded)-4]
			}
			blobs := map[string][]byte{}
			add := func(kind string, data []byte) v1.Descriptor {
				hash := digestBytes(data)
				blobs[hash.String()] = data
				return v1.Descriptor{MediaType: types.MediaType(kind), Digest: hash, Size: int64(len(data))}
			}
			cfg := []byte(`{"os":"linux","arch":"arm64"}`)
			config := add(configType, cfg)
			nvram := add(nvramType, []byte("firmware"))
			disk := add(diskType, encoded)
			disk.Annotations = map[string]string{sizeAnnotation: fmt.Sprint(len(raw)), hashAnnotation: digestBytes(raw).String()}
			switch failure {
			case "blob digest":
				blobs[config.Digest.String()] = bytes.Repeat([]byte("x"), len(cfg))
			case "decoded digest":
				disk.Annotations[hashAnnotation] = digestBytes([]byte("wrong")).String()
			case "decoded size":
				disk.Annotations[sizeAnnotation] = fmt.Sprint(len(raw) - 1)
			case "unknown layer":
				disk.MediaType = "application/unknown"
			}
			manifest, err := json.Marshal(v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: add(string(types.OCIConfigJSON), []byte(`{}`)), Layers: []v1.Descriptor{config, nvram, disk}})
			if err != nil {
				t.Fatal(err)
			}
			pin := digestBytes(manifest).String()
			if failure == "manifest digest" {
				pin = digestBytes([]byte("wrong manifest")).String()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v2/":
					w.WriteHeader(http.StatusOK)
				case strings.HasPrefix(r.URL.Path, "/v2/base/manifests/"):
					w.Header().Set("Content-Type", string(types.OCIManifestSchema1))
					w.Write(manifest)
				case strings.HasPrefix(r.URL.Path, "/v2/base/blobs/"):
					data, ok := blobs[strings.TrimPrefix(r.URL.Path, "/v2/base/blobs/")]
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					w.Write(data)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			dest := filepath.Join(t.TempDir(), "image")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			if failure == "existing destination" {
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, "sentinel"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = pull(ctx, strings.TrimPrefix(server.URL, "http://")+"/base@"+pin, dest, []remote.Option{remote.WithTransport(server.Client().Transport)})
			if failure != "" {
				if err == nil {
					t.Fatal("accepted invalid pull")
				}
				if failure == "existing destination" {
					got, err := os.ReadFile(filepath.Join(dest, "sentinel"))
					if err != nil || string(got) != "keep" {
						t.Fatalf("existing destination changed: %s, %v", got, err)
					}
				} else if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatalf("partial image left behind: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for filename, want := range map[string][]byte{"config.json": cfg, "nvram.bin": []byte("firmware"), "disk.img": raw} {
				got, err := os.ReadFile(filepath.Join(dest, filename))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s mismatch: %v", filename, err)
				}
				info, err := os.Stat(filepath.Join(dest, filename))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("%s permissions: %v", filename, err)
				}
			}
		})
	}
}

func TestPullRejectsUnpinnedReference(t *testing.T) {
	if err := Pull(context.Background(), "example.com/base:latest", filepath.Join(t.TempDir(), "image")); err == nil {
		t.Fatal("accepted mutable tag")
	}
}

func TestAppleLZ4HistoryAcrossShortBlocks(t *testing.T) {
	var stream bytes.Buffer
	for _, block := range []string{"abcdefgh", "ijklmnop"} {
		if err := writeAppleLZ4Uncompressed(&stream, []byte(block)); err != nil {
			t.Fatal(err)
		}
	}
	// Eight-byte match at offset 16 references the first, not the previous,
	// block. The final five literals satisfy the raw LZ4 block format.
	compressed := []byte{0x04, 0x10, 0x00, 0x50, '1', '2', '3', '4', '5'}
	if err := writeAppleLZ4Compressed(&stream, make([]byte, 13), compressed); err != nil {
		t.Fatal(err)
	}
	stream.Write(appleLZ4MagicEndOfStream[:])
	got, err := DecompressAppleLZ4(stream.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefghijklmnopabcdefgh12345" {
		t.Fatalf("decoded %q", got)
	}
}
