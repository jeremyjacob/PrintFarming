package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type config struct {
	ListenAddr               string
	BambuddyURL              string
	BambuddyAPIKey           string
	OpenAIAPIKey             string
	OpenAIBaseURL            string
	OpenAIModel              string
	OpenAIImageDetail        string
	WebhookSecret            string
	SnapshotDelay            time.Duration
	EmptyConfidenceThreshold float64
	FirstLayerFailThreshold  float64
	EventMaxAge              time.Duration
	BambuddyTimezone         *time.Location
	WorkerCount              int
	ShutdownTimeout          time.Duration
	AutoEnablePlateClear     bool
	DryRun                   bool
	BambuddyTimeout          time.Duration
	OpenAITimeout            time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:               envOrDefault("LISTEN_ADDR", "127.0.0.1:8787"),
		BambuddyURL:              strings.TrimRight(os.Getenv("BAMBUDDY_URL"), "/"),
		BambuddyAPIKey:           os.Getenv("BAMBUDDY_API_KEY"),
		OpenAIAPIKey:             os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:            strings.TrimRight(envOrDefault("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		OpenAIModel:              envOrDefault("OPENAI_MODEL", "gpt-5.6-terra"),
		OpenAIImageDetail:        envOrDefault("OPENAI_IMAGE_DETAIL", "high"),
		WebhookSecret:            os.Getenv("WEBHOOK_SECRET"),
		SnapshotDelay:            5 * time.Second,
		EmptyConfidenceThreshold: 0.95,
		FirstLayerFailThreshold:  0.99,
		EventMaxAge:              5 * time.Minute,
		WorkerCount:              4,
		ShutdownTimeout:          5 * time.Minute,
		AutoEnablePlateClear:     true,
		BambuddyTimeout:          15 * time.Second,
		OpenAITimeout:            60 * time.Second,
	}

	var err error
	if cfg.SnapshotDelay, err = envDuration("SNAPSHOT_DELAY", cfg.SnapshotDelay); err != nil {
		return config{}, err
	}
	if cfg.BambuddyTimeout, err = envDuration("BAMBUDDY_TIMEOUT", cfg.BambuddyTimeout); err != nil {
		return config{}, err
	}
	if cfg.OpenAITimeout, err = envDuration("OPENAI_TIMEOUT", cfg.OpenAITimeout); err != nil {
		return config{}, err
	}
	if cfg.EventMaxAge, err = envDuration("EVENT_MAX_AGE", cfg.EventMaxAge); err != nil {
		return config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return config{}, err
	}
	if cfg.WorkerCount, err = envInt("WORKER_COUNT", cfg.WorkerCount); err != nil {
		return config{}, err
	}
	if cfg.EmptyConfidenceThreshold, err = envFloat("EMPTY_CONFIDENCE_THRESHOLD", cfg.EmptyConfidenceThreshold); err != nil {
		return config{}, err
	}
	if cfg.FirstLayerFailThreshold, err = envFloat("FIRST_LAYER_FAILURE_THRESHOLD", cfg.FirstLayerFailThreshold); err != nil {
		return config{}, err
	}
	if cfg.AutoEnablePlateClear, err = envBool("AUTO_ENABLE_PLATE_CLEAR", cfg.AutoEnablePlateClear); err != nil {
		return config{}, err
	}
	if cfg.DryRun, err = envBool("DRY_RUN", false); err != nil {
		return config{}, err
	}

	if cfg.BambuddyURL == "" {
		return config{}, fmt.Errorf("BAMBUDDY_URL is required")
	}
	if cfg.OpenAIAPIKey == "" {
		return config{}, fmt.Errorf("OPENAI_API_KEY is required")
	}
	if cfg.WebhookSecret == "" {
		return config{}, fmt.Errorf("WEBHOOK_SECRET is required")
	}
	if math.IsNaN(cfg.EmptyConfidenceThreshold) || math.IsInf(cfg.EmptyConfidenceThreshold, 0) || cfg.EmptyConfidenceThreshold < 0 || cfg.EmptyConfidenceThreshold > 1 {
		return config{}, fmt.Errorf("EMPTY_CONFIDENCE_THRESHOLD must be between 0 and 1")
	}
	if math.IsNaN(cfg.FirstLayerFailThreshold) || math.IsInf(cfg.FirstLayerFailThreshold, 0) || cfg.FirstLayerFailThreshold < 0.99 || cfg.FirstLayerFailThreshold > 1 {
		return config{}, fmt.Errorf("FIRST_LAYER_FAILURE_THRESHOLD must be between 0.99 and 1")
	}
	if cfg.SnapshotDelay < 0 {
		return config{}, fmt.Errorf("SNAPSHOT_DELAY cannot be negative")
	}
	if cfg.BambuddyTimeout <= 0 {
		return config{}, fmt.Errorf("BAMBUDDY_TIMEOUT must be greater than zero")
	}
	if cfg.OpenAITimeout <= 0 {
		return config{}, fmt.Errorf("OPENAI_TIMEOUT must be greater than zero")
	}
	if cfg.EventMaxAge <= 0 {
		return config{}, fmt.Errorf("EVENT_MAX_AGE must be greater than zero")
	}
	if cfg.ShutdownTimeout <= 0 || cfg.ShutdownTimeout > 5*time.Minute {
		return config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be greater than zero and at most 5m")
	}
	if cfg.WorkerCount < 1 || cfg.WorkerCount > 32 {
		return config{}, fmt.Errorf("WORKER_COUNT must be between 1 and 32")
	}
	switch cfg.OpenAIImageDetail {
	case "low", "high", "auto":
	default:
		return config{}, fmt.Errorf("OPENAI_IMAGE_DETAIL must be low, high, or auto")
	}
	cfg.BambuddyTimezone, err = time.LoadLocation(envOrDefault("BAMBUDDY_TIMEZONE", "UTC"))
	if err != nil {
		return config{}, fmt.Errorf("BAMBUDDY_TIMEZONE is invalid: %w", err)
	}

	return cfg, nil
}

func envInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 5s: %w", key, err)
	}
	return parsed, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}
	return parsed, nil
}

func envBool(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", key, err)
	}
	return parsed, nil
}
