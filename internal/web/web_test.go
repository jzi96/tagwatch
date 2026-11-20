package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"tagwatch/internal/config"
	"tagwatch/internal/monitor"
)

type registryStub struct{}

func (registryStub) Tags(context.Context, name.Repository) ([]string, error) {
	return []string{"1.0.0"}, nil
}
func (registryStub) Digest(context.Context, name.Repository, string) (string, error) {
	return "<script>alert(1)</script>", nil
}
func (registryStub) ConfigDigest(context.Context, name.Repository, string, v1.Platform) (string, error) {
	return "", nil
}

func TestPageAndHealth(t *testing.T) {
	cfg := config.Config{Interval: "1h", Timeout: "5s", HistoryDays: 30, StateFile: filepath.Join(t.TempDir(), "state.json"), Repositories: []config.Repository{{Image: "alpine"}}}
	m, err := monitor.New(cfg, registryStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(m)
	for path, want := range map[string]int{"/": 200, "/healthz": 200, "/missing": 404} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if path == "/" && (!strings.Contains(w.Body.String(), "Waiting for the first successful check") || w.Header().Get("Content-Security-Policy") == "") {
			t.Fatal("missing baseline message/security header")
		}
	}
	// Template must escape untrusted registry/runtime values, even inside details.
	var b strings.Builder
	err = view.Execute(&b, monitor.Report{Repositories: []monitor.RepositoryReport{{Image: "<script>alert(1)</script>", History: []monitor.Event{{Tag: "1.0.0", Digest: "<script>evil</script>"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "<script>") || !strings.Contains(b.String(), "&lt;script&gt;") {
		t.Fatal("unescaped dynamic data")
	}
}
