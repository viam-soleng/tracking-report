package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/utils/rpc"
)

// Our module's model
var (
	ReportGenerator  = resource.NewModel("viam-soleng", "display-tracking", "report-generator")
	errUnimplemented = errors.New("unimplemented")
)

// Register the model in init()
func init() {
	resource.RegisterComponent(sensor.API, ReportGenerator,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newDisplayTrackingReportGenerator,
		},
	)
}

// Config holds any config parameters we need.
type Config struct {
	TrackerName      string `json:"tracker_name"`
	CameraName       string `json:"camera_name"`
	TemporaryDataDir string `json:"temporary_data_dir,omitempty"` // optional attribute
	DataSyncDir      string `json:"data_sync_dir,omitempty"`      // optional attribute
	Interval         int    `json:"interval,omitempty"`           // optional attribute
}

// Validate ensures all parts of the config are valid and returns implicit dependencies.
func (cfg *Config) Validate(path string) ([]string, error) {
	if cfg.TrackerName == "" {
		return nil, fmt.Errorf("%s.tracker_name must not be empty", path)
	}
	if cfg.CameraName == "" {
		return nil, fmt.Errorf("%s.camera_name must not be empty", path)
	}

	// Mark these as dependencies if you want the framework to load them
	deps := []string{cfg.TrackerName, cfg.CameraName}
	return deps, nil
}

// We'll store minimal info about each detection or "trackedObject" in this example.
type TrackedObjectInfo struct {
	FirstSeenDateTime string `json:"first_seen_date_time"`
	FirstSeenUnix     int64  `json:"first_seen_unix"`
	LastSeenDateTime  string `json:"last_seen_date_time"`
	LastSeenUnix      int64  `json:"last_seen_unix"`
	Age               int64  `json:"age,omitempty"`
	ID                string `json:"id,omitempty`
	Label             string `json:"label,omitempty`
}

// displayTrackingReportGenerator is our sensor implementation.
type displayTrackingReportGenerator struct {
	name resource.Name

	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()

	// We'll store an in-memory map of detections
	trackedObjectData map[string]TrackedObjectInfo

	// For daily file management:
	currentDate string // e.g. "2025-02-03"
	currentPath string // e.g. "/data/daily_json/2025-02-03-tracked-objects.json"
	dataSyncDir string // e.g. "/data/sync"
	tempDataDir string // e.g. "/data/json_daily_report" (new field)
	interval    int    // e.g. "/data/json_daily_report" (new field)

	// Tracker to call detections against
	tracker vision.Service

	// We'll use a mutex to protect file writes
	fileLock sync.Mutex
}

// newDisplayTrackingReportGenerator creates a new instance of our sensor.
func newDisplayTrackingReportGenerator(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	// Use defaults if not provided in the config.
	tempDataDir := conf.TemporaryDataDir
	if tempDataDir == "" {
		tempDataDir = "~/data/json_daily_report"
	}
	dataSyncDir := conf.DataSyncDir
	if dataSyncDir == "" {
		dataSyncDir = "/root/.viam/capture"
	}
	interval := conf.Interval
	if interval == 0 {
		interval = 60
	}

	tracker, _ := vision.FromDependencies(deps, conf.TrackerName)

	// Initialize the sensor struct
	s := &displayTrackingReportGenerator{
		name:              rawConf.ResourceName(),
		logger:            logger,
		cfg:               conf,
		cancelCtx:         cancelCtx,
		cancelFunc:        cancelFunc,
		trackedObjectData: make(map[string]TrackedObjectInfo),
		currentDate:       time.Now().Format("2006-01-02"),
		dataSyncDir:       dataSyncDir,
		tempDataDir:       tempDataDir,
		interval:          interval,
		tracker:           tracker,
	}
	// Generate today's file path
	s.currentPath = s.dailyFilePath(s.currentDate, s.tempDataDir)

	// Start our background goroutine
	go s.dataCollectionLoop()

	return s, nil
}

// dataCollectionLoop runs periodically to collect detections & write them to a daily file.
func (s *displayTrackingReportGenerator) dataCollectionLoop() {
	ticker := time.NewTicker(time.Duration(s.interval) * time.Second) // collect every 60 seconds
	defer ticker.Stop()

	s.logger.Info("Starting data collection loop.")
	for {
		select {
		case <-s.cancelCtx.Done():
			s.logger.Info("Data collection loop canceled; exiting.")
			return

		case <-ticker.C:
			// 1) Check if it's a new day
			nowDay := time.Now().Format("2006-01-02")
			if nowDay != s.currentDate {
				s.logger.Info("Day changed; rotating file.")
				// Rotate old file
				s.fileLock.Lock()
				err := s.rotateFile()
				s.fileLock.Unlock()

				if err != nil {
					s.logger.Errorw("failed to rotate file", "error", err)
				}

				// Update to new day
				s.currentDate = nowDay
				s.currentPath = s.dailyFilePath(s.currentDate, s.tempDataDir)
			}

			// 2) Get new detections (this is a placeholder function below)
			detections, err := s.getDetections()
			if err != nil {
				s.logger.Errorw("failed to get detections", "error", err)
				continue
			}
			// 3) If there are detections, lock the file, dump them in there
			if len(detections) > 0 {
				s.fileLock.Lock()
				err := s.writeDetectionsToFile(detections)
				s.fileLock.Unlock()

				if err != nil {
					s.logger.Errorw("failed to write detections to file", "error", err)
				}
			}

		}
	}
}

// getDetections simulates calling your vision service with camera name
func (s *displayTrackingReportGenerator) getDetections() ([]TrackedObjectInfo, error) {
	// Call the vision service to get raw detections as strings.
	// We assume that each string is formatted like "oven_0_20250204_150602".
	s.logger.Info("Getting detections from camera.")
	// s.logger.Info(fmt.Sprintf("Tracker: %v", s.tracker))
	// s.logger.Info(fmt.Sprintf("Camera Name: %v", s.cfg.CameraName))
	rawDetections, err := s.tracker.DetectionsFromCamera(s.cancelCtx, s.cfg.CameraName, map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	detections := make([]TrackedObjectInfo, 0, len(rawDetections))
	for _, detection := range rawDetections {
		class_name := detection.Label()
		parsed, err := newTrackedObjectInfoFromDetection(class_name)
		if err != nil {
			s.logger.Errorw("failed to parse detection", "detection", class_name, "error", err)
			continue
		}

		detections = append(detections, parsed)
	}

	return detections, nil
}

// writeDetectionsToFile opens the current daily file and appends JSON lines
func (s *displayTrackingReportGenerator) writeDetectionsToFile(detections []TrackedObjectInfo) error {
	// Check if the file exists and read its contents if it does.
	var existingDetections map[string][]TrackedObjectInfo
	if _, err := os.Stat(s.currentPath); err == nil {
		data, err := os.ReadFile(s.currentPath)
		if err != nil {
			return err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &existingDetections); err != nil {
				return err
			}
		}
	} else {
		// Initialize the map if the file doesn't exist yet
		existingDetections = make(map[string][]TrackedObjectInfo)
	}

	// Process each incoming detection and group by class (Label)
	for _, newDetection := range detections {
		// Group by class (Label)
		classLabel := newDetection.Label

		// Check if the class label already exists in the map
		if classDetections, exists := existingDetections[classLabel]; exists {
			// Update or add the detection if its ID already exists
			found := false
			for i, existing := range classDetections {
				if existing.ID == newDetection.ID {
					// Update the LastSeen field and Age
					existingDetections[classLabel][i].LastSeenUnix = newDetection.LastSeenUnix
					existingDetections[classLabel][i].LastSeenDateTime = newDetection.LastSeenDateTime
					existingDetections[classLabel][i].Age = newDetection.LastSeenUnix - existingDetections[classLabel][i].FirstSeenUnix
					found = true
					break
				}
			}
			// If the detection ID was not found, append it
			if !found {
				existingDetections[classLabel] = append(existingDetections[classLabel], newDetection)
			}
		} else {
			// If the class label doesn't exist, create a new list and add the detection
			existingDetections[classLabel] = []TrackedObjectInfo{newDetection}
		}
	}

	// Marshal the updated map back to JSON
	out, err := json.MarshalIndent(existingDetections, "", "  ")
	if err != nil {
		return err
	}

	// Overwrite the file with the updated JSON
	if err := os.WriteFile(s.currentPath, out, 0600); err != nil {
		return err
	}

	s.logger.Infof("Updated detections in %s", s.currentPath)
	return nil
}

// newTrackedObjectInfoFromDetection takes a detection string in the format:
// "<detection class>_<detection id>_<YYYYMMDD>_<HHMMSS>"
// and converts it into a TrackedObjectInfo struct.

func newTrackedObjectInfoFromDetection(detectionStr string) (TrackedObjectInfo, error) {
	// Split the detection string
	parts := strings.Split(detectionStr, "_")
	if len(parts) != 4 {
		return TrackedObjectInfo{}, fmt.Errorf("unexpected detection format: %s", detectionStr)
	}

	// Extract class, ID, date, and time from the parts
	detectionClass := parts[0]
	detectionID := parts[1]
	datePart := parts[2] // e.g., "20250204"
	timePart := parts[3] // e.g., "193808" (6 digits)

	// Combine date and time to form the complete timestamp
	dateTimeStr := datePart + timePart // e.g., "202502041938081"

	// Parse the combined dateTime string into a time.Time object
	t, err := time.ParseInLocation("20060102150405", dateTimeStr, time.Local)
	if err != nil {
		return TrackedObjectInfo{}, fmt.Errorf("failed to parse datetime %s: %w", dateTimeStr, err)
	}

	// Format FirstSeen in Unix timestamp (seconds since epoch)
	firstSeenUnix := t.Unix()

	// Get the current time for LastSeen
	lastSeenUnix := time.Now().Unix()

	// Calculate the Age (difference between FirstSeen and LastSeen in seconds)
	age := lastSeenUnix - firstSeenUnix

	// Convert FirstSeen and LastSeen to human-readable format
	firstSeenDateTime := t.Format("2006-01-02 15:04:05 -0700 MST") // YYYY-MM-DD HH:MM:SS ±HHMM TZ
	fmt.Println("Date time:", firstSeenDateTime)
	lastSeenDateTime := time.Now().Format("2006-01-02 15:04:05 -0700 MST") // Current time in human-readable format with timezone

	// Return the tracked object with all parsed values
	return TrackedObjectInfo{
		FirstSeenDateTime: firstSeenDateTime,
		FirstSeenUnix:     firstSeenUnix,
		LastSeenDateTime:  lastSeenDateTime,
		LastSeenUnix:      lastSeenUnix,
		Age:               age,
		ID:                detectionID,
		Label:             detectionClass,
	}, nil
}

// rotateFile moves the current daily file to s.dataSyncDir, creating the directory if needed.
func (s *displayTrackingReportGenerator) rotateFile() error {
	if _, err := os.Stat(s.currentPath); os.IsNotExist(err) {
		// If file doesn't exist, nothing to rotate
		return nil
	}

	// Ensure the target directory exists
	if err := os.MkdirAll(s.dataSyncDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", s.dataSyncDir, err)
	}

	baseName := filepath.Base(s.currentPath)
	newPath := filepath.Join(s.dataSyncDir, baseName)

	if err := os.Rename(s.currentPath, newPath); err != nil {
		return fmt.Errorf("failed to rename file from %s to %s: %w", s.currentPath, newPath, err)
	}

	s.logger.Infof("Rotated file from %s to %s", s.currentPath, newPath)
	return nil
}

// dailyFilePath returns something like "/data/json_daily_report/2025-02-03-tracked-objects.json"
// It ensures that the base directory exists.
func (s *displayTrackingReportGenerator) dailyFilePath(dateStr, baseDir string) string {
	// Create the directory if it does not exist.
	_ = os.MkdirAll(baseDir, 0755)
	return filepath.Join(baseDir, fmt.Sprintf("%s-tracked-objects.json", dateStr))
}

// Name returns the resource name
func (s *displayTrackingReportGenerator) Name() resource.Name {
	return s.name
}

// Reconfigure stops the old loop, updates config, restarts
func (s *displayTrackingReportGenerator) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	s.cancelFunc()

	newConf, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}
	s.cfg = newConf

	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	s.cancelCtx = cancelCtx
	s.cancelFunc = cancelFunc

	s.tracker, err = vision.FromDependencies(deps, s.cfg.TrackerName)

	go s.dataCollectionLoop()
	return nil
}

// NewClientFromConn is not implemented in this example
func (s *displayTrackingReportGenerator) NewClientFromConn(ctx context.Context, conn rpc.ClientConn, remoteName string, name resource.Name, logger logging.Logger) (sensor.Sensor, error) {
	panic("not implemented")
}

// Readings reads today's file, parses each line as a TrackedObjectInfo, and returns a map keyed by ID
func (s *displayTrackingReportGenerator) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	// Lock around file access to avoid race conditions with the dataCollectionLoop
	s.fileLock.Lock()
	defer s.fileLock.Unlock()

	// Open the current day's file
	f, err := os.Open(s.currentPath)
	if err != nil {
		// If file doesn't exist, return empty
		if os.IsNotExist(err) {
			s.logger.Warnf("File %s does not exist; returning empty data", s.currentPath)
			return map[string]interface{}{}, nil
		}
		return nil, err
	}
	defer f.Close()

	// We expect the file to be in the desired map format
	var fileData map[string]interface{}

	// Read and unmarshal the entire file content into the map
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&fileData); err != nil {
		return nil, fmt.Errorf("failed to decode file: %w", err)
	}

	return fileData, nil
}

// DoCommand is unimplemented
func (s *displayTrackingReportGenerator) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

// Close stops the background loop
func (s *displayTrackingReportGenerator) Close(ctx context.Context) error {
	s.cancelFunc()
	return nil
}
