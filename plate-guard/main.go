package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := log.New(os.Stdout, "", log.Ldate|log.Ltime|log.LUTC)
	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}

	bambuddyHTTP := &http.Client{Timeout: cfg.BambuddyTimeout}
	bambuddy, err := newBambuddyClient(cfg.BambuddyURL, cfg.BambuddyAPIKey, cfg.BambuddyTimezone, bambuddyHTTP)
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), cfg.BambuddyTimeout)
	settings, err := bambuddy.ensurePlateClearGate(startupCtx, cfg.AutoEnablePlateClear)
	startupCancel()
	if err != nil {
		logger.Fatalf("safety preflight failed: %v; enable Require plate clear in Bambuddy before starting this service", err)
	}
	logger.Printf("safety preflight passed require_plate_clear=%t", settings.RequirePlateClear)

	openAI := &openAIClient{
		apiKey:      cfg.OpenAIAPIKey,
		baseURL:     cfg.OpenAIBaseURL,
		model:       cfg.OpenAIModel,
		imageDetail: cfg.OpenAIImageDetail,
		httpClient:  &http.Client{Timeout: cfg.OpenAITimeout},
	}
	plateService := newService(cfg, bambuddy, openAI, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plateService.start(context.Background())

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           plateService.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Printf("starting bambuddy-plate-guard version=%s listen=%s model=%s workers=%d dry_run=%t", version, cfg.ListenAddr, cfg.OpenAIModel, cfg.WorkerCount, cfg.DryRun)
		serverErrors <- server.ListenAndServe()
	}()

	var serverErr error
	select {
	case <-ctx.Done():
		logger.Printf("shutdown requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serverErr = err
		}
	}

	plateService.stopAccepting()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("HTTP shutdown error: %v", err)
	}
	cancel()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	if !plateService.shutdown(drainCtx) {
		logger.Printf("background queue drain timed out; remaining plate gates stay closed")
	}
	drainCancel()
	if serverErr != nil {
		logger.Fatalf("HTTP server failed: %v", serverErr)
	}
}
