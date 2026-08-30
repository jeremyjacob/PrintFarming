package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxWebhookBodyBytes = 5 << 20
	maxImageBytes       = 4 << 20
)

type plateController interface {
	snapshot(context.Context, int) ([]byte, string, error)
	printerModel(context.Context, int) (string, error)
	gateStatus(context.Context, int) (plateGateStatus, error)
	latestTerminalJob(context.Context, int) (terminalJob, error)
	activeQueueJob(context.Context, int) (activeQueueJob, error)
	plateClearEnabled(context.Context) (bool, error)
	clearPlate(context.Context, int) error
	pausePrint(context.Context, int) error
	enableAMSFilamentBackup(context.Context, int) error
	setFanSpeed(context.Context, int, string, int) error
}

type plateAssessor interface {
	assess(context.Context, []byte, string) (plateAssessment, error)
	assessFirstLayer(context.Context, []byte, string) (firstLayerAssessment, error)
}

type webhookEvent struct {
	Event     string `json:"event"`
	Printer   string `json:"printer"`
	Filename  string `json:"filename"`
	Timestamp string `json:"timestamp"`
}

type plateJob struct {
	PrinterID       int
	Event           webhookEvent
	EventTime       time.Time
	AfterFanCycle   bool
	AfterFanCleanup bool
}

type service struct {
	controller            plateController
	assessor              plateAssessor
	logger                *log.Logger
	webhookSecret         string
	snapshotDelay         time.Duration
	eventMaxAge           time.Duration
	timezone              *time.Location
	workerCount           int
	enableAMSBackup       bool
	postPrintFanDuration  time.Duration
	postPrintFanSpeed     int
	postPrintFanStateFile string
	dryRun                bool
	jobs                  chan plateJob
	jobSlots              chan struct{}
	workerSlots           chan struct{}
	queueMu               sync.RWMutex
	accepting             bool
	workerCancel          context.CancelFunc
	workerWG              sync.WaitGroup
	fanMu                 sync.Mutex
	fanPersistMu          sync.Mutex
	fanContext            context.Context
	fanCancel             context.CancelFunc
	fanCycles             map[int]*fanCycle
	fanRecords            map[int]fanCycleRecord
	fanStopping           bool
	fanDrainContext       context.Context
	fanCleanupFailed      bool
	fanWG                 sync.WaitGroup
}

func newService(cfg config, controller plateController, assessor plateAssessor, logger *log.Logger) *service {
	workerCount := cfg.WorkerCount
	if workerCount < 1 {
		workerCount = 1
	}
	return &service{
		controller:            controller,
		assessor:              assessor,
		logger:                logger,
		webhookSecret:         cfg.WebhookSecret,
		snapshotDelay:         cfg.SnapshotDelay,
		eventMaxAge:           cfg.EventMaxAge,
		timezone:              cfg.BambuddyTimezone,
		workerCount:           workerCount,
		enableAMSBackup:       cfg.EnableAMSBackup,
		postPrintFanDuration:  cfg.PostPrintFanDuration,
		postPrintFanSpeed:     cfg.PostPrintFanSpeed,
		postPrintFanStateFile: cfg.PostPrintFanStateFile,
		dryRun:                cfg.DryRun,
		jobs:                  make(chan plateJob, 256),
		jobSlots:              make(chan struct{}, 256),
		workerSlots:           make(chan struct{}, workerCount),
		fanCycles:             make(map[int]*fanCycle),
		fanRecords:            make(map[int]fanCycleRecord),
	}
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /webhooks/bambuddy/{printerID}", s.handleWebhook)
	return mux
}

func (s *service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.queueMu.RLock()
	accepting := s.accepting
	s.queueMu.RUnlock()
	if !accepting {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *service) handleReady(w http.ResponseWriter, r *http.Request) {
	s.queueMu.RLock()
	accepting := s.accepting
	s.queueMu.RUnlock()
	if !accepting || len(s.jobSlots) == cap(s.jobSlots) || !s.fanStateReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	enabled, err := s.controller.plateClearEnabled(r.Context())
	if err != nil || !enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "plate-clear gate unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *service) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.validAuthorization(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	printerID, err := strconv.Atoi(r.PathValue("printerID"))
	if err != nil || printerID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid printer ID"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	decoder := json.NewDecoder(r.Body)
	var event webhookEvent
	if err := decoder.Decode(&event); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	if !isSupportedEvent(event.Event) {
		s.logger.Printf("ignored Bambuddy event=%q printer_id=%d", event.Event, printerID)
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	eventTime, err := parseBambuddyTime(event.Timestamp, s.timezone)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid webhook timestamp"})
		return
	}
	now := time.Now()
	if eventTime.Before(now.Add(-s.eventMaxAge)) || eventTime.After(now.Add(time.Minute)) {
		s.logger.Printf("rejected stale event=%q printer_id=%d timestamp=%q", event.Event, printerID, event.Timestamp)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "stale webhook timestamp"})
		return
	}
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if !s.accepting {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service is stopping"})
		return
	}
	select {
	case s.jobSlots <- struct{}{}:
	default:
		s.logger.Printf("job queue full; event=%q took no action printer_id=%d", event.Event, printerID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "job queue full"})
		return
	}
	select {
	case s.jobs <- plateJob{PrinterID: printerID, Event: event, EventTime: eventTime}:
		s.cancelFanCycleForEvent(printerID, event, eventTime)
		s.logger.Printf("accepted Bambuddy event=%q printer_id=%d printer=%q filename=%q", event.Event, printerID, event.Printer, event.Filename)
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	default:
		<-s.jobSlots
		s.logger.Printf("job queue full; event=%q took no action printer_id=%d", event.Event, printerID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "job queue full"})
	}
}

func (s *service) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	fanCtx, fanCancel := context.WithCancel(parent)
	s.queueMu.Lock()
	s.workerCancel = cancel
	s.accepting = true
	s.queueMu.Unlock()
	s.fanMu.Lock()
	s.fanContext = fanCtx
	s.fanCancel = fanCancel
	s.fanStopping = false
	s.fanDrainContext = nil
	s.fanCleanupFailed = false
	s.fanMu.Unlock()
	s.workerWG.Add(1)
	go s.dispatch(ctx)
}

func (s *service) dispatch(ctx context.Context) {
	defer s.workerWG.Done()
	printerQueues := make(map[int]chan plateJob)
	for job := range s.jobs {
		queue := printerQueues[job.PrinterID]
		if queue == nil {
			queue = make(chan plateJob, cap(s.jobs))
			printerQueues[job.PrinterID] = queue
			s.workerWG.Add(1)
			go s.runPrinter(ctx, job.PrinterID, queue)
		}
		queue <- job
	}
	for _, queue := range printerQueues {
		close(queue)
	}
}

func (s *service) runPrinter(ctx context.Context, printerID int, jobs <-chan plateJob) {
	defer s.workerWG.Done()
	for job := range jobs {
		func() {
			defer func() { <-s.jobSlots }()
			if ctx.Err() != nil {
				s.logger.Printf("discarded queued event during forced shutdown printer_id=%d", printerID)
				return
			}
			select {
			case s.workerSlots <- struct{}{}:
				defer func() { <-s.workerSlots }()
				s.processJob(ctx, job)
			case <-ctx.Done():
				s.logger.Printf("discarded queued event during forced shutdown printer_id=%d", printerID)
			}
		}()
	}
}

func (s *service) stopAccepting() {
	s.queueMu.Lock()
	s.accepting = false
	s.queueMu.Unlock()
}

func (s *service) shutdown(ctx context.Context) bool {
	s.queueMu.Lock()
	s.accepting = false
	close(s.jobs)
	cancel := s.workerCancel
	s.queueMu.Unlock()
	s.stopFanCycles(ctx)

	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		s.fanWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.fanMu.Lock()
		fanCleanupFailed := s.fanCleanupFailed
		s.fanMu.Unlock()
		return !fanCleanupFailed
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return false
	}
}

func (s *service) processJob(ctx context.Context, job plateJob) {
	switch job.Event.Event {
	case "print_complete", "print_failed", "print_stopped":
		s.processCompletion(ctx, job)
	case "first_layer_complete":
		s.processFirstLayer(ctx, job)
	}
}

func (s *service) processCompletion(ctx context.Context, job plateJob) {
	if !job.AfterFanCycle && !job.AfterFanCleanup && time.Since(job.EventTime) > s.eventMaxAge {
		s.logger.Printf("ignored stale queued event=%q printer_id=%d", job.Event.Event, job.PrinterID)
		return
	}
	initialGate, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot verify plate gate: %v", job.PrinterID, err)
		return
	}
	if !initialGate.AwaitingPlateClear {
		s.logger.Printf("ignored stale event=%q printer_id=%d: printer is not awaiting plate clear", job.Event.Event, job.PrinterID)
		return
	}
	if initialGate.ID != job.PrinterID || initialGate.Name != job.Event.Printer {
		s.logger.Printf(
			"plate remains gated printer_id=%d: webhook printer %q does not match Bambuddy printer %q",
			job.PrinterID,
			job.Event.Printer,
			initialGate.Name,
		)
		return
	}
	if !initialGate.matchesEvent(job.Event) {
		s.logger.Printf(
			"plate remains gated printer_id=%d: webhook filename %q does not match current print %q",
			job.PrinterID,
			job.Event.Filename,
			initialGate.identity(),
		)
		return
	}
	initialTerminal, err := s.controller.latestTerminalJob(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot identify terminal queue job: %v", job.PrinterID, err)
		return
	}
	if !terminalMatchesEvent(initialTerminal, job.Event.Event, job.EventTime, s.eventMaxAge) {
		s.logger.Printf("plate remains gated printer_id=%d: webhook does not match latest terminal queue job", job.PrinterID)
		return
	}
	if job.Event.Event != "print_complete" {
		if s.deferCanceledFanCycle(ctx, job) {
			return
		}
	}
	if job.Event.Event == "print_complete" && !job.AfterFanCycle {
		scheduled, err := s.schedulePostPrintFanCycle(ctx, job, initialGate, initialTerminal)
		if err != nil {
			s.logger.Printf("plate remains gated printer_id=%d: post-print fan cycle failed: %v", job.PrinterID, err)
			return
		}
		if scheduled {
			return
		}
	}

	first, err := s.assessFreshSnapshot(ctx, job.PrinterID, s.snapshotDelay)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: first fresh assessment failed: %v", job.PrinterID, err)
		return
	}
	s.logAssessment(job.PrinterID, "first", first)
	if !s.safeAssessment(first) {
		s.logger.Printf("plate remains gated printer_id=%d", job.PrinterID)
		return
	}

	second, err := s.assessFreshSnapshot(ctx, job.PrinterID, 0)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: confirmation assessment failed: %v", job.PrinterID, err)
		return
	}
	s.logAssessment(job.PrinterID, "confirmation", second)
	if !s.safeAssessment(second) {
		s.logger.Printf("plate remains gated printer_id=%d", job.PrinterID)
		return
	}
	plateClearEnabled, err := s.controller.plateClearEnabled(ctx)
	if err != nil || !plateClearEnabled {
		s.logger.Printf("plate remains gated printer_id=%d: require_plate_clear is not verified enabled: %v", job.PrinterID, err)
		return
	}
	currentTerminal, err := s.controller.latestTerminalJob(ctx, job.PrinterID)
	if err != nil || !sameTerminalJob(initialTerminal, currentTerminal) {
		s.logger.Printf("plate remains gated printer_id=%d: terminal queue job changed during assessment", job.PrinterID)
		return
	}
	currentGate, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot re-verify plate gate: %v", job.PrinterID, err)
		return
	}
	if !samePlateGate(initialGate, currentGate) {
		s.logger.Printf("plate remains gated printer_id=%d: plate gate changed during assessment", job.PrinterID)
		return
	}
	if s.dryRun {
		s.logger.Printf("dry run: all checks passed; would clear plate gate printer_id=%d", job.PrinterID)
		return
	}
	if err := s.controller.clearPlate(ctx, job.PrinterID); err != nil {
		s.logger.Printf("clear-plate outcome unknown printer_id=%d: request failed after possible delivery: %v", job.PrinterID, err)
		return
	}
	s.logger.Printf("plate gate cleared printer_id=%d; Bambuddy may dispatch the next queued print", job.PrinterID)
}

func (s *service) processFirstLayer(ctx context.Context, job plateJob) {
	if time.Since(job.EventTime) > s.eventMaxAge {
		s.logger.Printf("ignored stale first-layer event printer_id=%d", job.PrinterID)
		return
	}
	initialStatus, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("first-layer check skipped printer_id=%d: cannot verify active print: %v", job.PrinterID, err)
		return
	}
	if !activePrintMatchesEvent(initialStatus, job.PrinterID, job.Event) {
		s.logger.Printf("first-layer check skipped printer_id=%d: webhook does not match an active print", job.PrinterID)
		return
	}
	initialJob, err := s.controller.activeQueueJob(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("first-layer check skipped printer_id=%d: cannot bind event to an active queue job: %v", job.PrinterID, err)
		return
	}
	if !activeJobMatchesEvent(initialJob, job.EventTime) {
		s.logger.Printf("first-layer check skipped printer_id=%d: event predates the active queue job", job.PrinterID)
		return
	}
	if s.deferCanceledFanCycle(ctx, job) {
		return
	}

	firstImage, firstMediaType, err := s.freshSnapshot(ctx, job.PrinterID, 0)
	if err != nil {
		s.logger.Printf("first-layer check inconclusive printer_id=%d: first snapshot failed: %v", job.PrinterID, err)
		return
	}
	first, err := s.assessor.assessFirstLayer(ctx, firstImage, firstMediaType)
	if err != nil {
		s.logger.Printf("first-layer check inconclusive printer_id=%d: first assessment failed: %v", job.PrinterID, err)
		return
	}
	s.logFirstLayerAssessment(job.PrinterID, "first", first)
	if !s.certainFirstLayerFailure(first) {
		s.logger.Printf("first-layer assessment did not justify a pause; print continues printer_id=%d", job.PrinterID)
		s.enableAMSBackupForReviewedPrint(ctx, job, initialStatus, initialJob)
		return
	}

	secondImage, secondMediaType, err := s.freshSnapshot(ctx, job.PrinterID, 0)
	if err != nil {
		s.logger.Printf("first-layer check inconclusive printer_id=%d: confirmation snapshot failed: %v", job.PrinterID, err)
		return
	}
	if bytes.Equal(firstImage, secondImage) {
		s.logger.Printf("first-layer failure not confirmed printer_id=%d: camera returned identical snapshots", job.PrinterID)
		return
	}
	second, err := s.assessor.assessFirstLayer(ctx, secondImage, secondMediaType)
	if err != nil {
		s.logger.Printf("first-layer check inconclusive printer_id=%d: confirmation assessment failed: %v", job.PrinterID, err)
		return
	}
	s.logFirstLayerAssessment(job.PrinterID, "confirmation", second)
	if !s.certainFirstLayerFailure(second) {
		s.logger.Printf("first-layer failure was not confirmed; print continues printer_id=%d", job.PrinterID)
		s.enableAMSBackupForReviewedPrint(ctx, job, initialStatus, initialJob)
		return
	}
	if time.Since(job.EventTime) > s.eventMaxAge {
		s.logger.Printf("first-layer failure not actioned printer_id=%d: event expired during assessment", job.PrinterID)
		return
	}

	currentStatus, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("first-layer failure not actioned printer_id=%d: cannot re-verify active print: %v", job.PrinterID, err)
		return
	}
	if !sameActivePrint(initialStatus, currentStatus) {
		s.logger.Printf("first-layer failure not actioned printer_id=%d: active print changed during assessment", job.PrinterID)
		return
	}
	currentJob, err := s.controller.activeQueueJob(ctx, job.PrinterID)
	if err != nil || !sameActiveJob(initialJob, currentJob) {
		s.logger.Printf("first-layer failure not actioned printer_id=%d: active queue job changed during assessment", job.PrinterID)
		return
	}
	if s.dryRun {
		s.logger.Printf("dry run: certain first-layer failure confirmed; would pause print printer_id=%d", job.PrinterID)
		return
	}
	if err := s.controller.pausePrint(ctx, job.PrinterID); err != nil {
		s.logger.Printf("pause outcome unknown printer_id=%d: request failed after possible delivery: %v", job.PrinterID, err)
		return
	}
	s.logger.Printf("print pause requested after certain first-layer failure printer_id=%d", job.PrinterID)
}

func (s *service) enableAMSBackupForReviewedPrint(
	ctx context.Context,
	job plateJob,
	initialStatus plateGateStatus,
	initialJob activeQueueJob,
) {
	if !s.enableAMSBackup {
		return
	}
	if time.Since(job.EventTime) > s.eventMaxAge {
		s.logger.Printf("AMS filament backup not enabled printer_id=%d: first-layer event expired during review", job.PrinterID)
		return
	}
	currentStatus, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("AMS filament backup not enabled printer_id=%d: cannot re-verify active print: %v", job.PrinterID, err)
		return
	}
	if !sameActivePrint(initialStatus, currentStatus) {
		s.logger.Printf("AMS filament backup not enabled printer_id=%d: active print changed during review", job.PrinterID)
		return
	}
	currentJob, err := s.controller.activeQueueJob(ctx, job.PrinterID)
	if err != nil || !sameActiveJob(initialJob, currentJob) {
		s.logger.Printf("AMS filament backup not enabled printer_id=%d: active queue job changed during review", job.PrinterID)
		return
	}
	if s.dryRun {
		s.logger.Printf("dry run: first-layer review passed; would enable AMS filament backup printer_id=%d", job.PrinterID)
		return
	}
	if err := s.controller.enableAMSFilamentBackup(ctx, job.PrinterID); err != nil {
		s.logger.Printf("AMS filament backup outcome unknown printer_id=%d: request failed after possible delivery: %v", job.PrinterID, err)
		return
	}
	s.logger.Printf("AMS filament backup enabled after first-layer review printer_id=%d", job.PrinterID)
}

func (s *service) assessFreshSnapshot(ctx context.Context, printerID int, delay time.Duration) (plateAssessment, error) {
	image, mediaType, err := s.freshSnapshot(ctx, printerID, delay)
	if err != nil {
		return plateAssessment{}, err
	}
	return s.assessor.assess(ctx, image, mediaType)
}

func (s *service) freshSnapshot(ctx context.Context, printerID int, delay time.Duration) ([]byte, string, error) {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-timer.C:
		}
	}
	image, contentType, err := s.controller.snapshot(ctx, printerID)
	if err != nil {
		return nil, "", err
	}
	mediaType, err := validateImage(image, contentType)
	if err != nil {
		return nil, "", err
	}
	return image, mediaType, nil
}

func (s *service) safeAssessment(assessment plateAssessment) bool {
	return assessment.PlateVisible && assessment.IsEmpty
}

func (s *service) certainFirstLayerFailure(assessment firstLayerAssessment) bool {
	return assessment.FirstLayerVisible && assessment.IsDefective
}

func (s *service) logAssessment(printerID int, stage string, assessment plateAssessment) {
	s.logger.Printf(
		"plate assessment printer_id=%d stage=%s visible=%t empty=%t confidence=%.3f reason=%q",
		printerID,
		stage,
		assessment.PlateVisible,
		assessment.IsEmpty,
		assessment.Confidence,
		assessment.Reason,
	)
}

func (s *service) logFirstLayerAssessment(printerID int, stage string, assessment firstLayerAssessment) {
	s.logger.Printf(
		"first-layer assessment printer_id=%d stage=%s visible=%t defective=%t confidence=%.3f reason=%q",
		printerID,
		stage,
		assessment.FirstLayerVisible,
		assessment.IsDefective,
		assessment.Confidence,
		assessment.Reason,
	)
}

func (s plateGateStatus) identity() string {
	if strings.TrimSpace(s.SubtaskName) != "" {
		return s.SubtaskName
	}
	return s.GcodeFile
}

func (s plateGateStatus) matchesEvent(event webhookEvent) bool {
	return normalizePrintName(s.identity()) != "" && normalizePrintName(s.identity()) == normalizePrintName(event.Filename)
}

func samePlateGate(before, after plateGateStatus) bool {
	return before.AwaitingPlateClear &&
		after.AwaitingPlateClear &&
		before.ID == after.ID &&
		before.Name == after.Name &&
		before.State == after.State &&
		before.identity() != "" &&
		before.identity() == after.identity()
}

func activePrintMatchesEvent(status plateGateStatus, printerID int, event webhookEvent) bool {
	return status.ID == printerID &&
		status.Name == event.Printer &&
		status.Connected &&
		status.State == "RUNNING" &&
		status.LayerNum >= 2 &&
		normalizePrintName(status.CurrentPrint) != "" &&
		normalizePrintName(status.CurrentPrint) == normalizePrintName(event.Filename) &&
		status.matchesEvent(event)
}

func sameActivePrint(before, after plateGateStatus) bool {
	return before.ID > 0 &&
		before.ID == after.ID &&
		before.Name == after.Name &&
		before.Connected &&
		after.Connected &&
		before.State == "RUNNING" &&
		after.State == "RUNNING" &&
		before.CurrentPrint != "" &&
		before.CurrentPrint == after.CurrentPrint &&
		before.identity() != "" &&
		before.identity() == after.identity() &&
		before.LayerNum >= 2 &&
		after.LayerNum >= before.LayerNum
}

func activeJobMatchesEvent(job activeQueueJob, eventTime time.Time) bool {
	return job.ID > 0 && job.Status == "printing" && !eventTime.Before(job.StartedAt)
}

func sameActiveJob(before, after activeQueueJob) bool {
	return before.ID > 0 &&
		before.ID == after.ID &&
		before.Status == "printing" &&
		after.Status == "printing" &&
		before.StartedAt.Equal(after.StartedAt)
}

func sameTerminalJob(before, after terminalJob) bool {
	return before.ID > 0 &&
		before.ID == after.ID &&
		before.Status == after.Status &&
		before.CompletedAt.Equal(after.CompletedAt) &&
		before.PlateType == after.PlateType
}

func normalizePrintName(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Base(value)
	value = strings.ToLower(value)
	for {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(value, ".3mf"), ".gcode")
		if trimmed == value {
			break
		}
		value = trimmed
	}
	var normalized strings.Builder
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}

func terminalMatchesEvent(job terminalJob, eventType string, eventTime time.Time, maxAge time.Duration) bool {
	delta := eventTime.Sub(job.CompletedAt)
	return job.ID > 0 && terminalStatusMatchesEvent(job.Status, eventType) && delta >= 0 && delta <= maxAge
}

func terminalStatusMatchesEvent(status, eventType string) bool {
	switch eventType {
	case "print_complete":
		return status == "completed"
	case "print_failed":
		return status == "failed"
	case "print_stopped":
		return status == "cancelled" || status == "aborted"
	default:
		return false
	}
}

func isSupportedEvent(eventType string) bool {
	switch eventType {
	case "print_complete", "print_failed", "print_stopped", "first_layer_complete":
		return true
	default:
		return false
	}
}

func (s *service) validAuthorization(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.webhookSecret)) == 1
}

func validateImage(image []byte, declaredType string) (string, error) {
	if len(image) == 0 {
		return "", fmt.Errorf("image is empty")
	}
	if len(image) > maxImageBytes {
		return "", fmt.Errorf("image exceeds %d bytes", maxImageBytes)
	}
	detectedType := strings.Split(http.DetectContentType(image), ";")[0]
	switch detectedType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return detectedType, nil
	}
	declaredType = strings.Split(declaredType, ";")[0]
	return "", fmt.Errorf("unsupported image content type (declared %q, detected %q)", declaredType, detectedType)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
