package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/devices"
	"github.com/dkta-labs/agentd/internal/events"
	"github.com/dkta-labs/agentd/internal/fleet"
	"github.com/dkta-labs/agentd/internal/herdr"
	"github.com/dkta-labs/agentd/internal/httpapi"
	"github.com/dkta-labs/agentd/internal/mcp"
	"github.com/dkta-labs/agentd/internal/omp"
	"github.com/dkta-labs/agentd/internal/runtime"
	"github.com/dkta-labs/agentd/internal/sessions"
	"github.com/dkta-labs/agentd/internal/store"
	"github.com/dkta-labs/agentd/internal/workspaces"
)

func main() {
	if err := run(); err != nil {
		slog.Error("agentd stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to agentd JSON configuration")
	generateEnrollmentToken := flag.Bool("generate-enrollment-token", false, "print a new one-time device enrollment token")
	generateVAPIDKeys := flag.Bool("generate-vapid-keys", false, "print a new Web Push VAPID key pair as JSON")
	pair := flag.Bool("pair", false, "print a one-time Android pairing QR code")
	pairingQR := flag.Bool("pairing-qr", false, "print a one-time Android pairing QR code")
	flag.Parse()
	pairingRequested := *pair || *pairingQR
	selectedModes := 0
	for _, selected := range []bool{*generateEnrollmentToken, *generateVAPIDKeys, pairingRequested} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 {
		return errors.New("choose only one generation or pairing option")
	}
	if *generateEnrollmentToken {
		token, err := devices.GenerateEnrollmentToken()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, token)
		return err
	}
	if *generateVAPIDKeys {
		publicKey, privateKey, err := devices.GenerateVAPIDKeys()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"publicKey":  publicKey,
			"privateKey": privateKey,
		})
	}

	resolvedConfigPath := *configPath
	if pairingRequested {
		var err error
		resolvedConfigPath, err = resolvePairingConfigPath(resolvedConfigPath)
		if err != nil {
			return err
		}
	}

	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		return err
	}
	if pairingRequested {
		token, err := enrollmentTokenForPairing(resolvedConfigPath, os.Getenv("AGENTD_ENROLLMENT_TOKEN"))
		if err != nil {
			return err
		}
		if err := ensurePairingTokenUnused(context.Background(), cfg.DataDir, token); err != nil {
			return err
		}
		return printPairingQR(os.Stdout, cfg.PublicURL, token)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer database.Close()
	deviceService, err := devices.New(database, devices.Options{
		Enabled:         cfg.Auth.Enabled,
		CookieSecure:    cfg.Auth.CookieSecure,
		EnrollmentToken: os.Getenv("AGENTD_ENROLLMENT_TOKEN"),
		PushEnabled:     cfg.Push.Enabled,
		VAPIDPublicKey:  os.Getenv("AGENTD_VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey: os.Getenv("AGENTD_VAPID_PRIVATE_KEY"),
		VAPIDSubject:    cfg.Push.Subject,
		Logger:          logger,
	})
	if err != nil {
		return err
	}
	defer deviceService.Close()
	attachments := herdr.NewController(cfg.HerdrBinary, logger)
	catalog := workspaces.NewCatalog(cfg, logger, attachments)
	statusMonitor := workspaces.NewHerdrStatusMonitor(catalog, deviceService, logger)
	go statusMonitor.Run(ctx, 3*time.Second)
	broker := events.NewBroker(database)
	fleetService := fleet.NewService(database, fleet.Options{
		EnrollmentToken: os.Getenv("AGENTD_NODE_ENROLLMENT_TOKEN"),
		Logger:          logger,
	})
	harnesses, err := runtime.NewRegistry("omp", runtime.Registration{
		Descriptor: runtime.Descriptor{
			ID:   "omp",
			Name: "OMP",
			Capabilities: []runtime.Capability{
				runtime.CapabilityPrompt,
				runtime.CapabilitySteer,
				runtime.CapabilityFollowUp,
				runtime.CapabilityAbort,
				runtime.CapabilityInteractions,
				runtime.CapabilityToolEvents,
			},
		},
		Driver:    fleet.Driver{Local: omp.Driver{}, Service: fleetService},
		Normalize: omp.NormalizeEvent,
	})
	if err != nil {
		return err
	}
	sessionManager, err := sessions.NewManager(ctx, harnesses, broker, database, catalog, attachments, deviceService)
	if err != nil {
		return err
	}
	mcpServer := mcp.New(catalog, sessionManager, broker)
	app, err := httpapi.NewWithFleet(catalog, logger, sessionManager, broker, mcpServer, fleetService, deviceService)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr: cfg.Listen,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownComplete := make(chan struct{})
	go func() {
		defer close(shutdownComplete)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sessionManager.Close(shutdownCtx); err != nil {
			logger.Error("session shutdown failed", "error", err)
		}
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("agentd listening", "address", cfg.Listen)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownComplete
		return nil
	}
	return err
}
