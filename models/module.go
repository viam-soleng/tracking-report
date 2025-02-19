package models

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
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
	IsFull            bool   `json:"is_full"`
	LastFull          string `json:"last_full_date_time"`
	LastFullUnix      int64  `json:"last_seen_unix"`
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

	tempDataDir := conf.TemporaryDataDir
	if tempDataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(fmt.Errorf("failed to get user home directory: %w", err))
		}
		tempDataDir = filepath.Join(homeDir, "data", "json_daily_report")
	} else {
		if strings.HasPrefix(tempDataDir, "~") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				panic(fmt.Errorf("failed to get user home directory: %w", err))
			}
			tempDataDir = filepath.Join(homeDir, strings.TrimPrefix(tempDataDir, "~"))
		}
	}

	// **Log the final path** where we plan to write files
	logger.Infof("Using Temporary Data Directory: %s", tempDataDir)

	// You can also confirm the current working directory:
	pwd, err := os.Getwd()
	if err == nil {
		logger.Infof("Current working directory is: %s", pwd)
	} else {
		logger.Errorw("Failed to get working directory", "error", err)
	}

	dataSyncDir := conf.DataSyncDir
	if dataSyncDir == "" {
		dataSyncDir = "/root/.viam/capture"
	} else if strings.HasPrefix(dataSyncDir, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(fmt.Errorf("failed to get user home directory: %w", err))
		}
		dataSyncDir = filepath.Join(homeDir, strings.TrimPrefix(dataSyncDir, "~"))
	}

	logger.Infof("Using Data Sync Directory: %s", dataSyncDir)

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
	ticker := time.NewTicker(time.Duration(s.interval) * time.Second) // collect every s.interval seconds
	defer ticker.Stop()

	s.logger.Info("Starting data collection loop.")
	for {
		select {
		case <-s.cancelCtx.Done():
			s.logger.Info("Data collection loop canceled; exiting.")
			return

		case <-ticker.C:
			// 1) Get new detections.
			detections, err := s.getDetections()
			if err != nil {
				s.logger.Errorw("failed to get detections", "error", err)
				continue
			}
			// 2) If there are detections, lock the file, and write them.
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
	// s.logger.Debugf("Getting detections from camera.")
	// s.logger.Info(fmt.Sprintf("Tracker: %v", s.tracker))
	// s.logger.Info(fmt.Sprintf("Camera Name: %v", s.cfg.CameraName))
	rawDetections, err := s.tracker.DetectionsFromCamera(s.cancelCtx, s.cfg.CameraName, map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	detections := make([]TrackedObjectInfo, 0, len(rawDetections))

	// s.logger.Debugf("Raw Detections: ", rawDetections)
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
					// Update the last seen values
					existingDetections[classLabel][i].LastSeenUnix = newDetection.LastSeenUnix
					existingDetections[classLabel][i].LastSeenDateTime = newDetection.LastSeenDateTime
					existingDetections[classLabel][i].Age = newDetection.LastSeenUnix - existingDetections[classLabel][i].FirstSeenUnix

					// If the new detection is "full", update the last full seen fields.
					if newDetection.IsFull {
						existingDetections[classLabel][i].IsFull = true
						existingDetections[classLabel][i].LastFullUnix = newDetection.LastSeenUnix
						existingDetections[classLabel][i].LastFull = newDetection.LastSeenDateTime
					}
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
	// s.logger.Debugf("Updated detections in %s", s.currentPath)
	return nil
}

// newTrackedObjectInfoFromDetection takes a detection string in the format:
// "<detection class>_<detection id>_<YYYYMMDD>_<HHMMSS>"
// and converts it into a TrackedObjectInfo struct.

func newTrackedObjectInfoFromDetection(detectionStr string) (TrackedObjectInfo, error) {
	// Split the detection string
	parts := strings.Split(detectionStr, "_")
	if len(parts) > 5 {
		return TrackedObjectInfo{}, fmt.Errorf("unexpected detection format: %s", detectionStr)
	}

	// Extract class, ID, date, and time from the parts
	detectionClass := parts[0]
	detectionID := parts[1]
	datePart := parts[2] // e.g., "20250204"
	timePart := parts[3] // e.g., "193808" (6 digits)

	// Set default status to partial
	isFull := true
	if len(parts) == 5 {
		// The fifth part must be either "full" or "partial"
		status := parts[4]
		switch status {
		case "full":
			isFull = true
		case "partial":
			isFull = false
		default:
			return TrackedObjectInfo{}, fmt.Errorf("unexpected status '%s' in detection: %s", status, detectionStr)
		}
	}

	// Combine date and time to form the complete timestamp
	dateTimeStr := datePart + timePart // e.g., "202502041938081"

	// Parse the combined dateTime string into a time.Time object
	t, err := time.ParseInLocation("20060102150405", dateTimeStr, time.Local)
	if err != nil {
		return TrackedObjectInfo{}, fmt.Errorf("failed to parse datetime %s: %w", dateTimeStr, err)
	}

	firstSeenUnix := t.Unix()
	lastSeenUnix := time.Now().Unix()
	age := lastSeenUnix - firstSeenUnix
	firstSeenDateTime := t.Format("2006-01-02 15:04:05 -0700 MST")
	lastSeenDateTime := time.Now().Format("2006-01-02 15:04:05 -0700 MST")

	// If the detection is "full", initialize the LastFull fields to the same timestamp.
	lastFullUnix := int64(0)
	lastFullDateTime := ""
	if isFull {
		lastFullUnix = lastSeenUnix
		lastFullDateTime = lastSeenDateTime
	}

	// Return the tracked object with all parsed values
	return TrackedObjectInfo{
		FirstSeenDateTime: firstSeenDateTime,
		FirstSeenUnix:     firstSeenUnix,
		LastSeenDateTime:  lastSeenDateTime,
		LastSeenUnix:      lastSeenUnix,
		Age:               age,
		ID:                detectionID,
		Label:             detectionClass,
		IsFull:            isFull,
		LastFullUnix:      lastFullUnix,
		LastFull:          lastFullDateTime,
	}, nil
}

// createCSVFromJSON reads the given JSON file (in the shape map[string][]TrackedObjectInfo),
// converts all slices to CSV rows, and writes them to csvPath.
func createCSVFromJSON(jsonPath, csvPath string) error {
	// 1. Read the JSON file
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("failed to read JSON file %s: %w", jsonPath, err)
	}

	// 2. Parse into map[label] => []TrackedObjectInfo
	var allDetections map[string][]TrackedObjectInfo
	if err := json.Unmarshal(data, &allDetections); err != nil {
		return fmt.Errorf("failed to unmarshal JSON for %s: %w", jsonPath, err)
	}

	// 3. Flatten into a single list for CSV
	var rows []TrackedObjectInfo
	for label, detSlice := range allDetections {
		for _, det := range detSlice {
			// Make sure the struct’s Label is consistent with the map key
			// (sometimes they might mismatch, but presumably you’re storing them consistently).
			det.Label = label
			rows = append(rows, det)
		}
	}

	// 4. Create or overwrite the CSV file
	f, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("failed to create CSV file %s: %w", csvPath, err)
	}
	defer f.Close()

	// 5. Write header and rows
	writer := csv.NewWriter(f)
	defer writer.Flush()

	// CSV header
	headers := []string{
		"FirstSeenDateTime",
		"FirstSeenUnix",
		"LastSeenDateTime",
		"LastSeenUnix",
		"Age",
		"ID",
		"Label",
		"IsFull",
		"LastFull",
		"LastFullUnix",
	}
	if err := writer.Write(headers); err != nil {
		return fmt.Errorf("failed to write CSV header: %w", err)
	}

	// Write each row
	for _, det := range rows {
		record := []string{
			det.FirstSeenDateTime,
			fmt.Sprintf("%d", det.FirstSeenUnix),
			det.LastSeenDateTime,
			fmt.Sprintf("%d", det.LastSeenUnix),
			fmt.Sprintf("%d", det.Age),
			det.ID,
			det.Label,
			fmt.Sprintf("%t", det.IsFull),
			det.LastFull,
			fmt.Sprintf("%d", det.LastFullUnix),
		}
		if err := writer.Write(record); err != nil {
			return fmt.Errorf("failed to write CSV row: %w", err)
		}
	}

	return nil
}

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
	// e.g. "2025-02-03-tracked-objects.json"
	newPath := filepath.Join(s.dataSyncDir, baseName)

	// Move (rename) the JSON file to dataSyncDir
	if err := os.Rename(s.currentPath, newPath); err != nil {
		return fmt.Errorf("failed to rename file from %s to %s: %w", s.currentPath, newPath, err)
	}
	s.logger.Infof("Rotated file from %s to %s", s.currentPath, newPath)

	// Build the CSV path: same directory, same base, but .csv
	// E.g.  baseName = "2025-02-03-tracked-objects.json"
	//       csvBaseName = "2025-02-03-tracked-objects.csv"
	csvBaseName := strings.TrimSuffix(baseName, filepath.Ext(baseName)) + ".csv"
	csvPath := filepath.Join(s.dataSyncDir, csvBaseName)

	// Create CSV from the just-rotated JSON file
	if err := createCSVFromJSON(newPath, csvPath); err != nil {
		return fmt.Errorf("failed to create CSV from JSON %s: %w", newPath, err)
	}
	s.logger.Infof("Created CSV file at %s", csvPath)

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
	s.fileLock.Lock()
	defer s.fileLock.Unlock()

	if extra[data.FromDMString] == true {
		nowDay := time.Now().Format("2006-01-02")
		if nowDay != s.currentDate {
			s.logger.Info("Data manager called Readings on a new day; rotating the file so it can capture yesterday's data.")

			oldFileData, err := s.readCurrentFile()
			if err != nil {
				if os.IsNotExist(err) {
					s.logger.Warnf("File %s does not exist; returning empty data", s.currentPath)
					return map[string]interface{}{}, nil
				}
				return nil, fmt.Errorf("failed to read old file data: %w", err)
			}

			if err := s.rotateFile(); err != nil {
				return nil, fmt.Errorf("failed to rotate file: %w", err)
			}

			s.currentDate = nowDay
			s.currentPath = s.dailyFilePath(s.currentDate, s.tempDataDir)
			return oldFileData, nil
		}
		// return nil and an error if it's not a new day.
		return nil, data.ErrNoCaptureToStore
	}

	// For non-data manager calls, you may decide whether to return readings or not.
	return s.readCurrentFile()
}

// readCurrentFile is a small helper that opens the current JSON file and unmarshals it
func (s *displayTrackingReportGenerator) readCurrentFile() (map[string]interface{}, error) {
	f, err := os.Open(s.currentPath)
	if err != nil {
		// If file doesn’t exist, return an error so caller can handle it
		return nil, err
	}
	defer f.Close()

	var fileData map[string]interface{}
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&fileData); err != nil {
		return nil, fmt.Errorf("failed to decode JSON file: %w", err)
	}

	return fileData, nil
}

func (s *displayTrackingReportGenerator) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	// Example expected input: {"action": "rotate"}
	action, ok := cmd["action"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid 'action' in DoCommand input")
	}

	switch action {
	case "rotate":
		// Lock around file I/O
		s.fileLock.Lock()
		defer s.fileLock.Unlock()

		if err := s.rotateFile(); err != nil {
			return nil, fmt.Errorf("failed to rotate file: %w", err)
		}
		return map[string]interface{}{
			"status": "file rotated successfully",
		}, nil

	default:
		return nil, fmt.Errorf("unknown action '%s'", action)
	}
}

// Close stops the background loop
func (s *displayTrackingReportGenerator) Close(ctx context.Context) error {
	s.cancelFunc()
	return nil
}
