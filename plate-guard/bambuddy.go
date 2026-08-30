package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBambuddyResponseBytes = 4 << 20
const maxQueueResponseBytes = 64 << 20

type bambuddyClient struct {
	baseURL    *url.URL
	apiKey     string
	timezone   *time.Location
	httpClient *http.Client
}

type bambuddySettings struct {
	RequirePlateClear  bool `json:"require_plate_clear"`
	CaptureFinishPhoto bool `json:"capture_finish_photo"`
}

type plateGateStatus struct {
	ID                 int    `json:"id"`
	Name               string `json:"name"`
	Connected          bool   `json:"connected"`
	AwaitingPlateClear bool   `json:"awaiting_plate_clear"`
	State              string `json:"state"`
	CurrentPrint       string `json:"current_print"`
	SubtaskName        string `json:"subtask_name"`
	GcodeFile          string `json:"gcode_file"`
	LayerNum           int    `json:"layer_num"`
	LeftAuxFanSpeed    *int   `json:"left_aux_fan_speed"`
}

type terminalJob struct {
	ID          int
	CompletedAt time.Time
	PlateType   string
	Status      string
}

type activeQueueJob struct {
	ID        int
	StartedAt time.Time
	Status    string
}

func newBambuddyClient(rawURL, apiKey string, timezone *time.Location, httpClient *http.Client) (*bambuddyClient, error) {
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse BAMBUDDY_URL: %w", err)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("BAMBUDDY_URL must use http or https")
	}
	if baseURL.Host == "" {
		return nil, fmt.Errorf("BAMBUDDY_URL must include a host")
	}
	return &bambuddyClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		timezone:   timezone,
		httpClient: httpClient,
	}, nil
}

func (c *bambuddyClient) ensurePlateClearGate(ctx context.Context, autoEnable bool) (bambuddySettings, error) {
	settings, err := c.getSettings(ctx)
	if err != nil {
		return bambuddySettings{}, fmt.Errorf("read Bambuddy settings: %w", err)
	}
	if settings.RequirePlateClear {
		return settings, nil
	}
	if !autoEnable {
		return settings, fmt.Errorf("Bambuddy require_plate_clear is disabled")
	}

	body, err := json.Marshal(map[string]bool{"require_plate_clear": true})
	if err != nil {
		return settings, err
	}
	req, err := c.newRequest(ctx, http.MethodPatch, "/api/v1/settings", bytes.NewReader(body))
	if err != nil {
		return settings, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return settings, fmt.Errorf("enable require_plate_clear: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return settings, responseError("enable require_plate_clear", resp)
	}

	var updated bambuddySettings
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBambuddyResponseBytes)).Decode(&updated); err != nil {
		return settings, fmt.Errorf("decode updated Bambuddy settings: %w", err)
	}
	if !updated.RequirePlateClear {
		return settings, fmt.Errorf("Bambuddy accepted the settings update but require_plate_clear is still disabled")
	}
	return updated, nil
}

func (c *bambuddyClient) getSettings(ctx context.Context) (bambuddySettings, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/settings", nil)
	if err != nil {
		return bambuddySettings{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return bambuddySettings{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bambuddySettings{}, responseError("get Bambuddy settings", resp)
	}

	var settings bambuddySettings
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBambuddyResponseBytes)).Decode(&settings); err != nil {
		return bambuddySettings{}, fmt.Errorf("decode Bambuddy settings: %w", err)
	}
	return settings, nil
}

func (c *bambuddyClient) plateClearEnabled(ctx context.Context) (bool, error) {
	settings, err := c.getSettings(ctx)
	if err != nil {
		return false, err
	}
	return settings.RequirePlateClear, nil
}

func (c *bambuddyClient) snapshot(ctx context.Context, printerID int) ([]byte, string, error) {
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/camera/snapshot"
	u := c.endpoint(path)
	token, err := c.cameraAccessToken(ctx)
	if err != nil {
		return nil, "", err
	}
	if token != "" {
		query := u.Query()
		query.Set("token", token)
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	c.addAuthentication(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("capture Bambuddy snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", responseError("capture Bambuddy snapshot", resp)
	}
	image, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read Bambuddy snapshot: %w", err)
	}
	if len(image) > maxImageBytes {
		return nil, "", fmt.Errorf("Bambuddy snapshot exceeds %d bytes", maxImageBytes)
	}
	return image, resp.Header.Get("Content-Type"), nil
}

func (c *bambuddyClient) cameraAccessToken(ctx context.Context) (string, error) {
	if c.apiKey == "" {
		return "", nil
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/printers/camera/stream-token", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create Bambuddy camera token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", responseError("create Bambuddy camera token", resp)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBambuddyResponseBytes)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Bambuddy camera token: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("Bambuddy returned an empty camera token")
	}
	return result.Token, nil
}

func (c *bambuddyClient) gateStatus(ctx context.Context, printerID int) (plateGateStatus, error) {
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/status"
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return plateGateStatus{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return plateGateStatus{}, fmt.Errorf("get Bambuddy plate gate status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plateGateStatus{}, responseError("get Bambuddy plate gate status", resp)
	}

	var status plateGateStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBambuddyResponseBytes)).Decode(&status); err != nil {
		return plateGateStatus{}, fmt.Errorf("decode Bambuddy plate gate status: %w", err)
	}
	return status, nil
}

func (c *bambuddyClient) latestTerminalJob(ctx context.Context, printerID int) (terminalJob, error) {
	u := c.endpoint("/api/v1/queue/")
	query := u.Query()
	query.Set("printer_id", strconv.Itoa(printerID))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return terminalJob{}, err
	}
	c.addAuthentication(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return terminalJob{}, fmt.Errorf("get latest Bambuddy terminal queue job: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return terminalJob{}, responseError("get latest Bambuddy terminal queue job", resp)
	}

	var items []struct {
		ID          int    `json:"id"`
		PrinterID   int    `json:"printer_id"`
		CompletedAt string `json:"completed_at"`
		PlateType   string `json:"bed_type"`
		Status      string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxQueueResponseBytes)).Decode(&items); err != nil {
		return terminalJob{}, fmt.Errorf("decode Bambuddy queue jobs: %w", err)
	}
	var latest terminalJob
	for _, item := range items {
		if item.PrinterID != printerID || !isTerminalQueueStatus(item.Status) {
			continue
		}
		if item.ID <= 0 || item.CompletedAt == "" {
			return terminalJob{}, fmt.Errorf("Bambuddy returned an invalid terminal queue job for printer %d", printerID)
		}
		completedAt, err := parseBambuddyTime(item.CompletedAt, c.timezone)
		if err != nil {
			return terminalJob{}, fmt.Errorf("parse terminal queue job %d timestamp: %w", item.ID, err)
		}
		if latest.ID == 0 || completedAt.After(latest.CompletedAt) {
			latest = terminalJob{
				ID:          item.ID,
				CompletedAt: completedAt,
				PlateType:   item.PlateType,
				Status:      strings.ToLower(item.Status),
			}
		}
	}
	if latest.ID == 0 {
		return terminalJob{}, fmt.Errorf("Bambuddy has no terminal queue job for printer %d", printerID)
	}
	return latest, nil
}

func (c *bambuddyClient) activeQueueJob(ctx context.Context, printerID int) (activeQueueJob, error) {
	u := c.endpoint("/api/v1/queue/")
	query := u.Query()
	query.Set("printer_id", strconv.Itoa(printerID))
	query.Set("status", "printing")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return activeQueueJob{}, err
	}
	c.addAuthentication(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return activeQueueJob{}, fmt.Errorf("get active Bambuddy queue job: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return activeQueueJob{}, responseError("get active Bambuddy queue job", resp)
	}

	var items []struct {
		ID        int    `json:"id"`
		PrinterID int    `json:"printer_id"`
		StartedAt string `json:"started_at"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxQueueResponseBytes)).Decode(&items); err != nil {
		return activeQueueJob{}, fmt.Errorf("decode active Bambuddy queue jobs: %w", err)
	}
	var active activeQueueJob
	for _, item := range items {
		if item.PrinterID != printerID || strings.ToLower(item.Status) != "printing" {
			continue
		}
		if active.ID != 0 {
			return activeQueueJob{}, fmt.Errorf("Bambuddy returned multiple active queue jobs for printer %d", printerID)
		}
		if item.ID <= 0 || item.StartedAt == "" {
			return activeQueueJob{}, fmt.Errorf("Bambuddy returned an invalid active queue job for printer %d", printerID)
		}
		startedAt, err := parseBambuddyTime(item.StartedAt, c.timezone)
		if err != nil {
			return activeQueueJob{}, fmt.Errorf("parse active queue job %d timestamp: %w", item.ID, err)
		}
		active = activeQueueJob{ID: item.ID, StartedAt: startedAt, Status: "printing"}
	}
	if active.ID == 0 {
		return activeQueueJob{}, fmt.Errorf("Bambuddy has no active queue job for printer %d", printerID)
	}
	return active, nil
}

func isTerminalQueueStatus(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "failed", "cancelled", "aborted", "skipped":
		return true
	default:
		return false
	}
}

func (c *bambuddyClient) clearPlate(ctx context.Context, printerID int) error {
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/clear-plate"
	req, err := c.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("clear Bambuddy plate gate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError("clear Bambuddy plate gate", resp)
	}
	return nil
}

func (c *bambuddyClient) pausePrint(ctx context.Context, printerID int) error {
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/print/pause"
	req, err := c.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pause Bambuddy print: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError("pause Bambuddy print", resp)
	}
	return nil
}

func (c *bambuddyClient) enableAMSFilamentBackup(ctx context.Context, printerID int) error {
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/ams-backup"
	u := c.endpoint(path)
	query := u.Query()
	query.Set("enabled", "true")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	c.addAuthentication(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("enable AMS filament backup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError("enable AMS filament backup", resp)
	}
	return nil
}

func (c *bambuddyClient) setFanSpeed(ctx context.Context, printerID int, fan string, speed int) error {
	switch fan {
	case "aux", "aux2", "chamber":
	default:
		return fmt.Errorf("unsupported fan %q", fan)
	}
	if speed < 0 || speed > 100 {
		return fmt.Errorf("fan speed must be between 0 and 100")
	}
	path := "/api/v1/printers/" + strconv.Itoa(printerID) + "/fan-speed"
	u := c.endpoint(path)
	query := u.Query()
	query.Set("fan", fan)
	query.Set("speed", strconv.Itoa(speed))
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	c.addAuthentication(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("set Bambuddy %s fan speed to %d: %w", fan, speed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(fmt.Sprintf("set Bambuddy %s fan speed to %d", fan, speed), resp)
	}
	return nil
}

func (c *bambuddyClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path).String(), body)
	if err != nil {
		return nil, err
	}
	c.addAuthentication(req)
	return req, nil
}

func (c *bambuddyClient) addAuthentication(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
}

func (c *bambuddyClient) endpoint(path string) *url.URL {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

func responseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d: %s", operation, resp.StatusCode, detail)
}

func parseBambuddyTime(value string, location *time.Location) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	if location == nil {
		location = time.UTC
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported Bambuddy timestamp %q", value)
}
