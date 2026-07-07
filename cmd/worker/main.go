package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example/autostream-worker/internal/control"
	"github.com/example/autostream-worker/internal/encoder"
	"github.com/example/autostream-worker/internal/httpapi"
	"github.com/example/autostream-worker/internal/jobs"
	"github.com/example/autostream-worker/internal/observability"
	"github.com/example/autostream-worker/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("autostream-worker %s\ncommit: %s\nbuild_date: %s\n", version.Current(), version.Commit, version.BuildDate)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "configure" {
		if err := control.RunConfigureCommand(os.Args[2:], control.ServiceType, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "configure failed: %v\n", err)
			os.Exit(2)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("AUTOSTREAM_BIND_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	publisher := buildPublisher()

	controlClient := control.Client{Config: control.ConfigFromEnv()}
	manager := jobs.NewManager(publisher, buildReporter(controlClient))
	if controlClient.Config.ControlPanelURL != "" && controlClient.Config.Token != "" {
		if err := controlClient.Register(ctx); err != nil {
			if requireControlPanelRuntimeConfig() {
				log.Fatalf("control panel registration is required in this environment: %v", err)
			}
			log.Printf("control panel registration failed: %v", err)
		} else {
			log.Printf("registered with control panel as %s", controlClient.Config.ServiceID)
			if cfg, ok := logRuntimeConfig(ctx, controlClient); ok {
				manager.ApplyRuntimeConfig(cfg)
			} else if requireControlPanelRuntimeConfig() {
				log.Fatal("control panel runtime config is required in this environment")
			}
		}
		go controlClient.RunHeartbeatLoopWithMetrics(ctx, manager.CurrentStreamID, manager.Metrics, func(err error) {
			log.Printf("control panel heartbeat failed: %v", err)
		})
	} else if requireControlPanelRuntimeConfig() {
		log.Fatal("CONTROL_PANEL_URL and CONTROL_PANEL_TOKEN are required in this environment")
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewServerWithRuntimeConfig(control.ServiceType, manager, httpapi.TokenVerifierFromEnv(), controlClient.RuntimeConfig),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("autostream-worker listening on %s", addr)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("worker shutdown failed: %v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("worker close failed: %v", closeErr)
			}
		}
	}
}

func logRuntimeConfig(ctx context.Context, client control.Client) (control.RuntimeConfig, bool) {
	cfg, err := client.RuntimeConfig(ctx)
	if err != nil {
		log.Printf("control panel runtime config fetch failed: %v", err)
		return control.RuntimeConfig{}, false
	}
	profileCount := 0
	for _, profiles := range cfg.Profiles {
		profileCount += len(profiles)
	}
	log.Printf("loaded control panel runtime config for %s: assignments=%d profiles=%d", cfg.Service.ServiceID, len(cfg.Assignments), profileCount)
	return cfg, true
}

func workerProfileDefaultsFromRuntimeConfig(cfg control.RuntimeConfig) jobs.ProfileDefaults {
	return jobs.ProfileDefaultsFromRuntimeConfig(cfg)
}

func workerAssignmentPolicyFromRuntimeConfig(cfg control.RuntimeConfig) jobs.AssignmentPolicy {
	return jobs.AssignmentPolicyFromRuntimeConfig(cfg)
}

func requireControlPanelRuntimeConfig() bool {
	if envBool("AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG", false) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOSTREAM_ENV")), "production")
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func buildPublisher() encoder.Publisher {
	cfg := encoder.ConfigFromEnv()
	if cfg.URL == "" || cfg.Token == "" {
		log.Printf("static encoder route is incomplete; job-scoped encoder URL and signed ingest token will be preferred")
	}
	return encoder.Client{Config: cfg}
}

func buildReporter(controlClient control.Client) jobs.Reporter {
	obs := observability.NewClientFromEnv()
	if obs.Enabled() {
		return obs
	}
	if controlClient.Config.ControlPanelURL != "" && controlClient.Config.Token != "" {
		return controlClient
	}
	return nil
}
