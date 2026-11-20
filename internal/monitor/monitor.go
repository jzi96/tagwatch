package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"tagwatch/internal/config"
	"tagwatch/internal/docker"
)

type Registry interface {
	Tags(context.Context, name.Repository) ([]string, error)
	Digest(context.Context, name.Repository, string) (string, error)
	ConfigDigest(context.Context, name.Repository, string, v1.Platform) (string, error)
}

type Runtime interface {
	Running(context.Context) ([]docker.Container, error)
}

type RunningReport struct {
	Name    string
	Image   string
	Status  string
	Updates []string
}

type RepositoryReport struct {
	Image       string
	Canonical   string
	LastSuccess time.Time
	LastAttempt time.Time
	Initialized bool
	TagCount    int
	Error       string
	History     []Event
	Running     []RunningReport
	Baseline    string
	Updates     []string
}

type Report struct {
	Generated    time.Time
	HistoryDays  int
	Interval     string
	DockerStatus string
	StateError   string
	Repositories []RepositoryReport
}

type Monitor struct {
	cfg      config.Config
	registry Registry
	runtime  Runtime
	state    State // Owned by the polling goroutine only.
	mu       sync.RWMutex
	report   Report // Immutable after publication.
}

func New(cfg config.Config, registry Registry, runtime Runtime) (*Monitor, error) {
	s, err := LoadState(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	m := &Monitor{cfg: cfg, registry: registry, runtime: runtime, state: s}
	m.report = m.makeReport(s, time.Now().UTC())
	if runtime != nil {
		m.report.DockerStatus = "Docker inspection pending."
	}
	return m, nil
}

// View holds the read lock during rendering; no network or disk access occurs here.
func (m *Monitor) View(fn func(Report)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fn(m.report)
}

func (m *Monitor) publish(r Report) { m.mu.Lock(); m.report = r; m.mu.Unlock() }

func (m *Monitor) Run(ctx context.Context) {
	interval, _ := time.ParseDuration(m.cfg.Interval)
	for {
		if ctx.Err() != nil {
			return
		}
		m.Poll(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Monitor) Poll(ctx context.Context) {
	// Build a detached candidate; a failed disk write must not advance the baseline.
	next := State{Version: 1, Repositories: map[string]*RepositoryState{}}
	timeout, _ := time.ParseDuration(m.cfg.Timeout)
	retention := time.Duration(m.cfg.HistoryDays) * 24 * time.Hour
	for _, r := range m.cfg.Repositories {
		if ctx.Err() != nil {
			return
		}
		repo, _ := config.ParseRepository(r.Image)
		rs := RepositoryState{}
		if old := m.state.Repositories[repo.Name()]; old != nil {
			rs = *old
		}
		op, cancel := context.WithTimeout(ctx, timeout)
		tags, err := m.snapshot(op, repo, r)
		cancel()
		now := time.Now().UTC()
		if err != nil {
			rs.LastAttempt, rs.Error = now, err.Error()
			slog.Warn("repository check failed", "repository", r.Image, "error", err)
		} else {
			rs.Apply(tags, now, retention)
			slog.Info("repository checked", "repository", r.Image, "tags", len(tags))
		}
		next.Repositories[repo.Name()] = &rs
	}
	if ctx.Err() != nil {
		return
	}
	if err := SaveState(m.cfg.StateFile, next); err != nil {
		r := m.makeReport(m.state, time.Now().UTC())
		r.StateError = fmt.Sprintf("Cannot persist results; baseline was not advanced: %v", err)
		m.publish(r)
		slog.Error("state write failed", "error", err)
		return
	}
	m.state = next
	report := m.makeReport(next, time.Now().UTC())
	if m.runtime != nil {
		op, cancel := context.WithTimeout(ctx, timeout)
		m.addRunning(op, &report)
		cancel()
	}
	m.publish(report)
}

func (m *Monitor) snapshot(ctx context.Context, repo name.Repository, cfg config.Repository) (map[string]string, error) {
	tags, err := m.registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	var selected []string
	seen := map[string]bool{}
	for _, tag := range tags {
		if cfg.Accept(tag) && !seen[tag] {
			if _, err := name.NewTag(repo.Name()+":"+tag, name.StrictValidation); err != nil {
				return nil, err
			}
			seen[tag] = true
			selected = append(selected, tag)
		}
	}
	// Bound registry traffic. All accepted tags are checked, so moved version tags
	// are detected too. A partial failure never replaces a complete baseline.
	type result struct {
		tag, digest string
		err         error
	}
	jobs := make(chan string)
	results := make(chan result)
	op, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tag := range jobs {
				digest, err := m.registry.Digest(op, repo, tag)
				if err == nil && digest == "" {
					err = fmt.Errorf("empty manifest digest for %s", tag)
				}
				select {
				case results <- result{tag, digest, err}:
				case <-op.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, tag := range selected {
			select {
			case jobs <- tag:
			case <-op.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	snapshot := make(map[string]string, len(selected))
	var snapshotErr error
	for got := range results {
		if got.err != nil {
			if snapshotErr == nil {
				snapshotErr = got.err
				cancel()
			}
			continue
		}
		snapshot[got.tag] = got.digest
	}
	if snapshotErr != nil {
		return nil, snapshotErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (m *Monitor) makeReport(s State, now time.Time) Report {
	r := Report{Generated: now, HistoryDays: m.cfg.HistoryDays, Interval: m.cfg.Interval, DockerStatus: "Docker inspection disabled."}
	for _, cfg := range m.cfg.Repositories {
		repo, _ := config.ParseRepository(cfg.Image)
		row := RepositoryReport{Image: cfg.Image, Canonical: repo.Name(), Baseline: cfg.Baseline}
		if rs := s.Repositories[repo.Name()]; rs != nil {
			row.LastSuccess, row.LastAttempt = rs.LastSuccess, rs.LastAttempt
			row.Initialized, row.Error, row.TagCount = rs.Initialized, rs.Error, len(rs.Tags)
			cutoff := now.Add(-time.Duration(m.cfg.HistoryDays) * 24 * time.Hour)
			for _, e := range rs.History {
				if !e.Detected.Before(cutoff) && cfg.Accept(e.Tag) {
					row.History = append(row.History, e)
				}
			}
			sort.Slice(row.History, func(i, j int) bool {
				a, b := row.History[i], row.History[j]
				if a.Detected.Equal(b.Detected) {
					return config.CompareTags(a.Tag, b.Tag) > 0
				}
				return a.Detected.After(b.Detected)
			})
			if cfg.Baseline != "" {
				row.Updates = newer(rs.Tags, cfg.Baseline, cfg)
			}
		}
		r.Repositories = append(r.Repositories, row)
	}
	return r
}

func newer(tags map[string]string, baseline string, cfg config.Repository) []string {
	base := config.Version(baseline)
	if base == nil {
		return nil
	}
	var result []string
	for tag := range tags {
		if v := config.Version(tag); v != nil && v.GreaterThan(base) && cfg.Accept(tag) {
			result = append(result, tag)
		}
	}
	sort.Slice(result, func(i, j int) bool { return config.CompareTags(result[i], result[j]) > 0 })
	return result
}

func (m *Monitor) addRunning(ctx context.Context, report *Report) {
	containers, err := m.runtime.Running(ctx)
	if err != nil {
		report.DockerStatus = "Docker inspection failed; running comparisons unavailable: " + err.Error()
		return
	}
	report.DockerStatus = "Docker inspection enabled. Only configured repositories are shown."
	for i := range report.Repositories {
		row := &report.Repositories[i]
		rs := m.state.Repositories[row.Canonical]
		cfg := m.cfg.Repositories[i]
		repo, _ := config.ParseRepository(cfg.Image)
		for _, ctr := range containers {
			if ctr.Repo != row.Canonical {
				continue
			}
			running := RunningReport{Name: ctr.Name, Image: ctr.Image}
			if rs != nil {
				running.Updates = newer(rs.Tags, ctr.Tag, cfg)
			}
			switch {
			case ctr.Tag == "":
				running.Status = "Digest-pinned image: version ordering unavailable."
			case ctr.ImageID == "" || ctr.Platform.OS == "" || ctr.Platform.Architecture == "":
				running.Status = "Local image identity/platform unavailable; digest comparison skipped."
			default:
				digest, err := m.registry.ConfigDigest(ctx, repo, ctr.Tag, ctr.Platform)
				switch {
				case err != nil:
					running.Status = "Cannot compare running image: " + err.Error()
				case digest != ctr.ImageID:
					running.Status = "Same tag now points to different image content for this platform."
				default:
					running.Status = "Same tag matches running image content for this platform."
				}
				if config.Version(ctr.Tag) == nil {
					running.Status += " Non-SemVer tag: version ordering unavailable."
				}
			}
			row.Running = append(row.Running, running)
		}
		sort.Slice(row.Running, func(i, j int) bool { return row.Running[i].Name < row.Running[j].Name })
	}
}
