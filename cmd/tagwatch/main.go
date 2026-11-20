package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tagwatch/internal/config"
	"tagwatch/internal/docker"
	"tagwatch/internal/monitor"
	"tagwatch/internal/registry"
	"tagwatch/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tagwatch stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "YAML configuration file")
	once := flag.Bool("once", false, "check once, persist state, then exit (no HTTP server)")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	var runtime monitor.Runtime
	if cfg.DockerSocket != "" {
		runtime = docker.New(cfg.DockerSocket)
	}
	m, err := monitor.New(cfg, registry.Client{}, runtime)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		m.Poll(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var checkErr error
		m.View(func(r monitor.Report) {
			if r.StateError != "" {
				checkErr = errors.New(r.StateError)
			}
			for _, repo := range r.Repositories {
				if repo.Error != "" {
					checkErr = fmt.Errorf("%s: %s", repo.Image, repo.Error)
				}
			}
		})
		return checkErr
	}
	server := &http.Server{
		Addr: cfg.Listen, Handler: web.Handler(m),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); m.Run(ctx) }()
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	slog.Info("serving report", "listen", cfg.Listen)
	select {
	case err = <-errs:
		stop()
	case <-ctx.Done():
		err = nil
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdown)
	stop()
	<-pollDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}
