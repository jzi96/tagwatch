// Package docker provides opt-in GET-only Docker Engine inspection over a Unix socket.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type Container struct {
	Name     string
	Image    string
	Repo     string
	Tag      string
	ImageID  string
	Platform v1.Platform
}

type Client struct{ http *http.Client }

func New(socket string) *Client {
	socket = strings.TrimPrefix(socket, "unix://")
	return &Client{http: &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker %s: HTTP %d", path, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out)
}

func (c *Client) Running(ctx context.Context) ([]Container, error) {
	// Unversioned read endpoints work across Docker API versions.
	var listed []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		ImageID string
	}
	if err := c.get(ctx, "/containers/json?all=false", &listed); err != nil {
		return nil, err
	}
	var result []Container
	for _, item := range listed {
		ref, err := name.ParseReference(item.Image, name.WeakValidation)
		if err != nil {
			continue
		} // Locally built images without repository references cannot be matched.
		var image struct {
			ID                        string `json:"Id"`
			Architecture, Os, Variant string
		}
		// Inspect by immutable ID, never by tag (which may now point to a newer local image).
		if err := c.get(ctx, "/images/"+url.PathEscape(item.ImageID)+"/json", &image); err != nil {
			return nil, err
		}
		n := item.ID
		if len(item.Names) > 0 {
			n = strings.TrimPrefix(item.Names[0], "/")
		}
		tag := ""
		if t, ok := ref.(name.Tag); ok {
			tag = t.TagStr()
		}
		result = append(result, Container{
			Name: n, Image: item.Image, Repo: ref.Context().Name(), Tag: tag, ImageID: image.ID,
			Platform: v1.Platform{OS: image.Os, Architecture: image.Architecture, Variant: image.Variant},
		})
	}
	return result, nil
}
