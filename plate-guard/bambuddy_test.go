package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBambuddyEnsureGateAndClearPlate(t *testing.T) {
	var mu sync.Mutex
	requirePlateClear := false
	clearCalled := false
	pauseCalled := false
	amsBackupCalled := false
	var fanCalls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "bambuddy-key" {
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(bambuddySettings{RequirePlateClear: requirePlateClear, CaptureFinishPhoto: true})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/settings":
			mu.Lock()
			requirePlateClear = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(bambuddySettings{RequirePlateClear: true, CaptureFinishPhoto: true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/printers/7/clear-plate":
			mu.Lock()
			clearCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/printers/7/print/pause":
			mu.Lock()
			pauseCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/printers/7/ams-backup":
			if r.URL.Query().Get("enabled") != "true" {
				http.Error(w, "missing enabled=true", http.StatusBadRequest)
				return
			}
			mu.Lock()
			amsBackupCalled = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/printers/7/fan-speed":
			mu.Lock()
			fanCalls = append(fanCalls, r.URL.Query().Get("fan")+"="+r.URL.Query().Get("speed"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/printers/7/status":
			_ = json.NewEncoder(w).Encode(plateGateStatus{
				ID:                 7,
				Name:               "P1S",
				Connected:          true,
				AwaitingPlateClear: true,
				State:              "FINISH",
				SubtaskName:        "part",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/printers/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "model": "P1S"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/printers/camera/stream-token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "camera-token"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/printers/7/camera/snapshot":
			if r.URL.Query().Get("token") != "camera-token" {
				http.Error(w, "missing camera token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xdb})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queue/":
			if r.URL.Query().Get("printer_id") != "7" {
				http.Error(w, "missing queue filters", http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("status") == "printing" {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 45, "printer_id": 7, "status": "printing", "started_at": "2026-08-22T13:30:00Z"},
				})
				return
			}
			if r.URL.Query().Has("status") {
				http.Error(w, "unexpected queue status", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 40, "printer_id": 7, "status": "completed", "completed_at": "2026-08-22T10:00:00Z"},
				{"id": 42, "printer_id": 7, "status": "completed", "completed_at": "2026-08-22T12:00:00Z", "bed_type": "Textured PEI Plate"},
				{"id": 43, "printer_id": 7, "status": "failed", "completed_at": "2026-08-22T13:00:00Z", "bed_type": "Textured PEI Plate"},
				{"id": 44, "printer_id": 7, "status": "pending", "completed_at": nil},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newBambuddyClient(server.URL, "bambuddy-key", time.UTC, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	settings, err := client.ensurePlateClearGate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.RequirePlateClear {
		t.Fatal("plate-clear gate was not enabled")
	}
	status, err := client.gateStatus(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Connected || !status.AwaitingPlateClear || status.SubtaskName != "part" {
		t.Fatalf("unexpected gate status: %+v", status)
	}
	model, err := client.printerModel(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if model != "P1S" {
		t.Fatalf("unexpected printer model: %q", model)
	}
	terminal, err := client.latestTerminalJob(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.ID != 43 || terminal.Status != "failed" || !terminal.CompletedAt.Equal(time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected terminal job: %+v", terminal)
	}
	active, err := client.activeQueueJob(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != 45 || active.Status != "printing" || !active.StartedAt.Equal(time.Date(2026, 8, 22, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("unexpected active job: %+v", active)
	}
	image, mediaType, err := client.snapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(image) == 0 || mediaType != "image/jpeg" {
		t.Fatalf("unexpected snapshot: bytes=%d media_type=%q", len(image), mediaType)
	}
	if err := client.clearPlate(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := client.pausePrint(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := client.enableAMSFilamentBackup(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := client.setFanSpeed(context.Background(), 7, "aux2", 100); err != nil {
		t.Fatal(err)
	}
	if err := client.setFanSpeed(context.Background(), 7, "aux2", 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !clearCalled {
		t.Fatal("clear-plate endpoint was not called")
	}
	if !pauseCalled {
		t.Fatal("pause endpoint was not called")
	}
	if !amsBackupCalled {
		t.Fatal("AMS backup endpoint was not called")
	}
	if len(fanCalls) != 2 || fanCalls[0] != "aux2=100" || fanCalls[1] != "aux2=0" {
		t.Fatalf("unexpected fan calls: %v", fanCalls)
	}
}

func TestParseBambuddyTimeUsesConfiguredTimezone(t *testing.T) {
	location := time.FixedZone("test", -7*60*60)
	parsed, err := parseBambuddyTime("2026-08-22T12:30:00", location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Location() != location || parsed.Hour() != 12 {
		t.Fatalf("unexpected timestamp: %s", parsed)
	}
}
