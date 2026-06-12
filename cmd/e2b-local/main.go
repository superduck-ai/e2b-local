package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	_ "e2b-local/internal/backends/applecontainer"
	_ "e2b-local/internal/backends/docker"
	_ "e2b-local/internal/backends/orbstack"
	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"
)

var (
	loadGatewayConfig = gateway.LoadConfig
	newGatewayApp     = func(cfg gateway.Config, logger *log.Logger) (http.Handler, error) {
		return gateway.NewAppWithRuntime(cfg, logger, nil)
	}
	listenAndServe = http.ListenAndServe
)

const appShutdownTimeout = 30 * time.Second

func loadEnv() {
	envFile := ".env"
	if profile := os.Getenv("ENV_PROFILE"); profile != "" {
		dotFile := ".env." + profile
		if _, err := os.Stat(dotFile); err == nil {
			envFile = dotFile
		} else {
			envFile = ".env_" + profile
		}
	}
	if err := godotenv.Load(envFile); err != nil {
		log.Printf("Warning: failed to load %s: %v", envFile, err)
	}
}

func main() {
	loadEnv()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, logger); err != nil {
		logger.Fatalf("e2b-local failed: %v", err)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, logger *log.Logger) error {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	cmd := newRootCommand(logger)
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	return cmd.ExecuteContext(ctx)
}

type cliOptions struct {
	ConfigPath string
}

func newRootCommand(logger *log.Logger) *cobra.Command {
	options := &cliOptions{
		ConfigPath: "config.yaml",
	}

	rootCmd := &cobra.Command{
		Use:              "e2b-local",
		Short:            "Run the local E2B-compatible gateway and helper commands",
		SilenceErrors:    true,
		SilenceUsage:     true,
		TraverseChildren: true,
		Args:             cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveWithConfig(options.ConfigPath, logger)
		},
	}
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.PersistentFlags().StringVar(&options.ConfigPath, "config", options.ConfigPath, "path to gateway config file")
	rootCmd.AddCommand(newServeCommand(options, logger))
	rootCmd.AddCommand(newVolumeCommand(options, logger))

	return rootCmd
}

func newServeCommand(options *cliOptions, logger *log.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the e2b-local gateway server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveWithConfig(options.ConfigPath, logger)
		},
	}
}

func newVolumeCommand(options *cliOptions, logger *log.Logger) *cobra.Command {
	volumeCmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage volumes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	volumeCmd.AddCommand(newVolumeCreateCommand(options, logger))
	return volumeCmd
}

func newVolumeCreateCommand(options *cliOptions, logger *log.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGatewayConfig(options.ConfigPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			return executeVolumeCreate(cmd.Context(), cfg, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr(), logger)
		},
	}
}

func serveWithConfig(configPath string, logger *log.Logger) error {
	cfg, err := loadGatewayConfig(strings.TrimSpace(configPath))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	app, err := newGatewayApp(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create app: %w", err)
	}
	defer shutdownGatewayApp(app, logger)

	logger.Printf("starting e2b-local addr=%s runtime=%s",
		cfg.Server.Addr,
		cfg.Runtime.Type,
	)

	return listenAndServe(cfg.Server.Addr, app)
}

func shutdownGatewayApp(app http.Handler, logger *log.Logger) {
	shutdowner, ok := app.(interface {
		Shutdown(context.Context) error
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), appShutdownTimeout)
	defer cancel()
	if err := shutdowner.Shutdown(ctx); err != nil {
		logger.Printf("gateway shutdown failed: %v", err)
	}
}

func executeVolumeCreate(ctx context.Context, cfg gateway.Config, name string, stdout io.Writer, stderr io.Writer, logger *log.Logger) error {
	app, err := newGatewayApp(cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to create app: %w", err)
	}

	payload, err := json.Marshal(e2bapi.NewVolume{Name: name})
	if err != nil {
		return fmt.Errorf("marshal create volume request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/volumes", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build create volume request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	body := recorder.Body.Bytes()
	if recorder.Code >= http.StatusOK && recorder.Code < http.StatusMultipleChoices {
		return writeCLIResponse(stdout, body)
	}

	if err := writeCLIResponse(stderr, body); err != nil {
		return err
	}
	if len(body) > 0 {
		return fmt.Errorf("volume create failed with status %d", recorder.Code)
	}
	return fmt.Errorf("volume create failed with status %d and empty response body", recorder.Code)
}

func writeCLIResponse(w io.Writer, body []byte) error {
	if w == nil || len(body) == 0 {
		return nil
	}

	if _, err := w.Write(body); err != nil {
		return err
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		return nil
	}
	_, err := io.WriteString(w, "\n")
	return err
}
