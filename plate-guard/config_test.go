package main

import (
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("BAMBUDDY_URL", "http://bambuddy:8000/")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("AUTO_ENABLE_PLATE_CLEAR", "")
	t.Setenv("ENABLE_AMS_BACKUP_AFTER_FIRST_LAYER", "")
	t.Setenv("POST_PRINT_FAN_DURATION", "")
	t.Setenv("POST_PRINT_FAN_SPEED", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BambuddyURL != "http://bambuddy:8000" {
		t.Fatalf("unexpected Bambuddy URL: %q", cfg.BambuddyURL)
	}
	if cfg.ListenAddr != "127.0.0.1:8787" {
		t.Fatalf("unexpected listen address: %q", cfg.ListenAddr)
	}
	if cfg.OpenAIModel != "gpt-5.6-sol" {
		t.Fatalf("unexpected OpenAI model: %q", cfg.OpenAIModel)
	}
	if !cfg.AutoEnablePlateClear {
		t.Fatal("auto-enable should default to true")
	}
	if !cfg.EnableAMSBackup {
		t.Fatal("AMS backup after first-layer review should default to true")
	}
	if cfg.PostPrintFanDuration != 5*time.Minute || cfg.PostPrintFanSpeed != 100 {
		t.Fatalf("unexpected post-print fan defaults: duration=%s speed=%d", cfg.PostPrintFanDuration, cfg.PostPrintFanSpeed)
	}
}

func TestLoadConfigRejectsInvalidPostPrintFanSettings(t *testing.T) {
	t.Setenv("BAMBUDDY_URL", "http://bambuddy:8000")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("POST_PRINT_FAN_DURATION", "5m")
	t.Setenv("POST_PRINT_FAN_SPEED", "0")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected zero fan speed to fail when the fan cycle is enabled")
	}
}

func TestLoadConfigRejectsZeroTimeout(t *testing.T) {
	t.Setenv("BAMBUDDY_URL", "http://bambuddy:8000")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("OPENAI_TIMEOUT", "0s")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected zero OpenAI timeout to fail")
	}
}

func TestLoadConfigRejectsExcessiveShutdownTimeout(t *testing.T) {
	t.Setenv("BAMBUDDY_URL", "http://bambuddy:8000")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("SHUTDOWN_TIMEOUT", "6m")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected shutdown timeout above systemd limit to fail")
	}
}
