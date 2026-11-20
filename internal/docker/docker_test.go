package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUnixSocketInspectByImmutableID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets required")
	}
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodGet {
			t.Error("non-GET request")
		}
		switch r.URL.Path {
		case "/containers/json":
			if r.URL.Query().Get("all") != "false" {
				t.Error("stopped containers requested")
			}
			fmt.Fprint(w, `[{"Id":"container","Names":["/mail"],"Image":"stalwartlabs/stalwart:1.2.3","ImageID":"sha256:immutable"}]`)
		case "/images/sha256:immutable/json":
			fmt.Fprint(w, `{"Id":"sha256:immutable","Os":"linux","Architecture":"arm64","Variant":"v8"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	got, err := New("unix://" + socket).Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "mail" || got[0].Tag != "1.2.3" || got[0].Repo != "index.docker.io/stalwartlabs/stalwart" || got[0].Platform.Architecture != "arm64" {
		t.Fatalf("%+v", got)
	}
	if len(paths) != 2 {
		t.Fatalf("unexpected requests: %v", paths)
	}
}
func TestUnavailableSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	_, err := New(socket).Running(context.Background())
	if err == nil {
		t.Fatal("missing socket not reported")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("client created socket")
	}
}
