package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeController struct {
	mu               sync.Mutex
	cleared          chan int
	gateStatuses     []plateGateStatus
	gateCalls        int
	completions      []completionRecord
	completionCalls  int
	plateClearActive bool
	snapshotCalls    int
	localAssessment  localPlateAssessment
}

func (f *fakeController) snapshot(context.Context, int) ([]byte, string, error) {
	f.mu.Lock()
	f.snapshotCalls++
	f.mu.Unlock()
	return []byte{0xff, 0xd8, 0xff, 0xdb}, "image/jpeg", nil
}

func (f *fakeController) gateStatus(context.Context, int) (plateGateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gateStatuses) == 0 {
		return plateGateStatus{}, nil
	}
	index := f.gateCalls
	if index >= len(f.gateStatuses) {
		index = len(f.gateStatuses) - 1
	}
	f.gateCalls++
	return f.gateStatuses[index], nil
}

func (f *fakeController) latestCompletion(context.Context, int) (completionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.completions) == 0 {
		return completionRecord{}, nil
	}
	index := f.completionCalls
	if index >= len(f.completions) {
		index = len(f.completions) - 1
	}
	f.completionCalls++
	return f.completions[index], nil
}

func (f *fakeController) plateClearEnabled(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plateClearActive, nil
}

func (f *fakeController) checkPlate(context.Context, int, string) (localPlateAssessment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localAssessment, nil
}

func (f *fakeController) clearPlate(_ context.Context, printerID int) error {
	f.cleared <- printerID
	return nil
}

type fakeAssessor struct {
	assessment plateAssessment
}

func (f *fakeAssessor) assess(context.Context, []byte, string) (plateAssessment, error) {
	return f.assessment, nil
}

func testConfig() config {
	return config{
		WebhookSecret:            "webhook-secret",
		SnapshotDelay:            0,
		EmptyConfidenceThreshold: 0.95,
		EventMaxAge:              5 * time.Minute,
		BambuddyTimezone:         time.UTC,
		WorkerCount:              1,
	}
}

func testEvent(now time.Time) webhookEvent {
	return webhookEvent{
		Event:     "print_complete",
		Printer:   "P1S",
		Filename:  "part.3mf",
		Timestamp: now.Format(time.RFC3339Nano),
	}
}

func TestWebhookClearsOnlyConfidentEmptyPlate(t *testing.T) {
	now := time.Now()
	gate := plateGateStatus{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"}
	completion := completionRecord{ID: 42, CompletedAt: now.Add(-time.Second)}
	controller := &fakeController{
		cleared:          make(chan int, 1),
		gateStatuses:     []plateGateStatus{gate, gate},
		completions:      []completionRecord{completion, completion},
		plateClearActive: true,
		localAssessment:  localPlateAssessment{IsEmpty: true, Confidence: 0.99, Message: "clear"},
	}
	assessor := &fakeAssessor{assessment: plateAssessment{
		PlateVisible: true,
		IsEmpty:      true,
		Confidence:   0.99,
		Reason:       "clear",
	}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})

	payload, err := json.Marshal(testEvent(now))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/bambuddy/7", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer webhook-secret")
	recorder := httptest.NewRecorder()
	svc.handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	select {
	case printerID := <-controller.cleared:
		if printerID != 7 {
			t.Fatalf("unexpected printer ID: %d", printerID)
		}
	case <-time.After(time.Second):
		t.Fatal("plate gate was not cleared")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 2 {
		t.Fatalf("expected two fresh snapshots, got %d", controller.snapshotCalls)
	}
}

func TestWebhookRejectsInvalidSecret(t *testing.T) {
	svc := newService(testConfig(), &fakeController{cleared: make(chan int, 1)}, &fakeAssessor{}, log.New(io.Discard, "", 0))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/bambuddy/7", bytes.NewReader([]byte(`{"event":"print_complete"}`)))
	req.Header.Set("Authorization", "Bearer incorrect")
	recorder := httptest.NewRecorder()
	svc.handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
}

func TestWebhookRejectsStaleTimestamp(t *testing.T) {
	svc := newService(testConfig(), &fakeController{cleared: make(chan int, 1)}, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	event := testEvent(time.Now().Add(-10 * time.Minute))
	payload, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/bambuddy/7", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer webhook-secret")
	recorder := httptest.NewRecorder()
	svc.handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
}

func TestLowConfidenceLeavesPlateGated(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		cleared: make(chan int, 1),
		gateStatuses: []plateGateStatus{{
			ID:                 7,
			Name:               "P1S",
			AwaitingPlateClear: true,
			State:              "FINISH",
			SubtaskName:        "part.3mf",
		}},
		completions: []completionRecord{{ID: 42, CompletedAt: now.Add(-time.Second)}},
	}
	assessor := &fakeAssessor{assessment: plateAssessment{
		PlateVisible: true,
		IsEmpty:      true,
		Confidence:   0.80,
		Reason:       "possibly clear",
	}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	select {
	case <-controller.cleared:
		t.Fatal("low-confidence assessment must not clear the gate")
	default:
	}
}

func TestChangedGateIsNotCleared(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		cleared: make(chan int, 1),
		gateStatuses: []plateGateStatus{
			{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"},
			{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "next-part.3mf"},
		},
		completions:      []completionRecord{{ID: 42, CompletedAt: now.Add(-time.Second)}},
		plateClearActive: true,
		localAssessment:  localPlateAssessment{IsEmpty: true, Confidence: 0.99, Message: "clear"},
	}
	assessor := &fakeAssessor{assessment: plateAssessment{
		PlateVisible: true,
		IsEmpty:      true,
		Confidence:   0.99,
		Reason:       "clear",
	}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	select {
	case <-controller.cleared:
		t.Fatal("a changed plate gate must not be cleared")
	default:
	}
}

func TestPrinterMismatchIsNotCleared(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		cleared: make(chan int, 1),
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "Other P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf",
		}},
	}
	svc := newService(testConfig(), controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	select {
	case <-controller.cleared:
		t.Fatal("mismatched printer must not be cleared")
	default:
	}
}

func TestBambuddyLocalCheckCanVetoRelease(t *testing.T) {
	now := time.Now()
	gate := plateGateStatus{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"}
	completion := completionRecord{ID: 42, CompletedAt: now.Add(-time.Second), PlateType: "Textured PEI Plate"}
	controller := &fakeController{
		cleared:          make(chan int, 1),
		gateStatuses:     []plateGateStatus{gate, gate},
		completions:      []completionRecord{completion, completion},
		plateClearActive: true,
		localAssessment: localPlateAssessment{
			IsEmpty:    false,
			Confidence: 0.99,
			Message:    "Objects detected",
		},
	}
	assessor := &fakeAssessor{assessment: plateAssessment{
		PlateVisible: true,
		IsEmpty:      true,
		Confidence:   0.99,
		Reason:       "clear",
	}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	select {
	case <-controller.cleared:
		t.Fatal("Bambuddy's occupied result must veto the OpenAI assessments")
	default:
	}
}

func TestReadinessRequiresActiveGateAndAcceptingWorker(t *testing.T) {
	controller := &fakeController{plateClearActive: true, cleared: make(chan int, 1)}
	svc := newService(testConfig(), controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())

	recorder := httptest.NewRecorder()
	svc.handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected ready status: %d", recorder.Code)
	}

	svc.stopAccepting()
	recorder = httptest.NewRecorder()
	svc.handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected stopping status: %d", recorder.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !svc.shutdown(ctx) {
		t.Fatal("service did not shut down cleanly")
	}
}

func TestGateMatchesBambuddyNormalizedFilename(t *testing.T) {
	tests := []struct {
		status string
		event  string
	}{
		{status: "Framework_LL_A", event: "Framework LL A"},
		{status: "/data/Metadata/plate_5.gcode", event: "plate_5"},
	}
	for _, test := range tests {
		status := plateGateStatus{GcodeFile: test.status}
		if !status.matchesEvent(webhookEvent{Filename: test.event}) {
			t.Fatalf("expected %q and %q to match", test.status, test.event)
		}
	}
}

func TestCompletionMustMatchWebhookTimestamp(t *testing.T) {
	now := time.Now()
	completion := completionRecord{ID: 42, CompletedAt: now}
	if completionMatchesEvent(completion, now.Add(-time.Minute), 5*time.Minute) {
		t.Fatal("a webhook older than the queue completion must not match")
	}
	if !completionMatchesEvent(completion, now.Add(time.Second), 5*time.Minute) {
		t.Fatal("a webhook emitted after the queue completion should match")
	}
}
