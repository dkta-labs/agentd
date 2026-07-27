package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/fleet"
	"github.com/dkta-labs/agentd/internal/herdr"
	"github.com/dkta-labs/agentd/internal/omp"
)

type identity struct {
	NodeID string `json:"nodeId"`
	Token  string `json:"token"`
	Name   string `json:"name"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("agentd-node stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to agentd JSON configuration containing local workspaces")
	controllerURL := flag.String("controller", os.Getenv("AGENTD_CONTROLLER_URL"), "agentd controller URL")
	name := flag.String("name", "", "worker display name")
	dataDir := flag.String("data-dir", "", "worker identity directory")
	flag.Parse()
	if *controllerURL == "" {
		return errors.New("--controller or AGENTD_CONTROLLER_URL is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *dataDir == "" {
		userConfig, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*dataDir = filepath.Join(userConfig, "agentd-node")
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	credentials, err := loadOrEnroll(ctx, *dataDir, *controllerURL, *name, os.Getenv("AGENTD_NODE_ENROLLMENT_TOKEN"))
	if err != nil {
		return err
	}
	worker, err := fleet.NewWorker(fleet.WorkerOptions{
		ControllerURL: *controllerURL,
		Token:         credentials.Token,
		NodeID:        credentials.NodeID,
		Name:          credentials.Name,
		Workspaces:    cfg.Workspaces,
		Driver:        omp.Driver{},
		Attachments:   herdr.NewController(cfg.HerdrBinary, logger),
		Logger:        logger,
	})
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := worker.Close(closeCtx); err != nil {
			logger.Error("worker session shutdown failed", "error", err)
		}
	}()
	logger.Info("agentd node connected", "controller", *controllerURL, "nodeId", credentials.NodeID, "name", credentials.Name)
	err = worker.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func loadOrEnroll(ctx context.Context, dataDir, controllerURL, name, enrollmentToken string) (identity, error) {
	path := filepath.Join(dataDir, "identity.json")
	contents, err := os.ReadFile(path)
	if err == nil {
		var stored identity
		if err := json.Unmarshal(contents, &stored); err != nil {
			return identity{}, fmt.Errorf("decode worker identity: %w", err)
		}
		if stored.NodeID == "" || stored.Token == "" {
			return identity{}, errors.New("worker identity is incomplete")
		}
		return stored, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return identity{}, fmt.Errorf("read worker identity: %w", err)
	}
	if enrollmentToken == "" {
		return identity{}, errors.New("AGENTD_NODE_ENROLLMENT_TOKEN is required for first enrollment")
	}
	response, err := fleet.EnrollWorker(ctx, nil, controllerURL, fleet.EnrollRequest{Name: name, Token: enrollmentToken})
	if err != nil {
		return identity{}, fmt.Errorf("enroll worker: %w", err)
	}
	stored := identity{NodeID: response.Node.ID, Token: response.Token, Name: response.Node.Name}
	encoded, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return identity{}, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return identity{}, err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return identity{}, fmt.Errorf("store worker identity: %w", err)
	}
	return stored, nil
}
