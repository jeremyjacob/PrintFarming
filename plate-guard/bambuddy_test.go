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
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/printers/7/status":
			_ = json.NewEncoder(w).Encode(plateGateStatus{
				ID:                 7,
				Name:               "P1S",
				AwaitingPlateClear: true,
				State:              "FINISH",
				SubtaskName:        "part",
			})
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
			if r.URL.Query().Get("printer_id") != "7" || r.URL.Query().Get("status") != "completed" {
				http.Error(w, "missing queue filters", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 40, "printer_id": 7, "completed_at": "2026-08-22T10:00:00Z"},
				{"id": 42, "printer_id": 7, "completed_at": "2026-08-22T12:00:00Z", "bed_type": "Textured PEI Plate"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/printers/7/camera/check-plate":
			if r.URL.Query().Get("plate_type") != "Textured PEI Plate" {
				http.Error(w, "missing plate type", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(localPlateAssessment{
				IsEmpty:    true,
				Confidence: 0.99,
				Difference: 0.1,
				Message:    "Plate appears empty",
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
	if !status.AwaitingPlateClear || status.SubtaskName != "part" {
		t.Fatalf("unexpected gate status: %+v", status)
	}
	completion, err := client.latestCompletion(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if completion.ID != 42 || completion.PlateType != "Textured PEI Plate" || !completion.CompletedAt.Equal(time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected completion: %+v", completion)
	}
	image, mediaType, err := client.snapshot(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(image) == 0 || mediaType != "image/jpeg" {
		t.Fatalf("unexpected snapshot: bytes=%d media_type=%q", len(image), mediaType)
	}
	localAssessment, err := client.checkPlate(context.Background(), 7, completion.PlateType)
	if err != nil {
		t.Fatal(err)
	}
	if !localAssessment.IsEmpty || localAssessment.NeedsCalibration || localAssessment.LightWarning {
		t.Fatalf("unexpected local assessment: %+v", localAssessment)
	}
	if err := client.clearPlate(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !clearCalled {
		t.Fatal("clear-plate endpoint was not called")
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
