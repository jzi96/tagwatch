package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tagwatch/internal/config"
)

type Event struct {
	Tag      string    `json:"tag"`
	Kind     string    `json:"kind"`
	Previous string    `json:"previous,omitempty"`
	Digest   string    `json:"digest"`
	Detected time.Time `json:"detected"`
}

type RepositoryState struct {
	Initialized           bool              `json:"initialized"`
	Tags                  map[string]string `json:"tags"`
	History               []Event           `json:"history"`
	SemverHistoryMigrated bool              `json:"semver_history_migrated,omitempty"`
	LastSuccess           time.Time         `json:"last_success"`
	LastAttempt           time.Time         `json:"last_attempt"`
	Error                 string            `json:"error,omitempty"`
}

type State struct {
	Version      int                         `json:"version"`
	Repositories map[string]*RepositoryState `json:"repositories"`
}

func LoadState(path string) (State, error) {
	s := State{Version: 1, Repositories: map[string]*RepositoryState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	// Decode into a zero value so missing version/repositories cannot inherit defaults.
	s = State{}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("invalid state file (not overwritten): %w", err)
	}
	if s.Version != 1 || s.Repositories == nil {
		return s, fmt.Errorf("unsupported or invalid state file (not overwritten)")
	}
	for key, r := range s.Repositories {
		if r == nil || (r.Initialized && r.Tags == nil) {
			return s, fmt.Errorf("invalid repository state for %s", key)
		}
	}
	return s, nil
}

func SaveState(path string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tagwatch-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Apply advances only complete, successful snapshots. The first snapshot is a baseline.
func (r *RepositoryState) Apply(tags map[string]string, now time.Time, retention time.Duration) {
	history := make([]Event, 0, len(r.History))
	for _, e := range r.History {
		if !e.Detected.Before(now.Add(-retention)) {
			history = append(history, e)
		}
	}
	previousMax := maxSemverTag(r.Tags)
	if r.Initialized && !r.SemverHistoryMigrated {
		filtered := history[:0]
		for _, event := range history {
			if event.Kind == "new tag" && config.Version(event.Tag) != nil {
				continue
			}
			filtered = append(filtered, event)
		}
		history = filtered
		r.SemverHistoryMigrated = true
	}
	if r.Initialized {
		for tag, digest := range tags {
			old, exists := r.Tags[tag]
			switch {
			case !exists:
				if previousMax != "" {
					v := config.Version(tag)
					if v != nil && config.CompareTags(tag, previousMax) <= 0 {
						break
					}
				}
				history = append(history, Event{Tag: tag, Kind: "new tag", Digest: digest, Detected: now})
			case old != digest:
				history = append(history, Event{Tag: tag, Kind: "digest changed", Previous: old, Digest: digest, Detected: now})
			}
		}
	}
	r.Tags, r.History, r.Initialized = tags, history, true
	r.LastSuccess, r.LastAttempt, r.Error = now, now, ""
}

func maxSemverTag(tags map[string]string) string {
	var max string
	for tag := range tags {
		if config.Version(tag) == nil {
			continue
		}
		if max == "" || config.CompareTags(tag, max) > 0 {
			max = tag
		}
	}
	return max
}
