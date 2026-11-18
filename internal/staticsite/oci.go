package staticsite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	annotationTitle  = "org.opencontainers.image.title"
	annotationUnpack = "io.deis.oras.content.unpack" // set by `oras push dir/`
	maxManifestBytes = 4 << 20
)

// Pull fetches the artifact named by cfg into dest and returns the manifest
// digest it resolved to.
//
// The artifact is what `oras push` makes: a manifest whose layers are either
// a tar (optionally gzip) archive of the site, unpacked at the root, or single
// files named by their title annotation. Layers are verified against their
// digests while they are read. Credentials come from the Docker config file
// found through DOCKER_CONFIG (a mounted image pull secret); failing that, an
// ECR registry is authenticated through the AWS SDK default credential chain
// (see registryAuth). Pull builds a fresh authenticator; a Server keeps one,
// and with it the ECR token, between pulls.
func Pull(ctx context.Context, cfg *Config, dest string) (digest.Digest, error) {
	return pullWith(ctx, cfg, dest, newRegistryAuth(cfg, nil))
}

// PullLogged is Pull with the registry authentication method (never a
// credential) reported to log.
func PullLogged(ctx context.Context, cfg *Config, dest string, log *slog.Logger) (digest.Digest, error) {
	return pullWith(ctx, cfg, dest, newRegistryAuth(cfg, log))
}

// pullWith pulls with ra. When an ECR registry refuses the cached token (401)
// it drops the token and tries once more with a new one.
func pullWith(ctx context.Context, cfg *Config, dest string, ra *registryAuth) (digest.Digest, error) {
	d, method, err := pullOnce(ctx, cfg, dest, ra)
	if err != nil && ra.refused(err, method) {
		ra.log.Info("registry refused the ecr token, fetching a new one")
		_ = os.RemoveAll(dest)
		if merr := os.MkdirAll(dest, 0o755); merr != nil {
			return "", merr
		}
		d, _, err = pullOnce(ctx, cfg, dest, ra)
	}
	return d, err
}

func pullOnce(ctx context.Context, cfg *Config, dest string, ra *registryAuth) (digest.Digest, string, error) {
	ref, err := registry.ParseReference(cfg.ArtifactRef)
	if err != nil {
		return "", "", fmt.Errorf("artifact ref: %w", err)
	}
	if cfg.ArtifactDigest != "" {
		ref.Reference = cfg.ArtifactDigest
	}
	if ref.Reference == "" {
		return "", "", fmt.Errorf("artifact ref %q has no tag and no digest was given", cfg.ArtifactRef)
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return "", "", err
	}
	repo.PlainHTTP = cfg.PlainHTTP
	client, method, err := ra.client(ctx, ref.Registry)
	if err != nil {
		return "", "", fmt.Errorf("registry auth: %w", err)
	}
	// The method only; never a credential.
	ra.log.Info("registry auth: "+method, "method", method, "registry", ref.Registry)
	repo.Client = client
	d, err := pullArtifact(ctx, cfg, dest, repo, ref)
	return d, method, err
}

func pullArtifact(ctx context.Context, cfg *Config, dest string, repo *remote.Repository, ref registry.Reference) (digest.Digest, error) {
	desc, rc, err := repo.FetchReference(ctx, ref.Reference)
	if err != nil {
		return "", fmt.Errorf("fetch manifest %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if desc.Size > maxManifestBytes {
		return "", fmt.Errorf("manifest is %d bytes, over the %d limit", desc.Size, maxManifestBytes)
	}
	raw, err := content.ReadAll(rc, desc)
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return "", fmt.Errorf("%s is %s, not an image manifest (an index is not a site)", ref, desc.MediaType)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return "", fmt.Errorf("%s has no layers", ref)
	}
	lim := &limits{maxBytes: cfg.MaxBytes, maxFiles: cfg.MaxFiles}
	for _, layer := range m.Layers {
		if layer.Size > cfg.MaxBytes {
			return "", fmt.Errorf("layer %s is %d bytes, over the %d limit", layer.Digest, layer.Size, cfg.MaxBytes)
		}
		if err := pullLayer(ctx, repo, layer, dest, lim); err != nil {
			return "", fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
	}
	return desc.Digest, nil
}

func isTarLayer(l ocispec.Descriptor) bool {
	mt := l.MediaType
	return l.Annotations[annotationUnpack] == "true" ||
		mt == ocispec.MediaTypeImageLayer || mt == ocispec.MediaTypeImageLayerGzip ||
		strings.HasSuffix(mt, "tar+gzip") || strings.HasSuffix(mt, "+tar") || strings.HasSuffix(mt, ".tar")
}

func pullLayer(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor, dest string, lim *limits) error {
	rc, err := repo.Fetch(ctx, layer)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	vr := content.NewVerifyReader(rc, layer)
	if isTarLayer(layer) {
		if err := extractTar(vr, dest, lim); err != nil {
			return err
		}
	} else {
		title := layer.Annotations[annotationTitle]
		if title == "" {
			return fmt.Errorf("a non-archive layer needs a %s annotation to name its file", annotationTitle)
		}
		p, err := relPath(title)
		if err != nil {
			return err
		}
		if err := lim.file(); err != nil {
			return err
		}
		if err := writeLayerFile(dest, p, vr, lim); err != nil {
			return err
		}
	}
	// Drain what a tar reader leaves (padding) so the digest check sees it all.
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return err
	}
	return vr.Verify()
}
