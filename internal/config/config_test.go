package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilters(t *testing.T) {
	no := false
	cases := []struct {
		name string
		repo Repository
		tag  string
		want bool
	}{
		{"stable", Repository{}, "1.2.3", true},
		{"v prefix", Repository{}, "v1.2.3", true},
		{"partial excluded", Repository{}, "1.2", false},
		{"latest excluded", Repository{}, "latest", false},
		{"nightly excluded", Repository{}, "nightly", false},
		{"pre excluded", Repository{}, "1.2.3-rc.1", false},
		{"pre included", Repository{IncludePrereleases: true}, "1.2.3-rc.1", true},
		{"allow restricted", Repository{Allow: []string{"^v"}}, "1.2.3", false},
		{"deny wins", Repository{Allow: []string{".*"}, Deny: []string{"^1"}}, "1.2.3", false},
		{"allow any matches", Repository{Allow: []string{"^v", "^2"}}, "2.0.0", true},
		{"nonsemver opt in", Repository{SemverOnly: &no, Allow: []string{"^latest$"}}, "latest", true},
		{"leading zero", Repository{}, "01.2.3", false},
		{"stalwart stable", Repository{IncludePrereleases: true, Allow: []string{`^v\d+\.\d+\.\d+(-alpine)?$`}}, "v1.1.1", true},
		{"stalwart alpine", Repository{IncludePrereleases: true, Allow: []string{`^v\d+\.\d+\.\d+(-alpine)?$`}}, "v1.1.1-alpine", true},
		{"stalwart rejects floating alias", Repository{IncludePrereleases: true, Allow: []string{`^v\d+\.\d+\.\d+(-alpine)?$`}}, "v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.repo.Accept(tc.tag); got != tc.want {
				t.Fatalf("Accept(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

func TestRepositoryNames(t *testing.T) {
	for input, want := range map[string]string{
		"stalwartlabs/stalwart":                "index.docker.io/stalwartlabs/stalwart",
		"alpine":                               "index.docker.io/library/alpine",
		"docker.io/library/alpine":             "index.docker.io/library/alpine",
		"mcr.microsoft.com/dotnet/runtime":     "mcr.microsoft.com/dotnet/runtime",
		"us-docker.pkg.dev/project/repo/image": "us-docker.pkg.dev/project/repo/image",
		"gcr.io/project/image":                 "gcr.io/project/image",
		"registry.example.com:5000/team/image": "registry.example.com:5000/team/image",
	} {
		got, err := ParseRepository(input)
		if err != nil || got.Name() != want {
			t.Errorf("%s: %v %v", input, got, err)
		}
	}
	for _, input := range []string{"", "https://gcr.io/project/image", "alpine:latest", "team/image@sha256:abc", "alpine?tag=1"} {
		if _, err := ParseRepository(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	save := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	save("repositories:\n  - image: stalwartlabs/stalwart\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.HistoryDays != 30 || c.Interval != "1h" || c.DockerSocket != "" {
		t.Fatalf("defaults: %+v", c)
	}
	if c.ScanConcurrency != 12 || c.PollConcurrency != 3 {
		t.Fatalf("scan defaults: %+v", c)
	}
	save("repositories:\n  - image: alpine\n    scope: pending\nscan_concurrency: 32\npoll_concurrency: 8\n")
	if parsed, err := Load(path); err != nil {
		t.Fatal(err)
	} else if parsed.ScanConcurrency != 32 || parsed.PollConcurrency != 8 || parsed.Repositories[0].Scope != "pending" {
		t.Fatalf("explicit scan settings: %+v", parsed)
	}
	for _, input := range []string{
		"repositories: []",
		"interval: 0s\nrepositories:\n  - image: alpine",
		"unknown: yes\nrepositories:\n  - image: alpine",
		"repositories:\n  - image: alpine\n    deny: ['[']",
		"repositories:\n  - image: alpine\n  - image: docker.io/library/alpine",
		"repositories:\n  - image: alpine\n    baseline: latest",
		"repositories:\n  - image: alpine\n---\nrepositories: []",
		"scan_concurrency: 0\nrepositories:\n  - image: alpine",
		"scan_concurrency: 100\nrepositories:\n  - image: alpine",
		"poll_concurrency: 0\nrepositories:\n  - image: alpine",
		"poll_concurrency: 20\nrepositories:\n  - image: alpine",
		"repositories:\n  - image: alpine\n    scope: everything",
	} {
		save(input)
		if _, err := Load(path); err == nil {
			t.Errorf("accepted invalid config %s", input)
		}
	}
}

func TestSemanticOrdering(t *testing.T) {
	if CompareTags("1.10.0", "1.9.9") <= 0 {
		t.Fatal("lexical ordering used")
	}
	if CompareTags("1.0.0", "1.0.0-rc.1") <= 0 {
		t.Fatal("release should follow prerelease")
	}
}
