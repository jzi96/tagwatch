package config

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/name"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen       string       `yaml:"listen"`
	Interval     string       `yaml:"interval"`
	Timeout      string       `yaml:"timeout"`
	StateFile    string       `yaml:"state_file"`
	HistoryDays  int          `yaml:"history_days"`
	DockerSocket string       `yaml:"docker_socket"`
	Repositories []Repository `yaml:"repositories"`
}

type Repository struct {
	Image              string   `yaml:"image"`
	Baseline           string   `yaml:"baseline"`
	SemverOnly         *bool    `yaml:"semver_only"`
	IncludePrereleases bool     `yaml:"include_prereleases"`
	Allow              []string `yaml:"allow"`
	Deny               []string `yaml:"deny"`
}

func Load(path string) (Config, error) {
	c := Config{Listen: ":8080", Interval: "1h", Timeout: "5m", StateFile: "/data/state.json", HistoryDays: 30}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	for key, value := range map[string]string{"interval": c.Interval, "timeout": c.Timeout} {
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Second {
			return fmt.Errorf("%s must be a duration of at least 1s", key)
		}
	}
	if c.Listen == "" || c.StateFile == "" {
		return fmt.Errorf("listen and state_file must not be empty")
	}
	if c.HistoryDays < 1 || c.HistoryDays > 3650 {
		return fmt.Errorf("history_days must be between 1 and 3650")
	}
	if len(c.Repositories) == 0 {
		return fmt.Errorf("configure at least one repository")
	}
	seen := map[string]bool{}
	for _, r := range c.Repositories {
		repo, err := ParseRepository(r.Image)
		if err != nil {
			return fmt.Errorf("repository %q: %w", r.Image, err)
		}
		if seen[repo.Name()] {
			return fmt.Errorf("duplicate repository %q", r.Image)
		}
		seen[repo.Name()] = true
		for _, pattern := range append(append([]string{}, r.Allow...), r.Deny...) {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("%s: invalid filter: %w", r.Image, err)
			}
		}
		if r.Baseline != "" {
			if _, err := name.NewTag(repo.Name()+":"+r.Baseline, name.StrictValidation); err != nil {
				return fmt.Errorf("%s: invalid baseline tag: %w", r.Image, err)
			}
			if Version(r.Baseline) == nil {
				return fmt.Errorf("%s: baseline must be a full semantic version tag", r.Image)
			}
		}
	}
	return nil
}

func ParseRepository(s string) (name.Repository, error) {
	if s == "" || strings.ContainsAny(s, "@?#") || strings.Contains(s, "://") {
		return name.Repository{}, fmt.Errorf("use a repository name without a tag, digest, or URL scheme")
	}
	last := s[strings.LastIndex(s, "/")+1:]
	if strings.Contains(last, ":") {
		return name.Repository{}, fmt.Errorf("configure repository names without tags")
	}
	return name.NewRepository(s, name.WeakValidation)
}

func Version(tag string) *semver.Version {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(tag, "v"))
	if err != nil {
		return nil
	}
	return v
}

// Accept applies deny first, then allow, then semantic-version policy.
// Regexes are unanchored unless the configuration uses ^ and $.
func (r Repository) Accept(tag string) bool {
	for _, p := range r.Deny {
		if regexp.MustCompile(p).MatchString(tag) {
			return false
		}
	}
	if len(r.Allow) > 0 {
		matched := false
		for _, p := range r.Allow {
			if regexp.MustCompile(p).MatchString(tag) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	v := Version(tag)
	if r.SemverOnly == nil || *r.SemverOnly {
		if v == nil {
			return false
		}
	}
	return v == nil || r.IncludePrereleases || v.Prerelease() == ""
}

func CompareTags(a, b string) int {
	va, vb := Version(a), Version(b)
	if va != nil && vb != nil {
		if c := va.Compare(vb); c != 0 {
			return c
		}
	} else if va != nil {
		return 1
	} else if vb != nil {
		return -1
	}
	return strings.Compare(a, b)
}
