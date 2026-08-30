package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fanTestController struct {
	mu             sync.Mutex
	statuses       map[int]plateGateStatus
	terminals      map[int]terminalJob
	active         map[int]activeQueueJob
	firstLayerOK   chan struct{}
	fanBlocks      map[fanCall]<-chan struct{}
	fanStarted     chan fanCall
	fanCalls       []fanCall
	fanCallTimes   []time.Time
	fanReports     map[fanCall]int
	fanReportDelay time.Duration
	cleared        chan int
}

func (c *fanTestController) snapshot(context.Context, int) ([]byte, string, error) {
	return []byte{0xff, 0xd8, 0xff, 0xdb, 1}, "image/jpeg", nil
}

func (c *fanTestController) printerModel(context.Context, int) (string, error) {
	return "P1S", nil
}

func (c *fanTestController) gateStatus(_ context.Context, printerID int) (plateGateStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.statuses[printerID]
	if status.AuxFanSpeed != nil {
		value := *status.AuxFanSpeed
		status.AuxFanSpeed = &value
	}
	if status.ChamberFanSpeed != nil {
		value := *status.ChamberFanSpeed
		status.ChamberFanSpeed = &value
	}
	if status.LeftAuxFanSpeed != nil {
		value := *status.LeftAuxFanSpeed
		status.LeftAuxFanSpeed = &value
	}
	return status, nil
}

func (c *fanTestController) latestTerminalJob(_ context.Context, printerID int) (terminalJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminals[printerID], nil
}

func (c *fanTestController) activeQueueJob(_ context.Context, printerID int) (activeQueueJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[printerID], nil
}

func (c *fanTestController) plateClearEnabled(context.Context) (bool, error) { return true, nil }
func (c *fanTestController) clearPlate(_ context.Context, printerID int) error {
	if c.cleared != nil {
		c.cleared <- printerID
	}
	return nil
}
func (c *fanTestController) pausePrint(context.Context, int) error { return nil }
func (c *fanTestController) enableAMSFilamentBackup(context.Context, int) error {
	return nil
}

func (c *fanTestController) setFanSpeed(ctx context.Context, printerID int, fan string, speed int) error {
	c.mu.Lock()
	call := fanCall{fan: fan, speed: speed}
	c.fanCalls = append(c.fanCalls, call)
	c.fanCallTimes = append(c.fanCallTimes, time.Now())
	block := c.fanBlocks[call]
	started := c.fanStarted
	reportedSpeed, overridden := c.fanReports[call]
	if !overridden {
		reportedSpeed = speed
	}
	reportDelay := c.fanReportDelay
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- call:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if reportDelay > 0 && speed > 0 {
		time.AfterFunc(reportDelay, func() {
			c.updateFanStatus(printerID, fan, reportedSpeed)
		})
		return nil
	}
	c.updateFanStatus(printerID, fan, reportedSpeed)
	return nil
}

func (c *fanTestController) updateFanStatus(printerID int, fan string, speed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.statuses[printerID]
	var reported *int
	switch fan {
	case "aux":
		reported = status.AuxFanSpeed
	case "chamber":
		reported = status.ChamberFanSpeed
	case "aux2":
		reported = status.LeftAuxFanSpeed
	}
	if reported != nil {
		*reported = speed
	}
	c.statuses[printerID] = status
}

type fanTestAssessor struct {
	firstLayerSeen chan struct{}
	once           sync.Once
}

func (a *fanTestAssessor) assess(context.Context, []byte, string) (plateAssessment, error) {
	return plateAssessment{PlateVisible: true, IsEmpty: true}, nil
}

func (a *fanTestAssessor) assessFirstLayer(context.Context, []byte, string) (firstLayerAssessment, error) {
	a.once.Do(func() { close(a.firstLayerSeen) })
	return firstLayerAssessment{FirstLayerVisible: true, IsDefective: false}, nil
}

func TestFanWaitDoesNotConsumeWorkerSlot(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	controller := &fanTestController{
		statuses: map[int]plateGateStatus{
			1: {
				ID: 1, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
				SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
			},
			2: {
				ID: 2, Name: "P1S-2", Connected: true, State: "RUNNING", CurrentPrint: "next.3mf",
				SubtaskName: "next.3mf", LayerNum: 2,
			},
		},
		terminals: map[int]terminalJob{1: {ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
		active:    map[int]activeQueueJob{2: {ID: 99, Status: "printing", StartedAt: now.Add(-time.Minute)}},
	}
	releaseStartup := make(chan struct{})
	controller.fanBlocks = map[fanCall]<-chan struct{}{{fan: "aux", speed: 100}: releaseStartup}
	controller.fanStarted = make(chan fanCall, 8)
	assessor := &fanTestAssessor{firstLayerSeen: make(chan struct{})}
	cfg := testConfig()
	cfg.WorkerCount = 1
	cfg.PostPrintFanDuration = 200 * time.Millisecond
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})

	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 1, Event: terminalEvent("print_complete", now), EventTime: now}
	select {
	case call := <-controller.fanStarted:
		if call != (fanCall{fan: "aux", speed: 100}) {
			t.Fatalf("unexpected first fan call: %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("fan startup did not begin")
	}
	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 2, Event: webhookEvent{
		Event: "first_layer_complete", Printer: "P1S-2", Filename: "next.3mf", Timestamp: now.Format(time.RFC3339Nano),
	}, EventTime: now}

	select {
	case <-assessor.firstLayerSeen:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fan startup consumed the only worker slot")
	}
	close(releaseStartup)
}

func TestFanCleanupDoesNotConsumeWorkerSlot(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	controller := &fanTestController{
		statuses: map[int]plateGateStatus{
			1: {
				ID: 1, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
				SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
			},
			2: {
				ID: 2, Name: "P1S-2", Connected: true, State: "RUNNING", CurrentPrint: "next.3mf",
				SubtaskName: "next.3mf", LayerNum: 2,
			},
		},
		terminals:  map[int]terminalJob{1: {ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
		active:     map[int]activeQueueJob{2: {ID: 99, Status: "printing", StartedAt: now.Add(-time.Minute)}},
		fanStarted: make(chan fanCall, 16),
	}
	assessor := &fanTestAssessor{firstLayerSeen: make(chan struct{})}
	cfg := testConfig()
	cfg.WorkerCount = 1
	cfg.PostPrintFanDuration = time.Minute
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, assessor, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 1, Event: terminalEvent("print_complete", now), EventTime: now}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}

	releaseStop := make(chan struct{})
	failedTime := now.Add(time.Second)
	controller.mu.Lock()
	status := controller.statuses[1]
	status.State = "FAILED"
	controller.statuses[1] = status
	controller.terminals[1] = terminalJob{ID: 43, Status: "failed", CompletedAt: failedTime.Add(-time.Second)}
	controller.fanBlocks = map[fanCall]<-chan struct{}{{fan: "aux", speed: 0}: releaseStop}
	controller.mu.Unlock()
	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 1, Event: terminalEvent("print_failed", failedTime), EventTime: failedTime}
	for {
		select {
		case call := <-controller.fanStarted:
			if call == (fanCall{fan: "aux", speed: 0}) {
				goto cleanupBlocked
			}
		case <-time.After(time.Second):
			t.Fatal("fan cleanup did not block")
		}
	}

cleanupBlocked:
	svc.jobSlots <- struct{}{}
	svc.jobs <- plateJob{PrinterID: 2, Event: webhookEvent{
		Event: "first_layer_complete", Printer: "P1S-2", Filename: "next.3mf", Timestamp: now.Format(time.RFC3339Nano),
	}, EventTime: now}
	select {
	case <-assessor.firstLayerSeen:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fan cleanup consumed the only worker slot")
	}
	close(releaseStop)
}

func TestReplacementPrintRelinquishesFanControlWithoutStoppingFans(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	replacement := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "RUNNING", CurrentPrint: "replacement.3mf",
		SubtaskName: "replacement.3mf", LayerNum: 2, AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	controller := &fanTestController{
		statuses:  map[int]plateGateStatus{7: gate},
		terminals: map[int]terminalJob{7: terminal},
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = time.Minute
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})

	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}
	controller.mu.Lock()
	controller.statuses[7] = replacement
	controller.mu.Unlock()
	replacementEvent := webhookEvent{
		Event: "first_layer_complete", Printer: "P1S", Filename: "replacement.3mf", Timestamp: now.Add(time.Second).Format(time.RFC3339Nano),
	}
	svc.cancelFanCycleForEvent(7, replacementEvent, now.Add(time.Second))

	done := make(chan struct{})
	go func() {
		svc.fanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replacement cancellation did not finish")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if aux != 100 || chamber != 100 {
		t.Fatalf("old completion stopped replacement print fans: aux=%d chamber=%d", aux, chamber)
	}
}

func TestTerminalStateTransitionPreservesFanCycleAssessment(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	controller := &fanTestController{
		statuses:  map[int]plateGateStatus{7: gate},
		terminals: map[int]terminalJob{7: {ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
		cleared:   make(chan int, 1),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = 20 * time.Millisecond
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fanTestAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		if started {
			status := controller.statuses[7]
			status.State = "IDLE"
			controller.statuses[7] = status
		}
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case printerID := <-controller.cleared:
		if printerID != 7 {
			t.Fatalf("unexpected cleared printer: %d", printerID)
		}
	case <-time.After(time.Second):
		t.Fatal("FINISH to IDLE transition lost post-fan assessment")
	}
}

func TestDuplicateCompletionDoesNotCancelActiveFanCycle(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	controller := &fanTestController{
		statuses:  map[int]plateGateStatus{7: gate},
		terminals: map[int]terminalJob{7: terminal},
		cleared:   make(chan int, 1),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = 20 * time.Millisecond
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fanTestAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}
	duplicateTime := now.Add(time.Second)
	duplicate := terminalEvent("print_complete", duplicateTime)
	svc.cancelFanCycleForEvent(7, duplicate, duplicateTime)
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: duplicate, EventTime: duplicateTime})

	select {
	case <-controller.cleared:
	case <-time.After(time.Second):
		t.Fatal("duplicate completion lost the active fan-cycle continuation")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	starts := 0
	for _, call := range controller.fanCalls {
		if call.speed == 100 {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("duplicate completion started another fan cycle: %+v", controller.fanCalls)
	}
}

func TestReplacementCancelsBlockedFanStartup(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	releaseStartup := make(chan struct{})
	controller := &fanTestController{
		statuses:   map[int]plateGateStatus{7: gate},
		terminals:  map[int]terminalJob{7: terminal},
		fanBlocks:  map[fanCall]<-chan struct{}{{fan: "aux", speed: 100}: releaseStartup},
		fanStarted: make(chan fanCall, 8),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = time.Minute
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})

	select {
	case call := <-controller.fanStarted:
		if call != (fanCall{fan: "aux", speed: 100}) {
			t.Fatalf("unexpected blocked startup call: %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("fan startup did not block")
	}
	replacement := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "PREPARE", CurrentPrint: "replacement.3mf",
		SubtaskName: "replacement.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	controller.mu.Lock()
	controller.statuses[7] = replacement
	controller.mu.Unlock()
	eventTime := now.Add(time.Second)
	svc.cancelFanCycleForEvent(7, webhookEvent{
		Event: "first_layer_complete", Printer: "P1S", Filename: "replacement.3mf",
	}, eventTime)

	done := make(chan struct{})
	go func() {
		svc.fanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked startup was not cancelled")
	}
	close(releaseStartup)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if aux != 0 || chamber != 0 {
		t.Fatalf("replacement inherited stale fan startup: aux=%d chamber=%d", aux, chamber)
	}
	if len(controller.fanCalls) != 1 || controller.fanCalls[0] != (fanCall{fan: "aux", speed: 100}) {
		t.Fatalf("fan startup continued after cancellation: %+v", controller.fanCalls)
	}
}

func TestReplacementCancelsBlockedFanStop(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	terminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	releaseStop := make(chan struct{})
	controller := &fanTestController{
		statuses:   map[int]plateGateStatus{7: gate},
		terminals:  map[int]terminalJob{7: terminal},
		fanBlocks:  map[fanCall]<-chan struct{}{{fan: "aux", speed: 0}: releaseStop},
		fanStarted: make(chan fanCall, 8),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = time.Millisecond
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})

	for {
		select {
		case call := <-controller.fanStarted:
			if call == (fanCall{fan: "aux", speed: 0}) {
				goto stopBlocked
			}
		case <-time.After(time.Second):
			t.Fatal("fan stop did not block")
		}
	}

stopBlocked:
	replacement := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "PAUSE", CurrentPrint: "replacement.3mf",
		SubtaskName: "replacement.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	controller.mu.Lock()
	controller.statuses[7] = replacement
	controller.mu.Unlock()
	eventTime := now.Add(time.Second)
	svc.cancelFanCycleForEvent(7, webhookEvent{
		Event: "first_layer_complete", Printer: "P1S", Filename: "replacement.3mf",
	}, eventTime)

	done := make(chan struct{})
	go func() {
		svc.fanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked fan stop was not cancelled")
	}
	close(releaseStop)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if aux != 100 || chamber != 100 {
		t.Fatalf("replacement fan state was changed: aux=%d chamber=%d", aux, chamber)
	}
	for _, call := range controller.fanCalls {
		if call == (fanCall{fan: "chamber", speed: 0}) {
			t.Fatalf("fan stop continued after replacement cancellation: %+v", controller.fanCalls)
		}
	}
}

func TestFailedEventWaitsForOldFanCleanupBeforeClearingGate(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	completedGate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	completedTerminal := terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}
	controller := &fanTestController{
		statuses:   map[int]plateGateStatus{7: completedGate},
		terminals:  map[int]terminalJob{7: completedTerminal},
		fanStarted: make(chan fanCall, 16),
		cleared:    make(chan int, 1),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = time.Minute
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fanTestAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}

	releaseStop := make(chan struct{})
	eventTime := now.Add(time.Second)
	failedGate := completedGate
	failedGate.State = "FAILED"
	failedTerminal := terminalJob{ID: 43, Status: "failed", CompletedAt: eventTime.Add(-time.Second)}
	controller.mu.Lock()
	controller.statuses[7] = failedGate
	controller.terminals[7] = failedTerminal
	controller.fanBlocks = map[fanCall]<-chan struct{}{{fan: "aux", speed: 0}: releaseStop}
	controller.mu.Unlock()
	jobDone := make(chan struct{})
	go func() {
		svc.processJob(context.Background(), plateJob{
			PrinterID: 7,
			Event:     terminalEvent("print_failed", eventTime),
			EventTime: eventTime,
		})
		close(jobDone)
	}()
	for {
		select {
		case call := <-controller.fanStarted:
			if call == (fanCall{fan: "aux", speed: 0}) {
				goto cleanupBlocked
			}
		case <-time.After(time.Second):
			t.Fatal("old fan cleanup did not begin")
		}
	}

cleanupBlocked:
	select {
	case <-controller.cleared:
		t.Fatal("failed-print gate cleared while old fan cleanup was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStop)
	select {
	case printerID := <-controller.cleared:
		if printerID != 7 {
			t.Fatalf("unexpected cleared printer: %d", printerID)
		}
	case <-time.After(time.Second):
		t.Fatal("failed-print gate did not clear after fan cleanup")
	}
	select {
	case <-jobDone:
	case <-time.After(time.Second):
		t.Fatal("failed-print processing did not finish")
	}
}

func TestRecoverPersistedFanState(t *testing.T) {
	now := time.Now()
	aux, chamber := 100, 100
	stateFile := filepath.Join(t.TempDir(), "fan-cycles.json")
	record := fanCycleRecord{
		PrinterID: 7,
		Fans:      []string{"aux", "chamber"},
		ExpiresAt: now.Add(-time.Minute),
		Job:       plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now},
		Gate:      plateGateStatus{ID: 7, Name: "P1S", State: "FINISH", AwaitingPlateClear: true, SubtaskName: "part.3mf"},
		Terminal:  terminalJob{ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)},
	}
	data, err := json.Marshal(map[int]fanCycleRecord{7: record})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	status := record.Gate
	status.Connected = true
	status.AuxFanSpeed = &aux
	status.ChamberFanSpeed = &chamber
	controller := &fakeController{gateStatuses: []plateGateStatus{status}}
	cfg := testConfig()
	cfg.PostPrintFanStateFile = stateFile
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	if err := svc.recoverPostPrintFans(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	if len(controller.fanCalls) != 2 || controller.fanCalls[0].speed != 0 || controller.fanCalls[1].speed != 0 {
		controller.mu.Unlock()
		t.Fatalf("persisted fans were not stopped: %+v", controller.fanCalls)
	}
	controller.mu.Unlock()
	contents, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "{}" {
		t.Fatalf("persisted fan state was not cleared: %s", contents)
	}
}

func TestRecoverPersistedFanStateDoesNotStopActivePrint(t *testing.T) {
	for _, state := range []string{"PREPARE", "SLICING", "RUNNING", "PAUSE"} {
		t.Run(state, func(t *testing.T) {
			now := time.Now()
			stateFile := filepath.Join(t.TempDir(), "fan-cycles.json")
			record := fanCycleRecord{PrinterID: 7, Fans: []string{"aux", "chamber"}, ExpiresAt: now}
			data, err := json.Marshal(map[int]fanCycleRecord{7: record})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stateFile, data, 0o600); err != nil {
				t.Fatal(err)
			}
			controller := &fakeController{gateStatuses: []plateGateStatus{{ID: 7, Connected: true, State: state}}}
			cfg := testConfig()
			cfg.PostPrintFanStateFile = stateFile
			svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
			if err := svc.recoverPostPrintFans(context.Background()); err != nil {
				t.Fatal(err)
			}
			controller.mu.Lock()
			defer controller.mu.Unlock()
			if len(controller.fanCalls) != 0 {
				t.Fatalf("recovery changed active-print fans: %+v", controller.fanCalls)
			}
		})
	}
}

func TestDryRunRefusesPersistedLiveFanState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "fan-cycles.json")
	record := fanCycleRecord{PrinterID: 7, Fans: []string{"aux", "chamber"}}
	data, err := json.Marshal(map[int]fanCycleRecord{7: record})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	cfg := testConfig()
	cfg.DryRun = true
	cfg.PostPrintFanStateFile = stateFile
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	if err := svc.recoverPostPrintFans(context.Background()); err == nil {
		t.Fatal("dry run accepted persisted live fan ownership")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.fanCalls) != 0 {
		t.Fatalf("dry-run recovery sent fan commands: %+v", controller.fanCalls)
	}
}

func TestShutdownReportsUnacknowledgedFanCleanup(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	releaseStop := make(chan struct{})
	controller := &fanTestController{
		statuses:  map[int]plateGateStatus{7: gate},
		terminals: map[int]terminalJob{7: {ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second)}},
		fanBlocks: map[fanCall]<-chan struct{}{{fan: "aux", speed: 0}: releaseStop},
	}
	stateFile := filepath.Join(t.TempDir(), "fan-cycles.json")
	cfg := testConfig()
	cfg.PostPrintFanDuration = time.Minute
	cfg.PostPrintFanSpeed = 100
	cfg.PostPrintFanStateFile = stateFile
	svc := newService(cfg, controller, &fakeAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		started := aux == 100 && chamber == 100
		controller.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan cycle did not start")
		}
		time.Sleep(time.Millisecond)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if svc.shutdown(shutdownCtx) {
		cancel()
		t.Fatal("shutdown reported successful fan cleanup without acknowledgement")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		svc.fanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fan cycle did not terminate after shutdown deadline")
	}
	close(releaseStop)
	contents, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "{}" {
		t.Fatal("failed shutdown discarded persisted fan ownership")
	}
}

type blockingStopController struct{}

func (blockingStopController) snapshot(context.Context, int) ([]byte, string, error) {
	return nil, "", nil
}
func (blockingStopController) printerModel(context.Context, int) (string, error) {
	return "P1S", nil
}
func (blockingStopController) gateStatus(_ context.Context, printerID int) (plateGateStatus, error) {
	value := 100
	return plateGateStatus{
		ID: printerID, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &value, ChamberFanSpeed: &value,
	}, nil
}
func (blockingStopController) latestTerminalJob(context.Context, int) (terminalJob, error) {
	return terminalJob{ID: 42, Status: "completed"}, nil
}
func (blockingStopController) activeQueueJob(context.Context, int) (activeQueueJob, error) {
	return activeQueueJob{}, nil
}
func (blockingStopController) plateClearEnabled(context.Context) (bool, error) { return true, nil }
func (blockingStopController) clearPlate(context.Context, int) error           { return nil }
func (blockingStopController) pausePrint(context.Context, int) error           { return nil }
func (blockingStopController) enableAMSFilamentBackup(context.Context, int) error {
	return nil
}

func (blockingStopController) setFanSpeed(ctx context.Context, _ int, _ string, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestFanCleanupDeadlinesAreIndependentAcrossPrinters(t *testing.T) {
	svc := &service{controller: blockingStopController{}}
	started := time.Now()
	var wg sync.WaitGroup
	for printerID := 1; printerID <= 2; printerID++ {
		wg.Add(1)
		go func(printerID int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			value := 100
			gate := plateGateStatus{
				ID: printerID, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
				SubtaskName: "part.3mf", AuxFanSpeed: &value, ChamberFanSpeed: &value,
			}
			cycle := newFanCycle(fanCycleRecord{
				PrinterID: printerID,
				Fans:      []string{"aux", "chamber"},
				Gate:      gate,
				Terminal:  terminalJob{ID: 42, Status: "completed"},
			})
			_, _ = svc.stopFanList(ctx, cycle, 0)
		}(printerID)
	}
	wg.Wait()
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("fan cleanup deadlines were serialized across printers")
	}
}

func TestPostPrintFansUseReportedCapabilities(t *testing.T) {
	value := 0
	tests := []struct {
		name   string
		model  string
		status plateGateStatus
		want   []string
	}{
		{name: "P1S", model: "P1S", status: plateGateStatus{AuxFanSpeed: &value, ChamberFanSpeed: &value}, want: []string{"aux", "chamber"}},
		{name: "base P2S", model: "P2S", status: plateGateStatus{AuxFanSpeed: &value, ChamberFanSpeed: &value}, want: []string{"aux"}},
		{name: "base P2S alias", model: "N7", status: plateGateStatus{AuxFanSpeed: &value, ChamberFanSpeed: &value}, want: []string{"aux"}},
		{name: "P2S exhaust kit", model: "P2S", status: plateGateStatus{AuxFanSpeed: &value, ChamberFanSpeed: &value, ExhaustFanPresent: true}, want: []string{"aux", "chamber"}},
		{name: "base X2D alias", model: "N6", status: plateGateStatus{AuxFanSpeed: &value, ChamberFanSpeed: &value}, want: []string{"aux"}},
		{name: "left aux", model: "P1S", status: plateGateStatus{AuxFanSpeed: &value, LeftAuxFanSpeed: &value}, want: []string{"aux", "aux2"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fans := postPrintFans(test.status, test.model)
			if len(fans) != len(test.want) {
				t.Fatalf("unexpected capability-derived fans: %v", fans)
			}
			for i := range test.want {
				if fans[i] != test.want[i] {
					t.Fatalf("unexpected capability-derived fans: %v", fans)
				}
			}
		})
	}
}

func TestFanSpeedAcknowledgementAllowsQuantizedReports(t *testing.T) {
	for _, test := range []struct {
		reported  int
		requested int
		want      bool
	}{
		{reported: 53, requested: 50, want: true},
		{reported: 73, requested: 75, want: true},
		{reported: 100, requested: 100, want: true},
		{reported: 40, requested: 50, want: false},
	} {
		if got := fanSpeedMatches(test.reported, test.requested); got != test.want {
			t.Fatalf("fanSpeedMatches(%d, %d)=%t want %t", test.reported, test.requested, got, test.want)
		}
	}
}

func TestFanDurationStartsAfterDelayedQuantizedAcknowledgement(t *testing.T) {
	now := time.Now()
	aux, chamber := 0, 0
	gate := plateGateStatus{
		ID: 7, Name: "P1S", Connected: true, State: "FINISH", AwaitingPlateClear: true,
		SubtaskName: "part.3mf", AuxFanSpeed: &aux, ChamberFanSpeed: &chamber,
	}
	reportDelay := 20 * time.Millisecond
	duration := 20 * time.Millisecond
	controller := &fanTestController{
		statuses: map[int]plateGateStatus{7: gate},
		terminals: map[int]terminalJob{7: {
			ID: 42, Status: "completed", CompletedAt: now.Add(-time.Second),
		}},
		fanReports: map[fanCall]int{
			{fan: "aux", speed: 50}:     53,
			{fan: "chamber", speed: 50}: 53,
		},
		fanReportDelay: reportDelay,
		cleared:        make(chan int, 1),
	}
	cfg := testConfig()
	cfg.PostPrintFanDuration = duration
	cfg.PostPrintFanSpeed = 50
	cfg.PostPrintFanStateFile = filepath.Join(t.TempDir(), "fan-cycles.json")
	svc := newService(cfg, controller, &fanTestAssessor{}, log.New(io.Discard, "", 0))
	svc.start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		svc.shutdown(ctx)
	})
	svc.processJob(context.Background(), plateJob{PrinterID: 7, Event: testEvent(now), EventTime: now})
	select {
	case <-controller.cleared:
	case <-time.After(2 * time.Second):
		t.Fatal("quantized fan cycle did not complete")
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	var lastStart, firstStop time.Time
	for i, call := range controller.fanCalls {
		if call.speed == 50 {
			lastStart = controller.fanCallTimes[i]
		}
		if call.speed == 0 && firstStop.IsZero() {
			firstStop = controller.fanCallTimes[i]
		}
	}
	if lastStart.IsZero() || firstStop.IsZero() {
		t.Fatalf("missing start or stop calls: %+v", controller.fanCalls)
	}
	if elapsed := firstStop.Sub(lastStart); elapsed < reportDelay+duration {
		t.Fatalf("fan duration began before acknowledgement: elapsed=%s want_at_least=%s", elapsed, reportDelay+duration)
	}
}
