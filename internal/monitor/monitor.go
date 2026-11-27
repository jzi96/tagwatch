package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
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
	Name           string
	Image          string
	Tag            string
	Status         string
	Updates        []string
	ContentMatches bool
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

	// One Docker inspection per poll. Its tags also decide which manifests are worth fetching.
	var containers []docker.Container
	var dockerErr error
	if m.runtime != nil {
		op, cancel := context.WithTimeout(ctx, timeout)
		containers, dockerErr = m.runtime.Running(op)
		cancel()
		if dockerErr != nil {
			slog.Warn("docker inspection failed", "error", dockerErr)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.pollConcurrency())
	for _, r := range m.cfg.Repositories {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(r config.Repository) {
			defer wg.Done()
			defer func() { <-sem }()
			repo, _ := config.ParseRepository(r.Image)
			rs := RepositoryState{}
			if old := m.state.Repositories[repo.Name()]; old != nil {
				rs = *old
			}
			tags := containerTags(containers, repo.Name())
			op, cancel := context.WithTimeout(ctx, timeout)
			result, err := m.snapshot(op, repo, r, rs, tags)
			cancel()
			now := time.Now().UTC()
			if err != nil {
				rs.LastAttempt, rs.Error = now, err.Error()
				slog.Warn("repository check failed", "repository", r.Image, "error", err)
			} else {
				rs.Apply(result, now, retention)
				slog.Info("repository checked", "repository", r.Image, "tags", len(result))
			}
			mu.Lock()
			next.Repositories[repo.Name()] = &rs
			mu.Unlock()
		}(r)
	}
	wg.Wait()
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
		m.addRunning(op, &report, containers, dockerErr)
		cancel()
	}
	m.publish(report)
}

// pollConcurrency bounds how many repositories are checked at once. Config values are
// clamped here so a Config built without config.Load still makes progress.
func (m *Monitor) pollConcurrency() int {
	n := m.cfg.PollConcurrency
	if n < 1 {
		n = 3
	}
	if n > len(m.cfg.Repositories) {
		n = len(m.cfg.Repositories)
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Monitor) scanConcurrency() int {
	n := m.cfg.ScanConcurrency
	if n < 1 {
		return 12
	}
	if n > 64 {
		return 64
	}
	return n
}

// containerTags returns the tags of the containers belonging to one repository.
func containerTags(containers []docker.Container, repo string) []string {
	var tags []string
	for _, ctr := range containers {
		if ctr.Repo == repo && ctr.Tag != "" {
			tags = append(tags, ctr.Tag)
		}
	}
	return tags
}

func (m *Monitor) snapshot(ctx context.Context, repo name.Repository, cfg config.Repository, prev RepositoryState, containerTags []string) (map[string]string, error) {
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
	// Bound registry traffic. All accepted tags are checked by default, so moved version
	// tags are detected too. A partial failure never replaces a complete baseline.
	scan, skip := m.scanTargets(selected, prev, containerTags, cfg.Scope)
	type result struct {
		tag, digest string
		err         error
	}
	jobs := make(chan string)
	results := make(chan result)
	op, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < m.scanConcurrency(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tag := range jobs {
				digest, err := m.digest(op, repo, tag)
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
		for _, tag := range scan {
			select {
			case jobs <- tag:
			case <-op.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	snapshot := make(map[string]string, len(selected))
	for tag, digest := range skip {
		snapshot[tag] = digest
	}
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

// scanTargets splits accepted tags into tags that need a manifest request and tags whose
// known digest is carried forward. scope=pending skips only tags that are already known and
// cannot produce a pending change: an observed tag at or below the oldest running version is
// hidden by pendingChanges anyway. Unknown tags are always fetched, so a baseline stays
// complete and a newly published tag is never missed. Tags equal to a running tag stay
// scanned because their content still has to be compared.
func (m *Monitor) scanTargets(selected []string, prev RepositoryState, running []string, scope string) ([]string, map[string]string) {
	scan := selected
	if scope != "pending" || !prev.Initialized || len(running) == 0 {
		return scan, nil
	}
	oldest := (*semver.Version)(nil)
	live := map[string]bool{}
	for _, tag := range running {
		live[tag] = true
		if v := config.Version(tag); v != nil && (oldest == nil || v.LessThan(oldest)) {
			oldest = v
		}
	}
	if oldest == nil {
		return scan, nil
	}
	skip := map[string]string{}
	kept := make([]string, 0, len(selected))
	for _, tag := range selected {
		if live[tag] {
			kept = append(kept, tag)
			continue
		}
		v := config.Version(tag)
		if v == nil || v.GreaterThan(oldest) {
			kept = append(kept, tag)
			continue
		}
		if digest, ok := prev.Tags[tag]; ok && digest != "" {
			skip[tag] = digest
			continue
		}
		kept = append(kept, tag)
	}
	return kept, skip
}

// digest retries transient registry failures. A tag that keeps failing still aborts the
// repository scan, so a partial scan never replaces the baseline.
func (m *Monitor) digest(ctx context.Context, repo name.Repository, tag string) (string, error) {
	backoff := []time.Duration{250 * time.Millisecond, time.Second}
	for attempt := 0; ; attempt++ {
		digest, err := m.registry.Digest(ctx, repo, tag)
		if err == nil && digest != "" {
			return digest, nil
		}
		if err == nil {
			err = fmt.Errorf("empty manifest digest for %s", tag)
		}
		if attempt >= len(backoff) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
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
			visible := make([]Event, 0, len(rs.History))
			for _, e := range rs.History {
				if !e.Detected.Before(cutoff) && cfg.Accept(e.Tag) {
					visible = append(visible, e)
				}
			}
			row.History = newestPerTag(visible)
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

// newestPerTag collapses repeated changes of the same tag into the most recently
// observed event. The persisted history keeps every change; only the report shows
// one row per tag, so an upstream rebuild of a mutable tag is not listed once per poll.
func newestPerTag(events []Event) []Event {
	kept := make(map[string]Event, len(events))
	for _, e := range events {
		prev, ok := kept[e.Tag]
		if !ok || !e.Detected.Before(prev.Detected) {
			kept[e.Tag] = e
		}
	}
	result := make([]Event, 0, len(kept))
	for _, e := range kept {
		result = append(result, e)
	}
	return result
}

// pendingChanges drops events that every running container of a repository already
// provides, so an applied update disappears from the report. A container still
// lagging behind keeps the event visible. A container on the exact tag counts as
// deployed only while its platform image content still matches the registry, and
// non-SemVer tags cannot be ordered, so anything else stays pending.
func pendingChanges(events []Event, running []RunningReport) []Event {
	if len(events) == 0 || len(running) == 0 {
		return events
	}
	pending := make([]Event, 0, len(events))
	for _, e := range events {
		if !deployed(running, e.Tag) {
			pending = append(pending, e)
		}
	}
	return pending
}

func deployed(running []RunningReport, tag string) bool {
	want := config.Version(tag)
	for _, ctr := range running {
		if ctr.Tag == tag {
			if !ctr.ContentMatches {
				return false
			}
			continue
		}
		if have := config.Version(ctr.Tag); want != nil && have != nil && !have.LessThan(want) {
			continue
		}
		return false
	}
	return true
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

func (m *Monitor) addRunning(ctx context.Context, report *Report, containers []docker.Container, err error) {
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
			running := RunningReport{Name: ctr.Name, Image: ctr.Image, Tag: ctr.Tag}
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
					running.ContentMatches = true
				}
				if config.Version(ctr.Tag) == nil {
					running.Status += " Non-SemVer tag: version ordering unavailable."
				}
			}
			row.Running = append(row.Running, running)
		}
		sort.Slice(row.Running, func(i, j int) bool { return row.Running[i].Name < row.Running[j].Name })
		row.History = pendingChanges(row.History, row.Running)
	}
}
