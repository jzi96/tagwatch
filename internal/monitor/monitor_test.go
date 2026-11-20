package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"tagwatch/internal/config"
	"tagwatch/internal/docker"
)

type fakeRegistry struct {
	mu           sync.Mutex
	tags         map[string]string
	fail         string
	configDigest string
	platform     v1.Platform
}

func (f *fakeRegistry) Tags(ctx context.Context, repo name.Repository) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for tag := range f.tags {
		out = append(out, tag)
	}
	return out, nil
}
func (f *fakeRegistry) Digest(ctx context.Context, repo name.Repository, tag string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tag == f.fail {
		return "", errors.New("registry unavailable")
	}
	return f.tags[tag], nil
}
func (f *fakeRegistry) ConfigDigest(ctx context.Context, repo name.Repository, tag string, p v1.Platform) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.platform = p
	return f.configDigest, nil
}

type fakeRuntime struct {
	containers []docker.Container
	err        error
}

func (f fakeRuntime) Running(context.Context) ([]docker.Container, error) { return f.containers, f.err }

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Listen: ":8080", Interval: "1h", Timeout: "10s", StateFile: filepath.Join(t.TempDir(), "state.json"), HistoryDays: 30,
		Repositories: []config.Repository{{Image: "alpine"}},
	}
}
func getReport(m *Monitor) Report {
	var r Report
	m.View(func(got Report) { r = got })
	return r
}

func TestBaselineChangesRestartAndFailure(t *testing.T) {
	cfg := testConfig(t)
	f := &fakeRegistry{tags: map[string]string{"1.0.0": "a", "nightly": "ignored"}}
	m, err := New(cfg, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	r := getReport(m).Repositories[0]
	if !r.Initialized || len(r.History) != 0 || r.TagCount != 1 {
		t.Fatalf("baseline: %+v", r)
	}
	f.tags = map[string]string{"1.0.0": "b", "1.1.0": "c"}
	m.Poll(context.Background())
	r = getReport(m).Repositories[0]
	if len(r.History) != 2 {
		t.Fatalf("changes: %+v", r)
	}
	for _, e := range r.History {
		if e.Tag == "1.0.0" && (e.Kind != "digest changed" || e.Previous != "a" || e.Digest != "b") {
			t.Fatalf("digest event: %+v", e)
		}
	}
	m, err = New(cfg, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	if len(getReport(m).Repositories[0].History) != 2 {
		t.Fatal("history lost or repeated on restart")
	}
	f.tags = map[string]string{"1.0.0": "bad", "1.2.0": "d"}
	f.fail = "1.2.0"
	m.Poll(context.Background())
	r = getReport(m).Repositories[0]
	if r.Error == "" || len(r.History) != 2 {
		t.Fatalf("failure report: %+v", r)
	}
	s, err := LoadState(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if s.Repositories[r.Canonical].Tags["1.0.0"] != "b" {
		t.Fatal("partial failure advanced baseline")
	}
	f.fail = ""
	m.Poll(context.Background())
	if len(getReport(m).Repositories[0].History) != 4 {
		t.Fatal("successful retry did not record changes")
	}
}

func TestRetentionAndReappearance(t *testing.T) {
	now := time.Now().UTC()
	s := RepositoryState{}
	s.Apply(map[string]string{"1.0.0": "a"}, now, 24*time.Hour)
	s.Apply(map[string]string{"1.0.0": "b"}, now.Add(time.Hour), 24*time.Hour)
	s.Apply(map[string]string{"1.0.0": "b"}, now.Add(26*time.Hour), 24*time.Hour)
	if len(s.History) != 0 {
		t.Fatal("expired event retained")
	}
	s.Apply(map[string]string{}, now.Add(27*time.Hour), 24*time.Hour)
	s.Apply(map[string]string{"1.0.0": "b"}, now.Add(28*time.Hour), 24*time.Hour)
	if len(s.History) != 1 || s.History[0].Kind != "new tag" {
		t.Fatal("reappearing tag should be newly observed")
	}
}

func TestOlderSemverTagsAreNotObservedAsNew(t *testing.T) {
	now := time.Now().UTC()
	s := RepositoryState{}
	s.Apply(map[string]string{"2.16.0": "a"}, now, 24*time.Hour)
	s.Apply(map[string]string{
		"2.1.0":  "b",
		"2.16.0": "a",
		"2.17.0": "c",
	}, now.Add(time.Hour), 24*time.Hour)
	if len(s.History) != 1 || s.History[0].Tag != "2.17.0" {
		t.Fatalf("history: %+v", s.History)
	}

	s = RepositoryState{
		Initialized: true,
		Tags:        map[string]string{"2.16.0": "a", "2.17.0": "c"},
		History: []Event{
			{Tag: "2.1.0", Kind: "new tag", Detected: now},
			{Tag: "2.16.0", Kind: "new tag", Detected: now},
			{Tag: "2.17.0", Kind: "digest changed", Detected: now},
		},
	}
	s.Apply(map[string]string{"2.16.0": "a", "2.17.0": "c"}, now.Add(2*time.Hour), 24*time.Hour)
	if len(s.History) != 1 || s.History[0].Kind != "digest changed" || !s.SemverHistoryMigrated {
		t.Fatalf("legacy history not migrated: %+v", s.History)
	}
}

func TestStateValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, data := range []string{"{", "{}", `{"version":2,"repositories":{}}`, `{"version":1,"repositories":{"r":null}}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(path); err == nil {
			t.Fatalf("accepted %s", data)
		}
		got, _ := os.ReadFile(path)
		if string(got) != data {
			t.Fatal("invalid state overwritten")
		}
	}
}

func TestPersistenceFailureDoesNotAdvance(t *testing.T) {
	cfg := testConfig(t)
	f := &fakeRegistry{tags: map[string]string{"1.0.0": "a"}}
	m, err := New(cfg, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	f.tags["1.0.0"] = "b"
	good := m.cfg.StateFile
	m.cfg.StateFile = filepath.Join(good, "impossible.json") // Parent is a regular file.
	m.Poll(context.Background())
	if getReport(m).StateError == "" {
		t.Fatal("write error not shown")
	}
	if m.state.Repositories["index.docker.io/library/alpine"].Tags["1.0.0"] != "a" {
		t.Fatal("failed write advanced in-memory baseline")
	}
	m.cfg.StateFile = good
	m.Poll(context.Background())
	if len(getReport(m).Repositories[0].History) != 1 {
		t.Fatal("retry lost event")
	}
}

func TestRunningVersionsAndPlatform(t *testing.T) {
	cfg := testConfig(t)
	p := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	f := &fakeRegistry{tags: map[string]string{"1.9.0": "x", "1.10.0": "y", "2.0.0-rc.1": "z"}, configDigest: "sha256:old"}
	rt := fakeRuntime{containers: []docker.Container{
		{Name: "mail", Image: "alpine:1.9.0", Repo: "index.docker.io/library/alpine", Tag: "1.9.0", ImageID: "sha256:old", Platform: p},
		{Name: "unrelated", Repo: "index.docker.io/library/other"},
	}}
	m, err := New(cfg, f, rt)
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	row := getReport(m).Repositories[0]
	if len(row.Running) != 1 || !reflect.DeepEqual(row.Running[0].Updates, []string{"1.10.0"}) {
		t.Fatalf("%+v", row.Running)
	}
	if !reflect.DeepEqual(f.platform, p) || !strings.Contains(row.Running[0].Status, "matches running") {
		t.Fatal("wrong platform/content comparison")
	}
	f.configDigest = "sha256:new"
	m.Poll(context.Background())
	if !strings.Contains(getReport(m).Repositories[0].Running[0].Status, "different image content") {
		t.Fatal("moved running tag not detected")
	}
}

func TestRuntimeFailureIsVisible(t *testing.T) {
	cfg := testConfig(t)
	m, err := New(cfg, &fakeRegistry{tags: map[string]string{}}, fakeRuntime{err: errors.New("permission denied")})
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	if !strings.Contains(getReport(m).DockerStatus, "permission denied") {
		t.Fatal("runtime failure hidden")
	}
}

func TestNewerAndConfiguredBaseline(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repositories[0].Baseline = "v1.9.0"
	f := &fakeRegistry{tags: map[string]string{"v1.9.0": "a", "1.10.0": "b", "2.0.0": "c", "2.0.0-rc.1": "d"}}
	m, err := New(cfg, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Poll(context.Background())
	if !reflect.DeepEqual(getReport(m).Repositories[0].Updates, []string{"2.0.0", "1.10.0"}) {
		t.Fatal("baseline version order incorrect")
	}
	if got := newer(f.tags, "latest", cfg.Repositories[0]); len(got) != 0 {
		t.Fatal("guessed ordering for latest")
	}
}

func TestViewDuringPolling(t *testing.T) {
	cfg := testConfig(t)
	m, err := New(cfg, &fakeRegistry{tags: map[string]string{"1.0.0": "a"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = getReport(m)
		}
	}()
	for i := 0; i < 5; i++ {
		m.Poll(context.Background())
	}
	wg.Wait()
}

func TestCanceledSnapshotDoesNotInitialize(t *testing.T) {
	cfg := testConfig(t)
	m, err := New(cfg, &fakeRegistry{tags: map[string]string{"1.0.0": "a"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Poll(ctx)
	if getReport(m).Repositories[0].Initialized {
		t.Fatal("canceled poll initialized baseline")
	}
}
