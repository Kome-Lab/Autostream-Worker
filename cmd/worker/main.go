package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	addr, err := workerBindAddrFromEnv()
	if err != nil {
		log.Fatalf("invalid AUTOSTREAM_BIND_ADDR: %v", err)
	}
	updaterIdentity := httpapi.NewUpdaterIdentityLatch(control.ServiceType)
	if _, err := updaterIdentity.ResolveFromEnv(); err != nil && !errors.Is(err, httpapi.ErrUpdaterIdentityPending) {
		log.Fatalf("invalid updater identity: %v", err)
	}
	publisher := buildPublisher()

	controlClient := control.Client{Config: control.ConfigFromEnv()}
	if err := requireMatchingUpdaterIdentity(updaterIdentity, controlClient.Config.ServiceID); err != nil && !errors.Is(err, httpapi.ErrUpdaterIdentityPending) {
		log.Fatalf("invalid updater identity: %v", err)
	}
	manager := jobs.NewManager(publisher, buildReporter(controlClient))
	manager.SetCaptionRuntime(jobs.RuntimeSecretResolverFunc(func(resolveCtx context.Context, streamID, secretName string) (control.RuntimeSecret, error) {
		return (control.Client{Config: control.ConfigFromEnv()}).ResolveRuntimeSecret(resolveCtx, streamID, secretName)
	}), nil)
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
	} else if control.NodeConfigPendingFromEnv() {
		go runPendingControlPanelRegistrationLoop(ctx, manager, requireControlPanelRuntimeConfig(), updaterIdentity)
	} else if requireControlPanelRuntimeConfig() {
		if strings.TrimSpace(controlClient.Config.ConfigError) != "" {
			log.Fatalf("node config invalid: %v", controlClient.Config.ConfigError)
		} else {
			log.Fatal("CONTROL_PANEL_URL and CONTROL_PANEL_TOKEN are required in this environment")
		}
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewServerWithRuntimeConfigAndUpdaterIdentity(control.ServiceType, manager, httpapi.TokenVerifierFromEnv(), controlRuntimeConfigFromEnv, updaterIdentity),
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
		manager.Close(context.Background())
		if err != nil {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		manager.Close(shutdownCtx)
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("worker shutdown failed: %v", err)
			if closeErr := server.Close(); closeErr != nil {
				log.Printf("worker close failed: %v", closeErr)
			}
		}
	}
}

func workerBindAddrFromEnv() (string, error) {
	const defaultAddr = "127.0.0.1:8080"

	addr := strings.TrimSpace(os.Getenv("AUTOSTREAM_BIND_ADDR"))
	if addr == "" {
		addr = defaultAddr
	}
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", fmt.Errorf("port must be an integer: %w", err)
	}
	if port < 1024 || port > 65535 {
		return "", fmt.Errorf("port %d is outside the supported range 1024-65535", port)
	}
	return addr, nil
}

func controlRuntimeConfigFromEnv(ctx context.Context) (control.RuntimeConfig, error) {
	return control.Client{Config: control.ConfigFromEnv()}.RuntimeConfig(ctx)
}

func runPendingControlPanelRegistrationLoop(ctx context.Context, manager *jobs.Manager, requireRuntimeConfig bool, updaterIdentity *httpapi.UpdaterIdentityLatch) {
	lastState := ""
	registeredServiceID := ""
	for {
		cfg := control.ConfigFromEnv()
		client := control.Client{Config: cfg}
		wait := controlPanelRegistrationInterval(cfg)
		state := ""
		if err := requireMatchingUpdaterIdentity(updaterIdentity, cfg.ServiceID); err != nil {
			if errors.Is(err, httpapi.ErrUpdaterIdentityPending) {
				state = "pending:" + control.NodeConfigPathFromEnv()
				logRegistrationStateChange(&lastState, state, "node config pending: waiting for %s", control.NodeConfigPathFromEnv())
				registeredServiceID = ""
			} else {
				log.Fatalf("updater identity invalid: %v", err)
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		switch {
		case strings.TrimSpace(cfg.ConfigError) != "":
			state = "invalid:" + cfg.ConfigError
			if requireRuntimeConfig {
				log.Fatalf("node config invalid: %v", cfg.ConfigError)
			}
			logRegistrationStateChange(&lastState, state, "node config invalid: %v", cfg.ConfigError)
			registeredServiceID = ""
		case strings.TrimSpace(cfg.ControlPanelURL) == "" || strings.TrimSpace(cfg.Token) == "":
			state = "pending:" + control.NodeConfigPathFromEnv()
			logRegistrationStateChange(&lastState, state, "node config pending: waiting for %s", control.NodeConfigPathFromEnv())
			registeredServiceID = ""
		default:
			if registeredServiceID != cfg.ServiceID {
				if err := client.Register(ctx); err != nil {
					state = "register-failed:" + err.Error()
					if requireRuntimeConfig {
						log.Fatalf("control panel registration is required in this environment: %v", err)
					}
					logRegistrationStateChange(&lastState, state, "control panel registration failed: %v", err)
					registeredServiceID = ""
					break
				}
				registeredServiceID = cfg.ServiceID
				state = "registered:" + cfg.ServiceID
				logRegistrationStateChange(&lastState, state, "registered with control panel as %s", cfg.ServiceID)
				if runtimeCfg, ok := logRuntimeConfig(ctx, client); ok {
					manager.ApplyRuntimeConfig(runtimeCfg)
				} else if requireRuntimeConfig {
					log.Fatal("control panel runtime config is required in this environment")
				}
			}
			if registeredServiceID == cfg.ServiceID {
				if err := client.HeartbeatWithMetrics(ctx, "online", manager.CurrentStreamID(), manager.Metrics()); err != nil {
					state = "heartbeat-failed:" + err.Error()
					logRegistrationStateChange(&lastState, state, "control panel heartbeat failed: %v", err)
				} else {
					lastState = "online:" + cfg.ServiceID
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func requireMatchingUpdaterIdentity(latch *httpapi.UpdaterIdentityLatch, serviceID string) error {
	identity, err := latch.ResolveFromEnv()
	if err != nil {
		return err
	}
	if strings.TrimSpace(serviceID) != identity.ServiceID {
		return fmt.Errorf("%w: control client service id does not match the updater identity", httpapi.ErrUpdaterIdentityDrift)
	}
	return nil
}

func controlPanelRegistrationInterval(cfg control.Config) time.Duration {
	if strings.TrimSpace(cfg.ControlPanelURL) != "" && strings.TrimSpace(cfg.Token) != "" && cfg.HeartbeatEvery > 0 {
		return cfg.HeartbeatEvery
	}
	return 10 * time.Second
}

func logRegistrationStateChange(lastState *string, state, format string, args ...any) {
	if state == *lastState {
		return
	}
	log.Printf(format, args...)
	*lastState = state
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
