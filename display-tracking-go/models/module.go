package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
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
	TrackerName string `json:"tracker_name"`
	CameraName  string `json:"camera_name"`
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

// We'll store minimal info about each detection or "pizza" in this example.
type PizzaInfo struct {
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	ID        string `json:"id,omitempty`
}

// displayTrackingReportGenerator is our sensor implementation.
type displayTrackingReportGenerator struct {
	name resource.Name

	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()

	// We'll store an in-memory map of detections
	pizzaData map[string]PizzaInfo

	// For daily file management:
	currentDate string // e.g. "2025-02-03"
	currentPath string // e.g. "/data/daily_json/2025-02-03-pizzas.json"
	dataSyncDir string // e.g. "/data/sync"

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

	// Initialize the sensor struct
	s := &displayTrackingReportGenerator{
		name:       rawConf.ResourceName(),
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		pizzaData:  make(map[string]PizzaInfo),

		// For daily rotation:
		currentDate: time.Now().Format("2006-01-02"),
		dataSyncDir: "~/.viam/data/sync",
	}
	// Generate today's file path
	s.currentPath = s.dailyFilePath(s.currentDate)

	// Start our background goroutine
	go s.dataCollectionLoop()

	return s, nil
}

// dataCollectionLoop runs periodically to collect detections & write them to a daily file.
func (s *displayTrackingReportGenerator) dataCollectionLoop() {
	ticker := time.NewTicker(60 * time.Second) // collect every 60 seconds
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
				s.currentPath = s.dailyFilePath(nowDay)
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

				// Also update our in-memory map (optional)
				s.updatePizzaData(detections)
			}

		}
	}
}

// getDetections simulates calling your vision service with camera name
func (s *displayTrackingReportGenerator) getDetections() ([]PizzaInfo, error) {
	// In real code, you'd do something like:
	// detections, err := yourVisionService.GetDetectionsFromCamera(s.cfg.CameraName)
	// if err != nil { return nil, err }

	// For demo, let's pretend we found one detection
	now := time.Now().Unix()
	demo := []PizzaInfo{
		{
			ID:        "pizza_123",
			FirstSeen: now,
			LastSeen:  now,
		},
	}
	return demo, nil
}

// writeDetectionsToFile opens the current daily file and appends JSON lines
func (s *displayTrackingReportGenerator) writeDetectionsToFile(detections []PizzaInfo) error {
	// Create or append
	f, err := os.OpenFile(s.currentPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, d := range detections {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	s.logger.Infof("Appended %d detections to %s", len(detections), s.currentPath)
	return nil
}

// updatePizzaData merges new detections into our in-memory map
func (s *displayTrackingReportGenerator) updatePizzaData(detections []PizzaInfo) {
	now := time.Now().Unix()
	for _, d := range detections {
		existing, ok := s.pizzaData[d.ID]
		if !ok {
			s.pizzaData[d.ID] = PizzaInfo{
				FirstSeen: d.FirstSeen,
				LastSeen:  d.LastSeen,
				ID:        d.ID,
			}
		} else {
			existing.LastSeen = now
			s.pizzaData[d.ID] = existing
		}
	}
}

// rotateFile moves the current daily file to s.dataSyncDir
func (s *displayTrackingReportGenerator) rotateFile() error {
	if _, err := os.Stat(s.currentPath); os.IsNotExist(err) {
		// If file doesn't exist, nothing to rotate
		return nil
	}
	baseName := filepath.Base(s.currentPath)
	newPath := filepath.Join(s.dataSyncDir, baseName)

	if err := os.Rename(s.currentPath, newPath); err != nil {
		return err
	}
	s.logger.Infof("Rotated file from %s to %s", s.currentPath, newPath)
	return nil
}

// dailyFilePath returns something like /data/daily_json/2025-02-03-pizzas.json
func (s *displayTrackingReportGenerator) dailyFilePath(dateStr string) string {
	dir := "/data/daily_json"
	_ = os.MkdirAll(dir, 0755) // ensure directory exists
	return filepath.Join(dir, fmt.Sprintf("%s-pizzas.json", dateStr))
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

	// optionally reset date, path if you want to treat reconfigure as a fresh start
	// s.currentDate = time.Now().Format("2006-01-02")
	// s.currentPath = s.dailyFilePath(s.currentDate)

	go s.dataCollectionLoop()
	return nil
}

// NewClientFromConn is not implemented in this example
func (s *displayTrackingReportGenerator) NewClientFromConn(ctx context.Context, conn rpc.ClientConn, remoteName string, name resource.Name, logger logging.Logger) (sensor.Sensor, error) {
	panic("not implemented")
}

// Readings reads today's file, parses each line as a PizzaInfo, and returns a map keyed by ID
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

	// We'll accumulate everything in a map from ID -> detection info
	out := make(map[string]interface{})

	dec := json.NewDecoder(f)
	for {
		var pi PizzaInfo
		if err := dec.Decode(&pi); err != nil {
			if errors.Is(err, io.EOF) {
				// End of file
				break
			}
			// If there's some decoding error, log or return it
			return nil, err
		}

		// We assume pi.ID is a unique identifier
		out[pi.ID] = map[string]interface{}{
			"first_seen": pi.FirstSeen,
			"last_seen":  pi.LastSeen,
		}
	}

	return out, nil
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
