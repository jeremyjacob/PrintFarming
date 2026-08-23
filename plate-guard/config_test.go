package main

import "testing"

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("BAMBUDDY_URL", "http://bambuddy:8000/")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("AUTO_ENABLE_PLATE_CLEAR", "")

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
