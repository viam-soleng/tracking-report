# Display Tracking Report Generator

## Overview

The `display-tracking` module is a Viam sensor component that tracks objects detected by a vision service and logs the data into JSON files for daily reports. The module supports periodic data collection, JSON file rotation, and conversion to CSV for easy data analysis.

## Features

- Tracks objects detected by a vision service
- Stores detections in JSON format, organized by date
- Periodic data collection at a configurable interval
- Automatically rotates files daily and converts JSON to CSV
- Thread-safe file operations using a mutex
- Configurable temporary data storage and sync directory

## Installation

This module is implemented as a Viam sensor component and should be integrated into a Viam-powered system. Ensure that the required dependencies are available:

### Dependencies

- Viam RDK
- Go 1.18+
- `go.viam.com/rdk` package
- `go.viam.com/utils/rpc`
- Standard Go libraries (`os`, `sync`, `time`, `encoding/json`, `encoding/csv`)

## Configuration

The module requires the following configuration fields:

```json
{
  "tracker_name": "vision_tracker",
  "camera_name": "front_camera",
  "temporary_data_dir": "/data/json_daily_report",
  "data_sync_dir": "/root/.viam/capture",
  "interval": 60
}
```

### Configuration Fields

| Field                | Type   | Description                                                                       |
| -------------------- | ------ | --------------------------------------------------------------------------------- |
| `tracker_name`       | string | Name of the vision tracker service                                                |
| `camera_name`        | string | Name of the camera providing detections                                           |
| `temporary_data_dir` | string | Directory for storing daily JSON reports (default: `~/data/json_daily_report`)    |
| `data_sync_dir`      | string | Directory for storing rotated JSON and CSV files (default: `/root/.viam/capture`) |
| `interval`           | int    | Frequency (in seconds) at which detections are collected (default: 60s)           |

## Usage

### Running the Module

This module runs as a Viam sensor and automatically starts data collection. It periodically retrieves detections from the vision service and writes them to a JSON file in the configured `temporary_data_dir`.

### File Rotation

At the start of each day, the previous day's JSON file is rotated to `data_sync_dir` and converted into a CSV file. The module ensures that all data remains organized for further analysis.

### JSON File Structure

Each detection is logged in a structured JSON format:

```json
{
  "Label": [
    {
      "id": "object_1",
      "first_seen_date_time": "2025-02-03 14:30:00 -0700 MST",
      "first_seen_unix": 1706880600,
      "last_seen_date_time": "2025-02-03 14:35:00 -0700 MST",
      "last_seen_unix": 1706880900,
      "age": 300,
      "is_full": true,
      "last_full": "2025-02-03 14:35:00 -0700 MST",
      "last_full_unix": 1706880900
    }
  ]
}
```

### CSV File Structure

After rotation, JSON files are converted into CSV format:

| FirstSeenDateTime             | FirstSeenUnix | LastSeenDateTime              | LastSeenUnix | Age | ID        | Label    | IsFull | LastFull                      | LastFullUnix |
| ----------------------------- | ------------- | ----------------------------- | ------------ | --- | --------- | -------- | ------ | ----------------------------- | ------------ |
| 2025-02-03 14:30:00 -0700 MST | 1706880600    | 2025-02-03 14:35:00 -0700 MST | 1706880900   | 300 | object\_1 | Label\_1 | true   | 2025-02-03 14:35:00 -0700 MST | 1706880900   |

## API

### `DoCommand`

The module exposes a `DoCommand` method for manual file rotation:

#### Request:

```json
{
  "action": "rotate"
}
```

#### Response:

```json
{
  "status": "file rotated successfully"
}
```

### `Readings`

The `Readings` method fetches the latest detections stored in the JSON file. If called by a Data Manager on a new day, it will rotate the file first before returning the previous day's data.

## Development

### Building the Module

Run the following command to build:

```sh
go build -o display-tracking
```

### Testing the Module

To test the module, use:

```sh
go test ./...
```

## License

This project is licensed under the MIT License.

## Contact

For any issues or contributions, please contact the Viam Solutions Engineering Team.

