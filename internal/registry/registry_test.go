package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestPaginationAndManifestFallback(t *testing.T) {
	configJSON := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`
	configHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(configJSON)))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, configHash, len(configJSON))
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
	var pages, gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(200)
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			pages.Add(1)
			if r.URL.Query().Get("last") == "" {
				w.Header().Set("Link", `</v2/team/image/tags/list?n=1&last=1.0.0>; rel="next"`)
				fmt.Fprint(w, `{"name":"team/image","tags":["1.0.0"]}`)
			} else {
				fmt.Fprint(w, `{"name":"team/image","tags":["1.1.0"]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/blobs/"+configHash):
			fmt.Fprint(w, configJSON)
		case strings.Contains(r.URL.Path, "/manifests/"):
			if r.Method == http.MethodHead {
				w.WriteHeader(405)
				return
			}
			gets.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			fmt.Fprint(w, manifest)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	repo, err := name.NewRepository(strings.TrimPrefix(server.URL, "http://")+"/team/image", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	c := Client{}
	tags, err := c.Tags(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || pages.Load() != 2 {
		t.Fatalf("pagination: %v %d", tags, pages.Load())
	}
	digest, err := c.Digest(context.Background(), repo, "1.0.0")
	if err != nil || digest != want || gets.Load() != 1 {
		t.Fatalf("digest=%s, err=%v, GETs=%d", digest, err, gets.Load())
	}
	config, err := c.ConfigDigest(context.Background(), repo, "1.0.0", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || config != configHash {
		t.Fatalf("config=%s, err=%v", config, err)
	}
}

func TestMultiPlatformConfigDigest(t *testing.T) {
	const manifestType = "application/vnd.oci.image.manifest.v1+json"
	const indexType = "application/vnd.oci.image.index.v1+json"
	type document struct{ body, mediaType string }
	documents := map[string]document{}
	var descriptors []string
	configs := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		configJSON := fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`, arch)
		configHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(configJSON)))
		configs[arch] = configHash
		manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, manifestType, configHash, len(configJSON))
		manifestHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
		documents["/v2/team/image/blobs/"+configHash] = document{configJSON, "application/vnd.oci.image.config.v1+json"}
		documents["/v2/team/image/manifests/"+manifestHash] = document{manifest, manifestType}
		descriptors = append(descriptors, fmt.Sprintf(`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":%q}}`, manifestType, manifestHash, len(manifest), arch))
	}
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[%s]}`, indexType, strings.Join(descriptors, ","))
	documents["/v2/team/image/manifests/1.0.0"] = document{index, indexType}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		doc, exists := documents[r.URL.Path]
		if !exists {
			t.Errorf("unexpected request (no layers should be pulled): %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", doc.mediaType)
		w.Header().Set("Docker-Content-Digest", fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(doc.body))))
		w.Header().Set("Content-Length", fmt.Sprint(len(doc.body)))
		fmt.Fprint(w, doc.body)
	}))
	defer server.Close()
	repo, err := name.NewRepository(strings.TrimPrefix(server.URL, "http://")+"/team/image", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	client := Client{}
	for _, arch := range []string{"amd64", "arm64"} {
		got, err := client.ConfigDigest(context.Background(), repo, "1.0.0", v1.Platform{OS: "linux", Architecture: arch})
		if err != nil || got != configs[arch] {
			t.Fatalf("platform %s: config=%s want=%s error=%v", arch, got, configs[arch], err)
		}
	}
	if _, err := client.ConfigDigest(context.Background(), repo, "1.0.0", v1.Platform{OS: "linux", Architecture: "s390x"}); err == nil {
		t.Fatal("missing platform must not silently use the host platform")
	}
	got, err := client.Digest(context.Background(), repo, "1.0.0")
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(index)))
	if err != nil || got != want {
		t.Fatalf("index digest=%s want=%s error=%v", got, want, err)
	}
}

func TestAnonymousBearerExchange(t *testing.T) {
	var tokenRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenRequests.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("unexpected stored credentials")
			}
			fmt.Fprint(w, `{"token":"public-token","expires_in":300}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer public-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:team/image:pull"`, server.URL))
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/v2/" {
			return
		}
		fmt.Fprint(w, `{"name":"team/image","tags":["1.0.0"]}`)
	}))
	defer server.Close()
	repo, _ := name.NewRepository(strings.TrimPrefix(server.URL, "http://")+"/team/image", name.Insecure)
	tags, err := (Client{}).Tags(context.Background(), repo)
	if err != nil || len(tags) != 1 || tokenRequests.Load() == 0 {
		t.Fatalf("tags=%v err=%v tokens=%d", tags, err, tokenRequests.Load())
	}
}
