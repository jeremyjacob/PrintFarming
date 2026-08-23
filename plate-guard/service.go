package main

import (
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
	gateStatus(context.Context, int) (plateGateStatus, error)
	latestCompletion(context.Context, int) (completionRecord, error)
	plateClearEnabled(context.Context) (bool, error)
	checkPlate(context.Context, int, string) (localPlateAssessment, error)
	clearPlate(context.Context, int) error
}

type plateAssessor interface {
	assess(context.Context, []byte, string) (plateAssessment, error)
}

type webhookEvent struct {
	Event     string `json:"event"`
	Printer   string `json:"printer"`
	Filename  string `json:"filename"`
	Timestamp string `json:"timestamp"`
}

type plateJob struct {
	PrinterID int
	Event     webhookEvent
	EventTime time.Time
}

type service struct {
	controller          plateController
	assessor            plateAssessor
	logger              *log.Logger
	webhookSecret       string
	snapshotDelay       time.Duration
	confidenceThreshold float64
	eventMaxAge         time.Duration
	timezone            *time.Location
	workerCount         int
	dryRun              bool
	jobs                chan plateJob
	queueMu             sync.RWMutex
	accepting           bool
	workerCancel        context.CancelFunc
	workerWG            sync.WaitGroup
	printerLocksMu      sync.Mutex
	printerLocks        map[int]*sync.Mutex
}

func newService(cfg config, controller plateController, assessor plateAssessor, logger *log.Logger) *service {
	return &service{
		controller:          controller,
		assessor:            assessor,
		logger:              logger,
		webhookSecret:       cfg.WebhookSecret,
		snapshotDelay:       cfg.SnapshotDelay,
		confidenceThreshold: cfg.EmptyConfidenceThreshold,
		eventMaxAge:         cfg.EventMaxAge,
		timezone:            cfg.BambuddyTimezone,
		workerCount:         cfg.WorkerCount,
		dryRun:              cfg.DryRun,
		jobs:                make(chan plateJob, 256),
		printerLocks:        make(map[int]*sync.Mutex),
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
	if !accepting || len(s.jobs) == cap(s.jobs) {
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

	if event.Event != "print_complete" {
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
		s.logger.Printf("rejected stale completion printer_id=%d timestamp=%q", printerID, event.Timestamp)
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
	case s.jobs <- plateJob{PrinterID: printerID, Event: event, EventTime: eventTime}:
		s.logger.Printf("accepted print completion printer_id=%d printer=%q filename=%q", printerID, event.Printer, event.Filename)
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	default:
		s.logger.Printf("job queue full; plate remains gated printer_id=%d", printerID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "job queue full"})
	}
}

func (s *service) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.queueMu.Lock()
	s.workerCancel = cancel
	s.accepting = true
	s.queueMu.Unlock()
	for range s.workerCount {
		s.workerWG.Add(1)
		go func() {
			defer s.workerWG.Done()
			for job := range s.jobs {
				lock := s.printerLock(job.PrinterID)
				lock.Lock()
				s.processJob(ctx, job)
				lock.Unlock()
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

	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		<-done
		return false
	}
}

func (s *service) printerLock(printerID int) *sync.Mutex {
	s.printerLocksMu.Lock()
	defer s.printerLocksMu.Unlock()
	if s.printerLocks[printerID] == nil {
		s.printerLocks[printerID] = &sync.Mutex{}
	}
	return s.printerLocks[printerID]
}

func (s *service) processJob(ctx context.Context, job plateJob) {
	if time.Since(job.EventTime) > s.eventMaxAge {
		s.logger.Printf("ignored stale queued completion printer_id=%d", job.PrinterID)
		return
	}
	initialGate, err := s.controller.gateStatus(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot verify plate gate: %v", job.PrinterID, err)
		return
	}
	if !initialGate.AwaitingPlateClear {
		s.logger.Printf("ignored stale completion printer_id=%d: printer is not awaiting plate clear", job.PrinterID)
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
	initialCompletion, err := s.controller.latestCompletion(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot identify completed queue item: %v", job.PrinterID, err)
		return
	}
	if !completionMatchesEvent(initialCompletion, job.EventTime, s.eventMaxAge) {
		s.logger.Printf("plate remains gated printer_id=%d: webhook does not match latest queue completion", job.PrinterID)
		return
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
	if s.dryRun {
		s.logger.Printf("dry run: would clear plate gate printer_id=%d", job.PrinterID)
		return
	}
	plateClearEnabled, err := s.controller.plateClearEnabled(ctx)
	if err != nil || !plateClearEnabled {
		s.logger.Printf("plate remains gated printer_id=%d: require_plate_clear is not verified enabled: %v", job.PrinterID, err)
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
	currentCompletion, err := s.controller.latestCompletion(ctx, job.PrinterID)
	if err != nil || currentCompletion.ID != initialCompletion.ID {
		s.logger.Printf("plate remains gated printer_id=%d: completed queue item changed during assessment", job.PrinterID)
		return
	}
	localAssessment, err := s.controller.checkPlate(ctx, job.PrinterID, initialCompletion.PlateType)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: Bambuddy plate check failed: %v", job.PrinterID, err)
		return
	}
	s.logger.Printf(
		"Bambuddy plate assessment printer_id=%d empty=%t confidence=%.3f difference=%.3f needs_calibration=%t light_warning=%t message=%q",
		job.PrinterID,
		localAssessment.IsEmpty,
		localAssessment.Confidence,
		localAssessment.Difference,
		localAssessment.NeedsCalibration,
		localAssessment.LightWarning,
		localAssessment.Message,
	)
	if !localAssessment.IsEmpty || localAssessment.NeedsCalibration || localAssessment.LightWarning {
		s.logger.Printf("plate remains gated printer_id=%d: Bambuddy local plate check did not pass", job.PrinterID)
		return
	}
	if err := s.controller.clearPlate(ctx, job.PrinterID); err != nil {
		s.logger.Printf("clear-plate outcome unknown printer_id=%d: request failed after possible delivery: %v", job.PrinterID, err)
		return
	}
	s.logger.Printf("plate gate cleared printer_id=%d; Bambuddy may dispatch the next queued print", job.PrinterID)
}

func (s *service) assessFreshSnapshot(ctx context.Context, printerID int, delay time.Duration) (plateAssessment, error) {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return plateAssessment{}, ctx.Err()
		case <-timer.C:
		}
	}
	image, contentType, err := s.controller.snapshot(ctx, printerID)
	if err != nil {
		return plateAssessment{}, err
	}
	mediaType, err := validateImage(image, contentType)
	if err != nil {
		return plateAssessment{}, err
	}
	return s.assessor.assess(ctx, image, mediaType)
}

func (s *service) safeAssessment(assessment plateAssessment) bool {
	return assessment.PlateVisible && assessment.IsEmpty && assessment.Confidence >= s.confidenceThreshold
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
		before.identity() != "" &&
		before.identity() == after.identity()
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

func completionMatchesEvent(completion completionRecord, eventTime time.Time, maxAge time.Duration) bool {
	delta := eventTime.Sub(completion.CompletedAt)
	return completion.ID > 0 && delta >= 0 && delta <= maxAge
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
