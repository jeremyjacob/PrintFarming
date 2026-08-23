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
	paused           chan int
	gateStatuses     []plateGateStatus
	gateCalls        int
	terminalJobs     []terminalJob
	terminalCalls    int
	activeJobs       []activeQueueJob
	activeCalls      int
	plateClearActive bool
	snapshotCalls    int
	staticSnapshots  bool
}

func (f *fakeController) snapshot(context.Context, int) ([]byte, string, error) {
	f.mu.Lock()
	f.snapshotCalls++
	call := f.snapshotCalls
	if f.staticSnapshots {
		call = 0
	}
	f.mu.Unlock()
	return []byte{0xff, 0xd8, 0xff, 0xdb, byte(call)}, "image/jpeg", nil
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

func (f *fakeController) latestTerminalJob(context.Context, int) (terminalJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.terminalJobs) == 0 {
		return terminalJob{}, nil
	}
	index := f.terminalCalls
	if index >= len(f.terminalJobs) {
		index = len(f.terminalJobs) - 1
	}
	f.terminalCalls++
	return f.terminalJobs[index], nil
}

func (f *fakeController) activeQueueJob(context.Context, int) (activeQueueJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.activeJobs) == 0 {
		return activeQueueJob{}, nil
	}
	index := f.activeCalls
	if index >= len(f.activeJobs) {
		index = len(f.activeJobs) - 1
	}
	f.activeCalls++
	return f.activeJobs[index], nil
}

func (f *fakeController) plateClearEnabled(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plateClearActive, nil
}

func (f *fakeController) clearPlate(_ context.Context, printerID int) error {
	f.cleared <- printerID
	return nil
}

func (f *fakeController) pausePrint(_ context.Context, printerID int) error {
	f.paused <- printerID
	return nil
}

type fakeAssessor struct {
	mu                    sync.Mutex
	assessment            plateAssessment
	firstLayerAssessments []firstLayerAssessment
	firstLayerCalls       int
}

func (f *fakeAssessor) assess(context.Context, []byte, string) (plateAssessment, error) {
	return f.assessment, nil
}

func (f *fakeAssessor) assessFirstLayer(context.Context, []byte, string) (firstLayerAssessment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.firstLayerAssessments) == 0 {
		return firstLayerAssessment{}, nil
	}
	index := f.firstLayerCalls
	if index >= len(f.firstLayerAssessments) {
		index = len(f.firstLayerAssessments) - 1
	}
	f.firstLayerCalls++
	return f.firstLayerAssessments[index], nil
}

func testConfig() config {
	return config{
		WebhookSecret:            "webhook-secret",
		SnapshotDelay:            0,
		EmptyConfidenceThreshold: 0.95,
		FirstLayerFailThreshold:  0.99,
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

func firstLayerEvent(now time.Time) webhookEvent {
	return webhookEvent{
		Event:     "first_layer_complete",
		Printer:   "P1S",
		Filename:  "part.3mf",
		Timestamp: now.Format(time.RFC3339Nano),
	}
}

func testActiveJob(now time.Time) activeQueueJob {
	return activeQueueJob{ID: 99, StartedAt: now.Add(-time.Minute), Status: "printing"}
}

func TestWebhookClearsOnlyConfidentEmptyPlate(t *testing.T) {
	now := time.Now()
	gate := plateGateStatus{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	controller := &fakeController{
		cleared:          make(chan int, 1),
		gateStatuses:     []plateGateStatus{gate, gate},
		terminalJobs:     []terminalJob{terminal, terminal},
		plateClearActive: true,
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

func TestFirstLayerWebhookPausesOnlyAfterTwoCertainFailures(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2},
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 3},
		},
	}
	certainFailure := firstLayerAssessment{
		FirstLayerVisible: true,
		IsDefective:       true,
		Confidence:        0.995,
		Reason:            "Loose extrusion is visibly detached and tangled.",
	}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{certainFailure, certainFailure}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})

	payload, err := json.Marshal(firstLayerEvent(now))
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
	case printerID := <-controller.paused:
		if printerID != 7 {
			t.Fatalf("unexpected printer ID: %d", printerID)
		}
	case <-time.After(time.Second):
		t.Fatal("certain first-layer failure was not paused")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 2 || controller.gateCalls != 2 {
		t.Fatalf("expected two snapshots and two status checks, got snapshots=%d status=%d", controller.snapshotCalls, controller.gateCalls)
	}
}

func TestFirstLayerAmbiguityDoesNotPause(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2,
		}},
	}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{{
		FirstLayerVisible: false,
		IsDefective:       true,
		Confidence:        0.999,
		Reason:            "The deposited layer is obscured by glare.",
	}}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("an unassessable first layer must not pause")
	default:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 1 || controller.gateCalls != 1 {
		t.Fatalf("ambiguous result should stop after one assessment, got snapshots=%d status=%d", controller.snapshotCalls, controller.gateCalls)
	}
}

func TestFirstLayerFailureRequiresIndependentConfirmation(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2,
		}},
	}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{
		{FirstLayerVisible: true, IsDefective: true, Confidence: 0.999, Reason: "Possible detached filament."},
		{FirstLayerVisible: true, IsDefective: false, Confidence: 0.999, Reason: "The extrusion appears bonded."},
	}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("a disputed first-layer failure must not pause")
	default:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 2 || controller.gateCalls != 1 {
		t.Fatalf("expected two assessments without final action check, got snapshots=%d status=%d", controller.snapshotCalls, controller.gateCalls)
	}
}

func TestFirstLayerFailureRequiresDistinctSnapshots(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:          make(chan int, 1),
		staticSnapshots: true,
		activeJobs:      []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2,
		}},
	}
	failure := firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 1, Reason: "Certain detached extrusion."}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{failure, failure}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("identical snapshots must not trigger a pause")
	default:
	}
	assessor.mu.Lock()
	defer assessor.mu.Unlock()
	if assessor.firstLayerCalls != 1 {
		t.Fatalf("identical confirmation image should not be reassessed, got %d model calls", assessor.firstLayerCalls)
	}
}

func TestFirstLayerDryRunRevalidatesWithoutPausing(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2},
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 4},
		},
	}
	failure := firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 1, Reason: "Certain detached extrusion."}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{failure, failure}}
	cfg := testConfig()
	cfg.DryRun = true
	svc := newService(cfg, controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("dry run must not pause a print")
	default:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 2 || controller.gateCalls != 2 {
		t.Fatalf("dry run skipped first-layer checks: snapshots=%d status=%d", controller.snapshotCalls, controller.gateCalls)
	}
}

func TestFirstLayerFailureDoesNotPauseAReplacementPrint(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2},
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "replacement.3mf", SubtaskName: "replacement.3mf", LayerNum: 2},
		},
	}
	failure := firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 1, Reason: "Certain detached extrusion."}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{failure, failure}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("a replacement print must not be paused for an earlier event")
	default:
	}
}

func TestFirstLayerFailureDoesNotPauseSameNameReplacementJob(t *testing.T) {
	now := time.Now()
	initialJob := testActiveJob(now)
	replacementJob := activeQueueJob{ID: initialJob.ID + 1, StartedAt: now.Add(time.Second), Status: "printing"}
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{initialJob, replacementJob},
		gateStatuses: []plateGateStatus{
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2},
			{ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2},
		},
	}
	failure := firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 1, Reason: "Certain detached extrusion."}
	assessor := &fakeAssessor{firstLayerAssessments: []firstLayerAssessment{failure, failure}}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("a same-name replacement queue job must not be paused for an earlier event")
	default:
	}
}

func TestFirstLayerDisconnectedPrinterDoesNotPause(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "P1S", Connected: false, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2,
		}},
	}
	failure := firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 1, Reason: "Certain detached extrusion."}
	svc := newService(
		testConfig(),
		controller,
		&fakeAssessor{firstLayerAssessments: []firstLayerAssessment{failure, failure}},
		log.New(io.Discard, "", 0),
	)
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now})

	select {
	case <-controller.paused:
		t.Fatal("a disconnected printer must not be paused from cached status")
	default:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 0 || controller.activeCalls != 0 {
		t.Fatal("disconnected status should stop before queue or vision checks")
	}
}

func TestCertainFirstLayerFailureRequiresEveryCondition(t *testing.T) {
	svc := newService(testConfig(), &fakeController{}, &fakeAssessor{}, log.New(io.Discard, "", 0))
	tests := []struct {
		name       string
		assessment firstLayerAssessment
		want       bool
	}{
		{name: "certain failure", assessment: firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 0.99}, want: true},
		{name: "not visible", assessment: firstLayerAssessment{FirstLayerVisible: false, IsDefective: true, Confidence: 1}},
		{name: "not defective", assessment: firstLayerAssessment{FirstLayerVisible: true, IsDefective: false, Confidence: 1}},
		{name: "below threshold", assessment: firstLayerAssessment{FirstLayerVisible: true, IsDefective: true, Confidence: 0.989}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := svc.certainFirstLayerFailure(test.assessment); got != test.want {
				t.Fatalf("certainFirstLayerFailure()=%t want %t", got, test.want)
			}
		})
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
		terminalJobs: []terminalJob{{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
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
			{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FAILED", SubtaskName: "part.3mf"},
		},
		terminalJobs:     []terminalJob{{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
		plateClearActive: true,
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

func TestDryRunExecutesAllSafetyChecks(t *testing.T) {
	now := time.Now()
	gate := plateGateStatus{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	controller := &fakeController{
		cleared:          make(chan int, 1),
		gateStatuses:     []plateGateStatus{gate, gate},
		terminalJobs:     []terminalJob{terminal, terminal},
		plateClearActive: true,
	}
	assessor := &fakeAssessor{assessment: plateAssessment{
		PlateVisible: true,
		IsEmpty:      true,
		Confidence:   0.99,
		Reason:       "clear",
	}}
	cfg := testConfig()
	cfg.DryRun = true
	svc := newService(cfg, controller, assessor, log.New(io.Discard, "", 0))
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})

	select {
	case <-controller.cleared:
		t.Fatal("dry run must not clear the gate")
	default:
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.snapshotCalls != 2 || controller.gateCalls != 2 || controller.terminalCalls != 2 {
		t.Fatalf(
			"dry run skipped checks: snapshots=%d gates=%d terminal_jobs=%d",
			controller.snapshotCalls,
			controller.gateCalls,
			controller.terminalCalls,
		)
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

func TestNewerFailedJobIsNotCleared(t *testing.T) {
	now := time.Now()
	gate := plateGateStatus{ID: 7, Name: "P1S", AwaitingPlateClear: true, State: "FINISH", SubtaskName: "part.3mf"}
	controller := &fakeController{
		cleared:      make(chan int, 1),
		gateStatuses: []plateGateStatus{gate, gate},
		terminalJobs: []terminalJob{
			{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)},
			{ID: 43, Status: "failed", CompletedAt: now.Add(time.Second)},
		},
		plateClearActive: true,
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
		t.Fatal("a newer failed terminal job must not be cleared")
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

func TestTerminalJobMustMatchWebhookTimestampAndStatus(t *testing.T) {
	now := time.Now()
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now}
	if terminalMatchesEvent(terminal, now.Add(-time.Minute), 5*time.Minute) {
		t.Fatal("a webhook older than the queue completion must not match")
	}
	if !terminalMatchesEvent(terminal, now.Add(time.Second), 5*time.Minute) {
		t.Fatal("a webhook emitted after the queue completion should match")
	}
	terminal.Status = "failed"
	if terminalMatchesEvent(terminal, now.Add(time.Second), 5*time.Minute) {
		t.Fatal("a failed terminal job must not match a completion webhook")
	}
}

type blockingFirstLayerAssessor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingFirstLayerAssessor) assess(context.Context, []byte, string) (plateAssessment, error) {
	return plateAssessment{}, nil
}

func (a *blockingFirstLayerAssessor) assessFirstLayer(context.Context, []byte, string) (firstLayerAssessment, error) {
	a.once.Do(func() { close(a.started) })
	<-a.release
	return firstLayerAssessment{}, nil
}

func TestShutdownTimeoutDoesNotWaitForStuckAssessment(t *testing.T) {
	now := time.Now()
	controller := &fakeController{
		paused:     make(chan int, 1),
		activeJobs: []activeQueueJob{testActiveJob(now)},
		gateStatuses: []plateGateStatus{{
			ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "part.3mf", SubtaskName: "part.3mf", LayerNum: 2,
		}},
	}
	assessor := &blockingFirstLayerAssessor{started: make(chan struct{}), release: make(chan struct{})}
	svc := newService(testConfig(), controller, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 7, Event: firstLayerEvent(now), EventTime: now}
	select {
	case <-assessor.started:
	case <-time.After(time.Second):
		t.Fatal("assessment did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- svc.shutdown(ctx) }()
	select {
	case drained := <-result:
		if drained {
			close(assessor.release)
			t.Fatal("stuck assessment unexpectedly drained")
		}
	case <-time.After(200 * time.Millisecond):
		close(assessor.release)
		t.Fatal("shutdown remained blocked after its context expired")
	}
	close(assessor.release)

	done := make(chan struct{})
	go func() {
		svc.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not exit after the stuck assessment was released")
	}
}

type schedulingController struct {
	now time.Time
}

func (c *schedulingController) snapshot(_ context.Context, printerID int) ([]byte, string, error) {
	return []byte{0xff, 0xd8, 0xff, 0xdb, byte(printerID)}, "image/jpeg", nil
}

func (c *schedulingController) gateStatus(_ context.Context, printerID int) (plateGateStatus, error) {
	return plateGateStatus{
		ID:                 printerID,
		Name:               "P1S",
		AwaitingPlateClear: true,
		State:              "FINISH",
		SubtaskName:        "part.3mf",
	}, nil
}

func (c *schedulingController) latestTerminalJob(_ context.Context, printerID int) (terminalJob, error) {
	return terminalJob{ID: printerID, Status: "completed", CompletedAt: c.now.Add(-time.Second)}, nil
}

func (c *schedulingController) activeQueueJob(_ context.Context, printerID int) (activeQueueJob, error) {
	return activeQueueJob{ID: printerID, Status: "printing", StartedAt: c.now.Add(-time.Minute)}, nil
}

func (c *schedulingController) plateClearEnabled(context.Context) (bool, error) {
	return true, nil
}

func (c *schedulingController) clearPlate(context.Context, int) error {
	return nil
}

func (c *schedulingController) pausePrint(context.Context, int) error {
	return nil
}

type schedulingAssessor struct {
	releasePrinterOne <-chan struct{}
	printerTwoSeen    chan struct{}
	once              sync.Once
}

func (a *schedulingAssessor) assess(_ context.Context, image []byte, _ string) (plateAssessment, error) {
	printerID := int(image[len(image)-1])
	if printerID == 1 {
		<-a.releasePrinterOne
	} else if printerID == 2 {
		a.once.Do(func() { close(a.printerTwoSeen) })
	}
	return plateAssessment{PlateVisible: true, IsEmpty: false, Confidence: 0.99, Reason: "test"}, nil
}

func (a *schedulingAssessor) assessFirstLayer(context.Context, []byte, string) (firstLayerAssessment, error) {
	return firstLayerAssessment{}, nil
}

func TestSamePrinterBacklogDoesNotStarveOtherPrinters(t *testing.T) {
	now := time.Now()
	releasePrinterOne := make(chan struct{})
	assessor := &schedulingAssessor{
		releasePrinterOne: releasePrinterOne,
		printerTwoSeen:    make(chan struct{}),
	}
	cfg := testConfig()
	cfg.WorkerCount = 2
	svc := newService(cfg, &schedulingController{now: now}, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if !svc.shutdown(ctx) {
			t.Error("service did not drain after concurrency test")
		}
	}()

	enqueue := func(printerID int) {
		svc.jobSlots <- struct{}{}
		svc.jobs <- plateJob{PrinterID: printerID, Event: testEvent(now), EventTime: now}
	}
	enqueue(1)
	enqueue(1)
	enqueue(1)
	enqueue(2)

	select {
	case <-assessor.printerTwoSeen:
	case <-time.After(time.Second):
		close(releasePrinterOne)
		t.Fatal("printer 1 backlog starved printer 2")
	}
	close(releasePrinterOne)
}
