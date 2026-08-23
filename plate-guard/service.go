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
	latestTerminalJob(context.Context, int) (terminalJob, error)
	plateClearEnabled(context.Context) (bool, error)
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
	jobSlots            chan struct{}
	workerSlots         chan struct{}
	queueMu             sync.RWMutex
	accepting           bool
	workerCancel        context.CancelFunc
	workerWG            sync.WaitGroup
}

func newService(cfg config, controller plateController, assessor plateAssessor, logger *log.Logger) *service {
	workerCount := cfg.WorkerCount
	if workerCount < 1 {
		workerCount = 1
	}
	return &service{
		controller:          controller,
		assessor:            assessor,
		logger:              logger,
		webhookSecret:       cfg.WebhookSecret,
		snapshotDelay:       cfg.SnapshotDelay,
		confidenceThreshold: cfg.EmptyConfidenceThreshold,
		eventMaxAge:         cfg.EventMaxAge,
		timezone:            cfg.BambuddyTimezone,
		workerCount:         workerCount,
		dryRun:              cfg.DryRun,
		jobs:                make(chan plateJob, 256),
		jobSlots:            make(chan struct{}, 256),
		workerSlots:         make(chan struct{}, workerCount),
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
	if !accepting || len(s.jobSlots) == cap(s.jobSlots) {
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
	case s.jobSlots <- struct{}{}:
	default:
		s.logger.Printf("job queue full; plate remains gated printer_id=%d", printerID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "job queue full"})
		return
	}
	select {
	case s.jobs <- plateJob{PrinterID: printerID, Event: event, EventTime: eventTime}:
		s.logger.Printf("accepted print completion printer_id=%d printer=%q filename=%q", printerID, event.Printer, event.Filename)
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	default:
		<-s.jobSlots
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
				s.logger.Printf("discarded queued completion during forced shutdown printer_id=%d", printerID)
				return
			}
			select {
			case s.workerSlots <- struct{}{}:
				defer func() { <-s.workerSlots }()
				s.processJob(ctx, job)
			case <-ctx.Done():
				s.logger.Printf("discarded queued completion during forced shutdown printer_id=%d", printerID)
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
	initialTerminal, err := s.controller.latestTerminalJob(ctx, job.PrinterID)
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot identify terminal queue job: %v", job.PrinterID, err)
		return
	}
	if !terminalMatchesEvent(initialTerminal, job.EventTime, s.eventMaxAge) {
		s.logger.Printf("plate remains gated printer_id=%d: webhook does not match latest successful queue completion", job.PrinterID)
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
		before.State == after.State &&
		before.identity() != "" &&
		before.identity() == after.identity()
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

func terminalMatchesEvent(job terminalJob, eventTime time.Time, maxAge time.Duration) bool {
	delta := eventTime.Sub(job.CompletedAt)
	return job.ID > 0 && job.Status == "completed" && delta >= 0 && delta <= maxAge
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
