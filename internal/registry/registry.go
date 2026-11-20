// Package registry uses the standard OCI/Docker Distribution API, not vendor APIs.
package registry

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Client struct{ Transport remote.Option }

func (c Client) options(ctx context.Context) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuth(authn.Anonymous), remote.WithUserAgent("tagwatch")}
	if c.Transport != nil {
		opts = append(opts, c.Transport)
	}
	return opts
}

func (c Client) Tags(ctx context.Context, repo name.Repository) ([]string, error) {
	// remote.List follows paginated tag lists and performs anonymous bearer-token exchange.
	return remote.List(repo, c.options(ctx)...)
}

func (c Client) Digest(ctx context.Context, repo name.Repository, tag string) (string, error) {
	ref := repo.Tag(tag)
	desc, err := remote.Head(ref, c.options(ctx)...)
	if err == nil {
		return desc.Digest.String(), nil
	}
	// Some registries do not implement HEAD correctly. GET only downloads a manifest,
	// never filesystem layers.
	got, getErr := remote.Get(ref, c.options(ctx)...)
	if getErr != nil {
		return "", fmt.Errorf("manifest %s: %w", ref.Name(), getErr)
	}
	return got.Digest.String(), nil
}

// ConfigDigest resolves the platform manifest and returns its config digest.
// Docker's image ID is a config digest, NOT an index or manifest digest.
func (c Client) ConfigDigest(ctx context.Context, repo name.Repository, tag string, platform v1.Platform) (string, error) {
	opts := append(c.options(ctx), remote.WithPlatform(platform))
	image, err := remote.Image(repo.Tag(tag), opts...)
	if err != nil {
		return "", err
	}
	hash, err := image.ConfigName()
	if err != nil {
		return "", err
	}
	return hash.String(), nil
}
