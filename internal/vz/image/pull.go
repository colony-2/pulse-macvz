package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// These identifiers belong to the upstream Tart OCI format, not our provider
// name. VZ consumes existing Cirrus images without the Tart runtime.
const (
	configType         = "application/vnd.cirruslabs.tart.config.v1"
	nvramType          = "application/vnd.cirruslabs.tart.nvram.v1"
	diskType           = "application/vnd.cirruslabs.tart.disk.v2"
	sizeAnnotation     = "org.cirruslabs.tart.uncompressed-size"
	hashAnnotation     = "org.cirruslabs.tart.uncompressed-content-digest"
	diskSizeAnnotation = "org.cirruslabs.tart.uncompressed-disk-size"
	maxDiskSize        = int64(4 << 40)
)

type diskLayer struct {
	descriptor v1.Descriptor
	size       int64
	digest     string
}
type plan struct {
	config, nvram v1.Descriptor
	disk          []diskLayer
	size          int64
}

func parseManifest(data []byte) (plan, error) {
	var out plan
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return out, err
	}
	if manifest.SchemaVersion != 2 {
		return out, errors.New("image manifest must have schema version 2")
	}
	for _, layer := range manifest.Layers {
		if layer.Digest.Algorithm != "sha256" || len(layer.Digest.Hex) != 64 || layer.Size <= 0 || layer.Size > maxDiskSize {
			return out, errors.New("invalid layer digest or size")
		}
		if _, err := hex.DecodeString(layer.Digest.Hex); err != nil {
			return out, err
		}
		switch string(layer.MediaType) {
		case configType:
			if out.config.Size != 0 || layer.Size > 4<<20 {
				return out, errors.New("duplicate or oversized VM configuration")
			}
			out.config = layer
		case nvramType:
			if out.nvram.Size != 0 || layer.Size > 64<<20 {
				return out, errors.New("duplicate or oversized NVRAM")
			}
			out.nvram = layer
		case diskType:
			size, err := strconv.ParseInt(layer.Annotations[sizeAnnotation], 10, 64)
			if err != nil || size <= 0 || size > maxDiskSize-out.size {
				return out, errors.New("invalid disk chunk size")
			}
			digest := layer.Annotations[hashAnnotation]
			parsed, err := v1.NewHash(digest)
			if err != nil || parsed.Algorithm != "sha256" || len(parsed.Hex) != 64 {
				return out, errors.New("invalid uncompressed disk digest")
			}
			out.disk = append(out.disk, diskLayer{layer, size, digest})
			out.size += size
		default:
			return out, fmt.Errorf("unsupported image layer type %q", layer.MediaType)
		}
	}
	if out.config.Size == 0 || out.nvram.Size == 0 || len(out.disk) == 0 {
		return out, errors.New("image requires configuration, NVRAM and disk chunks")
	}
	if declared := manifest.Annotations[diskSizeAnnotation]; declared != "" {
		size, err := strconv.ParseInt(declared, 10, 64)
		if err != nil || size != out.size {
			return out, errors.New("image disk size does not match chunk sizes")
		}
	}
	return out, nil
}

// Pull writes a verified raw VM image into an absent destination directory.
// Publication into the shared cache is the caller's responsibility.
func Pull(ctx context.Context, reference, destination string) error {
	return pull(ctx, reference, destination, nil)
}

func pull(ctx context.Context, reference, destination string, options []remote.Option) error {
	backend.ReportProgress(ctx, backend.TransferProgress{Stage: "manifest"})
	ref, err := name.NewDigest(reference, name.StrictValidation)
	if err != nil {
		return fmt.Errorf("digest-pinned base required: %w", err)
	}
	if len(ref.DigestStr()) != 71 || ref.DigestStr()[:7] != "sha256:" {
		return errors.New("base image must use a SHA256 digest")
	}
	options = append(append([]remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}, options...), remote.WithContext(ctx))
	descriptor, err := remote.Get(ref, options...)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(descriptor.Manifest)
	if "sha256:"+hex.EncodeToString(sum[:]) != ref.DigestStr() {
		return errors.New("manifest digest mismatch")
	}
	plan, err := parseManifest(descriptor.Manifest)
	if err != nil {
		return err
	}
	transfer := &transferCounter{ctx: ctx, total: plan.config.Size + plan.nvram.Size}
	for _, layer := range plan.disk {
		transfer.total += layer.descriptor.Size
	}
	transfer.report("downloading")
	if err = os.Mkdir(destination, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(destination)
		}
	}()
	for _, item := range []struct {
		name       string
		descriptor v1.Descriptor
	}{{"config.json", plan.config}, {"nvram.bin", plan.nvram}} {
		f, err := os.OpenFile(filepath.Join(destination, item.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = fetchLayer(ctx, ref, item.descriptor, options, func(r io.Reader) error { _, err := io.Copy(f, r); return err }, transfer)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	disk, err := os.OpenFile(filepath.Join(destination, "disk.img"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer disk.Close()
	if err = disk.Truncate(plan.size); err != nil {
		return err
	}
	for _, layer := range plan.disk {
		if err = ctx.Err(); err != nil {
			return err
		}
		hash := sha256.New()
		output := &limitedOutput{writer: io.MultiWriter(&sparseWriter{file: disk}, hash), remaining: layer.size}
		err = fetchLayer(ctx, ref, layer.descriptor, options, func(r io.Reader) error { return readAppleLZ4(r, output) }, transfer)
		if err != nil {
			return err
		}
		if output.remaining != 0 {
			return errors.New("decoded disk chunk size mismatch")
		}
		if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != layer.digest {
			return errors.New("decoded disk chunk digest mismatch")
		}
	}
	transfer.report("verifying")
	if err = disk.Sync(); err != nil {
		return err
	}
	if err = disk.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func fetchLayer(ctx context.Context, ref name.Digest, desc v1.Descriptor, options []remote.Option, consume func(io.Reader) error, transfer *transferCounter) error {
	layer, err := remote.Layer(ref.Context().Digest(desc.Digest.String()), options...)
	if err != nil {
		return err
	}
	body, err := layer.Compressed()
	if err != nil {
		return err
	}
	defer body.Close()
	hash := sha256.New()
	bounded := &io.LimitedReader{R: body, N: desc.Size + 1}
	counted := &countReader{reader: io.TeeReader(bounded, hash), transfer: transfer}
	if err = consume(counted); err != nil {
		return err
	}
	// Drain through EOF so compressed-blob integrity is always checked, even if
	// the decoder reached its end marker before the transport reached EOF.
	if _, err = io.Copy(io.Discard, counted); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if counted.n != desc.Size {
		return errors.New("image layer size mismatch")
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != desc.Digest.String() {
		return errors.New("image layer digest mismatch")
	}
	return nil
}

type countReader struct {
	reader   io.Reader
	n        int64
	transfer *transferCounter
}

func (r *countReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	if r.transfer != nil {
		r.transfer.current += int64(n)
		if time.Since(r.transfer.last) >= 250*time.Millisecond {
			r.transfer.report("downloading")
		}
	}
	return n, err
}

type transferCounter struct {
	ctx            context.Context
	current, total int64
	last           time.Time
}

func (t *transferCounter) report(stage string) {
	t.last = time.Now()
	backend.ReportProgress(t.ctx, backend.TransferProgress{Stage: stage, Current: t.current, Total: t.total})
}

type limitedOutput struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("disk chunk exceeds declared size")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type sparseWriter struct{ file *os.File }

func (w *sparseWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		size := min(len(p), 64<<10)
		block := p[:size]
		zero := true
		for _, b := range block {
			if b != 0 {
				zero = false
				break
			}
		}
		if zero {
			if _, err := w.file.Seek(int64(size), io.SeekCurrent); err != nil {
				return written, err
			}
		} else {
			n, err := w.file.Write(block)
			if err != nil {
				return written + n, err
			}
			if n != size {
				return written + n, io.ErrShortWrite
			}
		}
		written += size
		p = p[size:]
	}
	return written, nil
}
