package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fanCommandTimeout  = 5 * time.Second
	fanRetryDelay      = 5 * time.Second
	fanStatusPollDelay = 250 * time.Millisecond
)

var errFanCycleSuperseded = errors.New("fan cycle superseded")

type fanCycleRecord struct {
	PrinterID int             `json:"printer_id"`
	Fans      []string        `json:"fans"`
	ExpiresAt time.Time       `json:"expires_at"`
	Job       plateJob        `json:"job"`
	Gate      plateGateStatus `json:"gate"`
	Terminal  terminalJob     `json:"terminal"`
}

type fanCycle struct {
	mu               sync.Mutex
	record           fanCycleRecord
	generation       uint64
	lastCancelKey    string
	stopRequested    bool
	controlAttempted bool
	operationCancel  context.CancelFunc
	wake             chan struct{}
	done             chan struct{}
}

type fanControlState int

const (
	fanControlOriginal fanControlState = iota
	fanControlSafeToStop
	fanControlRelinquish
)

func newFanCycle(record fanCycleRecord) *fanCycle {
	return &fanCycle{
		record: record,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (c *fanCycle) snapshot() fanCycleRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := c.record
	record.Fans = append([]string(nil), record.Fans...)
	return record
}

func (c *fanCycle) setFans(fans []string) fanCycleRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record.Fans = append([]string(nil), fans...)
	return c.record
}

func (c *fanCycle) setExpiration(expiresAt time.Time) fanCycleRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record.ExpiresAt = expiresAt
	return c.record
}

func (c *fanCycle) currentGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *fanCycle) canceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopRequested
}

func (c *fanCycle) attemptedControl() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.controlAttempted
}

func (c *fanCycle) markControlAttempted() {
	c.mu.Lock()
	c.controlAttempted = true
	c.mu.Unlock()
}

func (c *fanCycle) requestStop(key string) {
	c.mu.Lock()
	if key == c.lastCancelKey {
		c.mu.Unlock()
		return
	}
	c.lastCancelKey = key
	c.stopRequested = true
	c.generation++
	if c.operationCancel != nil {
		c.operationCancel()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	c.mu.Unlock()
}

func (c *fanCycle) operationContext(parent context.Context, generation uint64) (context.Context, context.CancelFunc, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return nil, nil, errFanCycleSuperseded
	}
	ctx, cancel := context.WithTimeout(parent, fanCommandTimeout)
	c.operationCancel = cancel
	return ctx, cancel, nil
}

func postPrintFans(status plateGateStatus, model string) []string {
	var fans []string
	if status.AuxFanSpeed != nil {
		fans = append(fans, "aux")
	}
	if status.ChamberFanSpeed != nil && (!optionalExhaustModel(model) || status.ExhaustFanPresent) {
		fans = append(fans, "chamber")
	}
	if status.LeftAuxFanSpeed != nil {
		fans = append(fans, "aux2")
	}
	return fans
}

func optionalExhaustModel(model string) bool {
	normalized := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToUpper(model))
	return normalized == "P2S" || normalized == "N7" || normalized == "X2D" || normalized == "N6"
}

func (s *service) schedulePostPrintFanCycle(
	ctx context.Context,
	job plateJob,
	gate plateGateStatus,
	terminal terminalJob,
) (bool, error) {
	if s.postPrintFanDuration <= 0 {
		return false, nil
	}
	if !s.dryRun && s.postPrintFanStateFile == "" {
		return false, fmt.Errorf("POST_PRINT_FAN_STATE_FILE is required when the fan cycle is enabled")
	}

	for {
		s.fanMu.Lock()
		if s.fanStopping || s.fanContext == nil {
			s.fanMu.Unlock()
			return false, fmt.Errorf("service is stopping")
		}
		if existing := s.fanCycles[job.PrinterID]; existing != nil {
			existingRecord := existing.snapshot()
			if existingRecord.Terminal.ID == terminal.ID && !existing.canceled() {
				s.fanMu.Unlock()
				s.logger.Printf("post-print fan cycle already active printer_id=%d terminal_job_id=%d", job.PrinterID, terminal.ID)
				return true, nil
			}
			done := existing.done
			if !existing.canceled() {
				existing.requestStop(fmt.Sprintf("terminal-job:%d", terminal.ID))
			}
			s.fanMu.Unlock()
			s.deferJobUntilFanCleanup(ctx, job, done)
			return true, nil
		}
		if _, exists := s.fanRecords[job.PrinterID]; exists {
			s.fanMu.Unlock()
			return false, fmt.Errorf("unreconciled persisted fan state exists for printer %d", job.PrinterID)
		}

		cycle := newFanCycle(fanCycleRecord{
			PrinterID: job.PrinterID,
			Job:       job,
			Gate:      gate,
			Terminal:  terminal,
		})
		s.fanCycles[job.PrinterID] = cycle
		s.fanWG.Add(1)
		s.fanMu.Unlock()
		go s.runPostPrintFanCycle(cycle)
		return true, nil
	}
}

func (s *service) runPostPrintFanCycle(cycle *fanCycle) {
	defer s.finishFanCycle(cycle)
	record := cycle.snapshot()
	generation := cycle.currentGeneration()
	parent := s.fanWorkContext()
	if parent == nil || parent.Err() != nil || cycle.canceled() {
		return
	}

	modelCtx, modelCancel, err := cycle.operationContext(parent, generation)
	if err != nil {
		return
	}
	model, err := s.controller.printerModel(modelCtx, record.PrinterID)
	modelCancel()
	if err != nil {
		s.logger.Printf("plate remains gated printer_id=%d: cannot identify fan capabilities: %v", record.PrinterID, err)
		return
	}
	fans := postPrintFans(record.Gate, model)
	if len(fans) == 0 {
		s.logger.Printf("plate remains gated printer_id=%d: Bambuddy did not report a supported auxiliary or chamber fan", record.PrinterID)
		return
	}
	record = cycle.setFans(fans)
	if !s.dryRun {
		if err := s.addFanRecord(record); err != nil {
			s.logger.Printf("plate remains gated printer_id=%d: persist post-print fan state: %v", record.PrinterID, err)
			return
		}
	}

	if cycle.canceled() {
		s.reconcilePostPrintFanCycle(cycle, false)
		return
	}
	if !s.dryRun {
		if err := s.startFanList(parent, cycle, generation); err != nil {
			s.logger.Printf("plate remains gated printer_id=%d: post-print fan startup failed: %v", record.PrinterID, err)
			s.reconcilePostPrintFanCycle(cycle, false)
			return
		}
	}

	expiresAt := time.Now().Add(s.postPrintFanDuration)
	record = cycle.setExpiration(expiresAt)
	if !s.dryRun {
		if err := s.updateFanRecord(record); err != nil {
			s.logger.Printf("plate remains gated printer_id=%d: persist confirmed fan-cycle deadline: %v", record.PrinterID, err)
			s.reconcilePostPrintFanCycle(cycle, false)
			return
		}
	}
	s.logger.Printf(
		"post-print fan cycle started printer_id=%d fans=%s speed=%d duration=%s dry_run=%t",
		record.PrinterID,
		strings.Join(record.Fans, ","),
		s.postPrintFanSpeed,
		s.postPrintFanDuration,
		s.dryRun,
	)

	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	monitor := time.NewTicker(fanRetryDelay)
	defer monitor.Stop()
	resumeAssessment := false
	waiting := true
	for waiting {
		select {
		case <-timer.C:
			resumeAssessment = true
			waiting = false
		case <-cycle.wake:
			waiting = false
		case <-monitor.C:
			generation = cycle.currentGeneration()
			state, inspectErr := s.inspectFanControl(parent, cycle, generation)
			if inspectErr != nil {
				s.logger.Printf("post-print fan ownership check deferred printer_id=%d: %v", record.PrinterID, inspectErr)
				continue
			}
			if state != fanControlOriginal {
				waiting = false
			}
		}
	}
	s.reconcilePostPrintFanCycle(cycle, resumeAssessment)
}

func (s *service) startFanList(parent context.Context, cycle *fanCycle, generation uint64) error {
	record := cycle.snapshot()
	for _, fan := range record.Fans {
		state, err := s.inspectFanControl(parent, cycle, generation)
		if err != nil {
			return err
		}
		if state != fanControlOriginal {
			return errFanCycleSuperseded
		}
		fanCtx, cancel, err := cycle.operationContext(parent, generation)
		if err != nil {
			return err
		}
		cycle.markControlAttempted()
		err = s.controller.setFanSpeed(fanCtx, record.PrinterID, fan, s.postPrintFanSpeed)
		cancel()
		if err != nil {
			return fmt.Errorf("start %s fan: %w", fan, err)
		}
	}
	if err := s.waitForFanState(parent, cycle, generation, s.postPrintFanSpeed, true); err != nil {
		return fmt.Errorf("verify post-print fan startup: %w", err)
	}
	return nil
}

func (s *service) reconcilePostPrintFanCycle(cycle *fanCycle, resumeAssessment bool) {
	record := cycle.snapshot()
	if s.dryRun {
		for {
			parent := s.fanWorkContext()
			if parent == nil || parent.Err() != nil {
				return
			}
			generation := cycle.currentGeneration()
			state, err := s.inspectFanControl(parent, cycle, generation)
			if errors.Is(err, errFanCycleSuperseded) {
				continue
			}
			if err == nil {
				if resumeAssessment && state == fanControlOriginal && cycle.currentGeneration() == generation {
					s.enqueueFanContinuation(parent, record.Job)
				}
				return
			}
			s.logger.Printf("dry-run post-print reconciliation deferred printer_id=%d: %v", record.PrinterID, err)
			if !s.waitForFanRetry(parent, cycle) {
				return
			}
		}
	}
	if !s.dryRun && !cycle.attemptedControl() {
		if err := s.removeFanRecord(record.PrinterID); err != nil {
			s.logger.Printf("post-print unused fan-state cleanup failed printer_id=%d: %v", record.PrinterID, err)
			s.retryFanReconciliation(cycle)
		}
		return
	}

	for {
		parent := s.fanWorkContext()
		if parent == nil || parent.Err() != nil {
			s.markFanCleanupFailed(record.PrinterID, parent)
			return
		}
		generation := cycle.currentGeneration()
		state, err := s.inspectFanControl(parent, cycle, generation)
		if errors.Is(err, errFanCycleSuperseded) {
			continue
		}
		if err == nil && state == fanControlRelinquish {
			if removeErr := s.removeFanRecord(record.PrinterID); removeErr == nil {
				s.logger.Printf("post-print fan control relinquished after printer ownership changed printer_id=%d", record.PrinterID)
				return
			} else {
				err = removeErr
			}
		}
		allOriginal := false
		if err == nil {
			allOriginal, err = s.stopFanList(parent, cycle, generation)
			if errors.Is(err, errFanCycleSuperseded) {
				continue
			}
		}
		if err == nil {
			if removeErr := s.removeFanRecord(record.PrinterID); removeErr != nil {
				err = removeErr
			} else {
				s.logger.Printf("post-print fan cycle stopped printer_id=%d fans=%s", record.PrinterID, strings.Join(record.Fans, ","))
				if resumeAssessment && allOriginal && cycle.currentGeneration() == generation {
					if !s.enqueueFanContinuation(parent, record.Job) {
						s.logger.Printf("plate remains gated printer_id=%d: service stopped before post-print assessment could be queued", record.PrinterID)
					}
				}
				return
			}
		}
		s.logger.Printf("post-print fan reconciliation deferred printer_id=%d: %v", record.PrinterID, err)
		if !s.waitForFanRetry(parent, cycle) {
			s.markFanCleanupFailed(record.PrinterID, parent)
			return
		}
	}
}

func (s *service) retryFanReconciliation(cycle *fanCycle) {
	for {
		parent := s.fanWorkContext()
		if parent == nil || !s.waitForFanRetry(parent, cycle) {
			s.markFanCleanupFailed(cycle.snapshot().PrinterID, parent)
			return
		}
		if err := s.removeFanRecord(cycle.snapshot().PrinterID); err == nil {
			return
		}
	}
}

func (s *service) inspectFanControl(parent context.Context, cycle *fanCycle, generation uint64) (fanControlState, error) {
	record := cycle.snapshot()
	statusCtx, statusCancel, err := cycle.operationContext(parent, generation)
	if err != nil {
		return fanControlRelinquish, err
	}
	status, err := s.controller.gateStatus(statusCtx, record.PrinterID)
	statusCancel()
	if err != nil {
		return fanControlRelinquish, fmt.Errorf("recheck printer state: %w", err)
	}
	if status.ID != record.PrinterID || !status.Connected {
		return fanControlRelinquish, fmt.Errorf("printer status is unavailable or mismatched")
	}
	if activePrinterState(status.State) || !status.AwaitingPlateClear {
		return fanControlRelinquish, nil
	}
	if !terminalPrinterState(status.State) {
		return fanControlRelinquish, fmt.Errorf("printer state %q is neither an owned terminal gate nor an active replacement", status.State)
	}

	terminalCtx, terminalCancel, err := cycle.operationContext(parent, generation)
	if err != nil {
		return fanControlRelinquish, err
	}
	terminal, err := s.controller.latestTerminalJob(terminalCtx, record.PrinterID)
	terminalCancel()
	if err != nil {
		return fanControlRelinquish, fmt.Errorf("recheck terminal queue job: %w", err)
	}
	if sameFanCycleGate(record.Gate, status) && sameTerminalJob(record.Terminal, terminal) {
		return fanControlOriginal, nil
	}
	return fanControlSafeToStop, nil
}

func activePrinterState(state string) bool {
	switch strings.ToUpper(state) {
	case "PREPARE", "SLICING", "RUNNING", "PAUSE":
		return true
	default:
		return false
	}
}

func terminalPrinterState(state string) bool {
	switch strings.ToUpper(state) {
	case "FINISH", "FAILED", "IDLE":
		return true
	default:
		return false
	}
}

func sameFanCycleGate(before, after plateGateStatus) bool {
	return before.AwaitingPlateClear &&
		after.AwaitingPlateClear &&
		before.ID == after.ID &&
		before.Name == after.Name &&
		terminalPrinterState(before.State) &&
		terminalPrinterState(after.State) &&
		before.identity() != "" &&
		before.identity() == after.identity()
}

func (s *service) stopFanList(parent context.Context, cycle *fanCycle, generation uint64) (bool, error) {
	record := cycle.snapshot()
	allOriginal := true
	for _, fan := range record.Fans {
		state, err := s.inspectFanControl(parent, cycle, generation)
		if err != nil {
			return false, err
		}
		if state == fanControlRelinquish {
			return false, errFanCycleSuperseded
		}
		allOriginal = allOriginal && state == fanControlOriginal
		fanCtx, cancel, err := cycle.operationContext(parent, generation)
		if err != nil {
			return false, err
		}
		err = s.controller.setFanSpeed(fanCtx, record.PrinterID, fan, 0)
		cancel()
		if err != nil {
			return false, fmt.Errorf("stop %s fan: %w", fan, err)
		}
	}
	if err := s.waitForFanState(parent, cycle, generation, 0, false); err != nil {
		return false, err
	}
	state, err := s.inspectFanControl(parent, cycle, generation)
	if err != nil {
		return false, err
	}
	if state == fanControlRelinquish {
		return false, errFanCycleSuperseded
	}
	return allOriginal && state == fanControlOriginal, nil
}

func (s *service) waitForFanState(
	parent context.Context,
	cycle *fanCycle,
	generation uint64,
	speed int,
	requireOriginal bool,
) error {
	record := cycle.snapshot()
	deadline := time.Now().Add(fanCommandTimeout)
	for {
		state, err := s.inspectFanControl(parent, cycle, generation)
		if err != nil {
			return err
		}
		if state == fanControlRelinquish || (requireOriginal && state != fanControlOriginal) {
			return errFanCycleSuperseded
		}
		statusCtx, statusCancel, err := cycle.operationContext(parent, generation)
		if err != nil {
			return err
		}
		status, err := s.controller.gateStatus(statusCtx, record.PrinterID)
		statusCancel()
		if err == nil && fanStateMatches(status, record.Fans, speed) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("Bambuddy did not report fan speed %d before timeout", speed)
		}
		timer := time.NewTimer(fanStatusPollDelay)
		select {
		case <-timer.C:
		case <-cycle.wake:
			timer.Stop()
			return errFanCycleSuperseded
		case <-parent.Done():
			timer.Stop()
			return parent.Err()
		}
	}
}

func fanStateMatches(status plateGateStatus, fans []string, speed int) bool {
	for _, fan := range fans {
		var reported *int
		switch fan {
		case "aux":
			reported = status.AuxFanSpeed
		case "chamber":
			reported = status.ChamberFanSpeed
		case "aux2":
			reported = status.LeftAuxFanSpeed
		}
		if reported == nil || !fanSpeedMatches(*reported, speed) {
			return false
		}
	}
	return true
}

func fanSpeedMatches(reported, requested int) bool {
	pwm := math.Round(float64(requested) * 255 / 100)
	level := math.Round(pwm * 15 / 255)
	quantized := int(math.Round(level * 100 / 15))
	return abs(reported-requested) <= 1 || abs(reported-quantized) <= 1
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (s *service) enqueueFanContinuation(ctx context.Context, job plateJob) bool {
	job.AfterFanCycle = true
	return s.enqueueInternalJob(ctx, job, "post-print assessment queued after fan cycle")
}

func (s *service) enqueueInternalJob(ctx context.Context, job plateJob, message string) bool {
	for {
		s.queueMu.RLock()
		if !s.accepting {
			s.queueMu.RUnlock()
			return false
		}
		reserved := false
		select {
		case s.jobSlots <- struct{}{}:
			reserved = true
		default:
		}
		if reserved {
			select {
			case s.jobs <- job:
				s.queueMu.RUnlock()
				s.logger.Printf("%s printer_id=%d", message, job.PrinterID)
				return true
			default:
				<-s.jobSlots
			}
		}
		s.queueMu.RUnlock()

		retry := time.NewTimer(100 * time.Millisecond)
		select {
		case <-retry.C:
		case <-ctx.Done():
			retry.Stop()
			return false
		}
	}
}

func fanEventKey(event webhookEvent, eventTime time.Time) string {
	return event.Event + "|" + eventTime.UTC().Format(time.RFC3339Nano) + "|" + normalizePrintName(event.Filename)
}

func (s *service) cancelFanCycleForEvent(printerID int, event webhookEvent, eventTime time.Time) (<-chan struct{}, bool) {
	s.fanMu.Lock()
	cycle := s.fanCycles[printerID]
	if cycle == nil {
		s.fanMu.Unlock()
		return nil, false
	}
	record := cycle.snapshot()
	if event.Event == "print_complete" &&
		normalizePrintName(record.Job.Event.Filename) == normalizePrintName(event.Filename) {
		s.fanMu.Unlock()
		return cycle.done, true
	}
	cycle.requestStop(fanEventKey(event, eventTime))
	done := cycle.done
	s.fanMu.Unlock()
	s.logger.Printf("post-print fan cycle cancellation requested by event=%q printer_id=%d", event.Event, printerID)
	return done, true
}

func (s *service) deferCanceledFanCycle(ctx context.Context, job plateJob) bool {
	done, exists := s.cancelFanCycleForEvent(job.PrinterID, job.Event, job.EventTime)
	if !exists {
		return false
	}
	s.deferJobUntilFanCleanup(ctx, job, done)
	return true
}

func (s *service) deferJobUntilFanCleanup(ctx context.Context, job plateJob, done <-chan struct{}) {
	job.AfterFanCleanup = true
	s.workerWG.Add(1)
	go func() {
		defer s.workerWG.Done()
		select {
		case <-done:
			s.enqueueInternalJob(ctx, job, "lifecycle event requeued after fan cleanup")
		case <-ctx.Done():
		}
	}()
}

func (s *service) stopFanCycles(ctx context.Context) {
	s.fanMu.Lock()
	s.fanStopping = true
	s.fanDrainContext = ctx
	cycles := make([]*fanCycle, 0, len(s.fanCycles))
	for _, cycle := range s.fanCycles {
		cycles = append(cycles, cycle)
	}
	s.fanMu.Unlock()
	for _, cycle := range cycles {
		cycle.requestStop("service-shutdown")
	}
}

func (s *service) fanWorkContext() context.Context {
	s.fanMu.Lock()
	defer s.fanMu.Unlock()
	if s.fanStopping {
		return s.fanDrainContext
	}
	return s.fanContext
}

func (s *service) waitForFanRetry(ctx context.Context, cycle *fanCycle) bool {
	timer := time.NewTimer(fanRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-cycle.wake:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *service) markFanCleanupFailed(printerID int, ctx context.Context) {
	s.fanMu.Lock()
	s.fanCleanupFailed = true
	s.fanMu.Unlock()
	err := context.Canceled
	if ctx != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	s.logger.Printf("post-print fan cleanup stopped before acknowledgement printer_id=%d: %v", printerID, err)
}

func (s *service) finishFanCycle(cycle *fanCycle) {
	record := cycle.snapshot()
	s.fanMu.Lock()
	if s.fanCycles[record.PrinterID] == cycle {
		delete(s.fanCycles, record.PrinterID)
	}
	close(cycle.done)
	s.fanMu.Unlock()
	s.fanWG.Done()
}

func (s *service) fanStateReady() bool {
	s.fanMu.Lock()
	defer s.fanMu.Unlock()
	if s.fanCleanupFailed {
		return false
	}
	for printerID := range s.fanRecords {
		if s.fanCycles[printerID] == nil {
			return false
		}
	}
	return true
}

func (s *service) addFanRecord(record fanCycleRecord) error {
	if s.dryRun {
		return nil
	}
	return s.mutateFanRecords(
		func(records map[int]fanCycleRecord) error {
			if _, exists := records[record.PrinterID]; exists {
				return fmt.Errorf("fan state already exists for printer %d", record.PrinterID)
			}
			record.Fans = append([]string(nil), record.Fans...)
			records[record.PrinterID] = record
			return nil
		},
	)
}

func (s *service) updateFanRecord(record fanCycleRecord) error {
	if s.dryRun {
		return nil
	}
	return s.mutateFanRecords(
		func(records map[int]fanCycleRecord) error {
			if _, exists := records[record.PrinterID]; !exists {
				return fmt.Errorf("fan state is missing for printer %d", record.PrinterID)
			}
			record.Fans = append([]string(nil), record.Fans...)
			records[record.PrinterID] = record
			return nil
		},
	)
}

func (s *service) removeFanRecord(printerID int) error {
	if s.dryRun {
		return nil
	}
	return s.mutateFanRecords(
		func(records map[int]fanCycleRecord) error {
			delete(records, printerID)
			return nil
		},
	)
}

func (s *service) mutateFanRecords(mutate func(map[int]fanCycleRecord) error) error {
	s.fanPersistMu.Lock()
	defer s.fanPersistMu.Unlock()

	s.fanMu.Lock()
	before := cloneFanRecords(s.fanRecords)
	if err := mutate(s.fanRecords); err != nil {
		s.fanMu.Unlock()
		return err
	}
	data, err := json.Marshal(s.fanRecords)
	s.fanMu.Unlock()
	if err == nil {
		err = s.writeFanRecords(data)
	}
	if err != nil {
		s.fanMu.Lock()
		s.fanRecords = before
		s.fanMu.Unlock()
		return err
	}
	return nil
}

func cloneFanRecords(records map[int]fanCycleRecord) map[int]fanCycleRecord {
	clone := make(map[int]fanCycleRecord, len(records))
	for printerID, record := range records {
		record.Fans = append([]string(nil), record.Fans...)
		clone[printerID] = record
	}
	return clone
}

func (s *service) writeFanRecords(data []byte) error {
	directory := filepath.Dir(s.postPrintFanStateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".fan-cycles-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, s.postPrintFanStateFile); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}

func (s *service) ensureFanStateWritable() error {
	directory := filepath.Dir(s.postPrintFanStateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".fan-state-check-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		os.Remove(name)
		return closeErr
	}
	return os.Remove(name)
}

func (s *service) recoverPostPrintFans(ctx context.Context) error {
	if s.postPrintFanStateFile == "" {
		if s.postPrintFanDuration > 0 && !s.dryRun {
			return fmt.Errorf("POST_PRINT_FAN_STATE_FILE is required when the fan cycle is enabled")
		}
		return nil
	}
	if s.postPrintFanDuration > 0 && !s.dryRun {
		if err := s.ensureFanStateWritable(); err != nil {
			return fmt.Errorf("prepare fan state directory: %w", err)
		}
	}
	data, err := os.ReadFile(s.postPrintFanStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read persisted fan state: %w", err)
	}
	var records map[int]fanCycleRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("decode persisted fan state: %w", err)
	}
	for printerID, record := range records {
		if err := validateFanRecord(printerID, record); err != nil {
			return err
		}
	}
	if s.dryRun && len(records) > 0 {
		return fmt.Errorf("persisted live fan state requires non-dry-run recovery")
	}
	s.fanMu.Lock()
	s.fanRecords = cloneFanRecords(records)
	s.fanMu.Unlock()

	var wg sync.WaitGroup
	errorsByPrinter := make(chan error, len(records))
	for _, record := range records {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			cycle := newFanCycle(record)
			cycle.controlAttempted = true
			state, inspectErr := s.inspectFanControl(ctx, cycle, 0)
			if inspectErr != nil {
				errorsByPrinter <- fmt.Errorf("recover fan state for printer %d: %w", record.PrinterID, inspectErr)
				return
			}
			if state != fanControlRelinquish {
				if _, stopErr := s.stopFanList(ctx, cycle, 0); stopErr != nil {
					errorsByPrinter <- fmt.Errorf("recover fan state for printer %d: %w", record.PrinterID, stopErr)
					return
				}
			}
			if removeErr := s.removeFanRecord(record.PrinterID); removeErr != nil {
				errorsByPrinter <- fmt.Errorf("recover fan state for printer %d: %w", record.PrinterID, removeErr)
				return
			}
			if state == fanControlRelinquish {
				s.logger.Printf("persisted fan control relinquished after printer ownership changed printer_id=%d", record.PrinterID)
			} else {
				s.logger.Printf("persisted post-print fans stopped printer_id=%d fans=%s", record.PrinterID, strings.Join(record.Fans, ","))
			}
		}()
	}
	wg.Wait()
	close(errorsByPrinter)
	var recoveryErrors []error
	for recoveryErr := range errorsByPrinter {
		recoveryErrors = append(recoveryErrors, recoveryErr)
	}
	return errors.Join(recoveryErrors...)
}

func validateFanRecord(printerID int, record fanCycleRecord) error {
	if printerID <= 0 || record.PrinterID != printerID || len(record.Fans) == 0 {
		return fmt.Errorf("persisted fan state contains an invalid record")
	}
	seen := make(map[string]bool, len(record.Fans))
	for _, fan := range record.Fans {
		if fan != "aux" && fan != "chamber" && fan != "aux2" {
			return fmt.Errorf("persisted fan state contains unsupported fan %q", fan)
		}
		if seen[fan] {
			return fmt.Errorf("persisted fan state contains duplicate fan %q", fan)
		}
		seen[fan] = true
	}
	return nil
}
