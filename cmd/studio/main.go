// Command studio is the Calystral Studio BFF. It exposes two subcommands:
// `studio serve` (run the HTTP server) and `studio version`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/config"
	"github.com/calystral-io/studio/internal/coreclient"
	"github.com/calystral-io/studio/internal/httpapi"
	"github.com/calystral-io/studio/internal/version"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "studio",
		Short:         "Calystral Studio BFF",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd(), versionCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(version.Current())
		},
	}
}

func serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the BFF HTTP server",
		RunE:  runServe,
	}
	// Flags mirror the env vars; env wins over defaults, an explicitly-set flag
	// wins over env (resolved in config.Load via Flags pointers).
	f := cmd.Flags()
	f.String("http-addr", "", "listen address ("+config.EnvHTTPAddr+")")
	f.String("auth-mode", "", "auth mode: mock|nexus ("+config.EnvAuthMode+")")
	f.String("core-source", "", "node source: fixture|grpc ("+config.EnvCoreSource+")")
	f.String("core-grpc-addr", "", "Core gRPC endpoint ("+config.EnvCoreGRPCAddr+")")
	f.String("core-grpc-addrs", "", "comma-separated Core replica endpoints for cluster fan-out ("+config.EnvCoreGRPCAddrs+")")
	f.String("core-dev-signing-key", "", "dev EdDSA signing key path/inline ("+config.EnvCoreDevSigningKey+")")
	f.String("cors-origins", "", "comma-separated allowed origins ("+config.EnvCORSOrigins+")")
	f.String("log-level", "", "log level: debug|info|warn|error ("+config.EnvLogLevel+")")
	return cmd
}

// flagOverrides reads only explicitly-set flags into config.Flags pointers.
func flagOverrides(cmd *cobra.Command) config.Flags {
	get := func(name string) *string {
		if !cmd.Flags().Changed(name) {
			return nil
		}
		v, _ := cmd.Flags().GetString(name)
		return &v
	}
	return config.Flags{
		HTTPAddr:          get("http-addr"),
		AuthMode:          get("auth-mode"),
		CoreSource:        get("core-source"),
		CoreGRPCAddr:      get("core-grpc-addr"),
		CoreGRPCAddrs:     get("core-grpc-addrs"),
		CoreDevSigningKey: get("core-dev-signing-key"),
		CORSOrigins:       get("cors-origins"),
		LogLevel:          get("log-level"),
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(os.LookupEnv, flagOverrides(cmd))
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)

	authn, err := buildAuthenticator(cfg)
	if err != nil {
		return err
	}

	core, err := buildCoreClient(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = core.Close() }()

	srv := httpapi.New(authn, core, logger, httpapi.Options{CORSOrigins: cfg.CORSOrigins})

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("studio serve starting",
		"addr", cfg.HTTPAddr,
		"auth_mode", string(cfg.AuthMode),
		"core_source", string(cfg.CoreSource),
		"version", version.Version,
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("studio serve shutting down")
		// Terminate live WebSocket push loops (hijacked conns Shutdown can't track).
		srv.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func buildAuthenticator(cfg config.Config) (auth.Authenticator, error) {
	switch cfg.AuthMode {
	case config.AuthModeMock:
		return auth.MockAuthenticator{}, nil
	case config.AuthModeNexus:
		return nil, fmt.Errorf("auth mode %q not implemented in PR1", cfg.AuthMode)
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.AuthMode)
	}
}

func buildCoreClient(cfg config.Config, logger *slog.Logger) (coreclient.CoreClient, error) {
	switch cfg.CoreSource {
	case config.CoreSourceFixture:
		return coreclient.NewFixture(), nil
	case config.CoreSourceGRPC:
		signer, err := auth.NewPrincipalSigner(cfg.CoreDevSigningKey)
		if err != nil {
			return nil, fmt.Errorf("build principal signer: %w", err)
		}
		if signer.DevGenerated() {
			logger.Warn("using an ephemeral dev EdDSA signing key for x-calystral-principal; set " +
				config.EnvCoreDevSigningKey + " for a stable dev key")
		}
		addrs := cfg.CoreReplicaAddrs()
		if cfg.ClusterMode() {
			// Multiple Core replicas: fan cluster topology reads out across all of
			// them and aggregate (other reads go to the primary).
			logger.Info("core cluster mode: fanning out across replicas", "replicas", addrs)
			return coreclient.NewFanoutClient(addrs, signer)
		}
		return coreclient.NewGRPCClient(addrs[0], signer)
	default:
		return nil, fmt.Errorf("unsupported core source %q", cfg.CoreSource)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
