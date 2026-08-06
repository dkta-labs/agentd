package main

import (
	"context"
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

	"github.com/dkta-labs/agentd/internal/githubhook"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("agentd GitHub hook stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("agentd-github-hook", flag.ContinueOnError)
	configPath := flags.String("config", "", "webhook adapter JSON configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := githubhook.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	secret, err := githubhook.LoadSecret(cfg.SecretFile)
	if err != nil {
		return err
	}
	defer clear(secret)
	store, err := githubhook.OpenReceiptStore(context.Background(), cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	dispatcher, err := githubhook.NewAgentdDispatcher(cfg.AgentdURL, nil)
	if err != nil {
		return err
	}
	service, err := githubhook.NewServer(cfg.Rules, secret, store, dispatcher)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	server := &http.Server{
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errChannel := make(chan error, 1)
	go func() {
		errChannel <- server.Serve(listener)
	}()
	slog.Info("agentd GitHub hook listening", "address", cfg.Listen, "rules", len(cfg.Rules))
	select {
	case <-ctx.Done():
	case err := <-errChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	serveErr := <-errChannel
	if !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
