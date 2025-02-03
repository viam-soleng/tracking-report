import asyncio
import os
import json
import time
import threading
import datetime
from typing import Any, ClassVar, Final, Mapping, Optional, Sequence
from typing_extensions import Self

from viam.components.sensor import Sensor
from viam.module.module import Module
from viam.proto.app.robot import ComponentConfig
from viam.proto.common import ResourceName
from viam.resource.base import ResourceBase
from viam.resource.easy_resource import EasyResource
from viam.resource.types import Model, ModelFamily
from viam.utils import SensorReading, struct_to_dict
from viam.logging import getLogger
from viam.services.vision.vision import Vision
from viam.components.camera.camera import Camera

logger = getLogger(__name__)


class ReportGenerator(Sensor, EasyResource):
    MODEL: ClassVar[Model] = Model(
        ModelFamily("viam-soleng", "display-tracking"), "report-generator"
    )

    def __init__(self, name: str):
        """Typically __init__ is called by super().new(), so you can set defaults here."""
        super().__init__(name)
        self.running = False
        self.data_collector_thread = None
        self.lock = threading.Lock()

        # We'll store pizza data in a dict:
        # pizza_id -> { "first_seen": <timestamp>, "last_seen": <timestamp> }
        self.pizza_data = {}

        # Keep track of the current daily file
        self.current_date = time.strftime("%Y-%m-%d")
        self.current_filepath = self._get_daily_filepath(self.current_date)

        # Interval (seconds) for how often we collect data
        self.collection_interval = 60

        # References to kioskCamera and pizzaTracking
        self.kiosk_camera_name = None
        self.pizza_tracking_name = None

    @classmethod
    def new(
        cls,
        config: ComponentConfig,
        dependencies: Mapping[ResourceName, ResourceBase]
    ) -> Self:
        """Create a new instance of the ReportGenerator Sensor."""
        # Create the sensor instance
        sensor = super().new(config, dependencies)

        # Extract config attributes
        attributes = struct_to_dict(config.attributes)

        sensor.kiosk_camera_name = attributes.get("camera_name")
        if not sensor.kiosk_camera_name:
            raise Exception("No camera_name found in config")

        sensor.pizza_tracking_name = attributes.get("tracker_name")
        if not sensor.pizza_tracking_name:
            raise Exception("No tracker_name found in config")

        # Optionally, if these are actual Viam resources, you can fetch them here:
        sensor.kiosk_camera = dependencies.get(ResourceName(sensor.kiosk_camera_name))
        sensor.pizza_tracking_service = dependencies.get(ResourceName(sensor.pizza_tracking_name))

        # Start the background thread
        sensor.running = True
        sensor.data_collector_thread = threading.Thread(
            target=sensor._data_collection_loop, daemon=True
        )
        sensor.data_collector_thread.start()

        return sensor

    @classmethod
    def validate_config(cls, config: ComponentConfig) -> Sequence[str]:
        """Validate the config object and return any implicit dependencies based on it."""
        deps = []
        attributes = struct_to_dict(config.attributes)

        kiosk_camera = attributes.get("camera_name")
        if not kiosk_camera:
            raise Exception("No camera_name found in config")
        deps.append(str(kiosk_camera))

        pizza_tracking = attributes.get("tracker_name")
        if not pizza_tracking:
            raise Exception("No tracker_name found in config")
        deps.append(str(pizza_tracking))

        return deps

    def reconfigure(
        self,
        config: ComponentConfig,
        dependencies: Mapping[ResourceName, ResourceBase]
    ):
        """Dynamically update the service when it receives a new config."""
        # Stop the old thread
        self.running = False
        if self.data_collector_thread:
            self.data_collector_thread.join()

        # Update from the new config
        attributes = struct_to_dict(config.attributes)
        self.kiosk_camera_name = attributes.get("kioskFeed")
        self.pizza_tracking_name = attributes.get("myPizzaTracker")

        # If you have new intervals, file paths, etc., update them here
        # self.collection_interval = attributes.get("interval", 60)

        # Start a new thread if still needed
        self.running = True
        self.data_collector_thread = threading.Thread(
            target=self._data_collection_loop, daemon=True
        )
        self.data_collector_thread.start()

        # Call the parent reconfigure if you need to
        super().reconfigure(config, dependencies)

    async def get_readings(
        self,
        *,
        extra: Optional[Mapping[str, Any]] = None,
        timeout: Optional[float] = None,
        **kwargs
    ) -> Mapping[str, SensorReading]:
        """When asked for readings, return the current pizza data."""
        with self.lock:
            # Convert self.pizza_data to SensorReading objects
            readings = {}
            for pizza_id, timestamps in self.pizza_data.items():
                readings[pizza_id] = SensorReading(
                    value={
                        "first_seen": timestamps.get("first_seen"),
                        "last_seen": timestamps.get("last_seen"),
                    },
                    meta={}
                )
            return readings

    def _data_collection_loop(self):
        """Background thread that periodically collects data and writes to JSON."""
        while self.running:
            # 1. Collect data from the kiosk camera and pizza tracker
            #    (Replace these calls with your actual logic)
            detection_results = self._mock_data_fetch()

            # 2. Update the in-memory data
            with self.lock:
                self._update_pizza_data(detection_results)

                # Check date to see if we need to rotate
                current_date = time.strftime("%Y-%m-%d")
                if current_date != self.current_date:
                    self._rotate_file()
                    self.current_date = current_date
                    self.current_filepath = self._get_daily_filepath(current_date)

                # 3. Write to JSON
                self._write_to_json()

            # Sleep for the interval
            time.sleep(self.collection_interval)

        logger.info("Data collection thread stopped.")

    def _mock_data_fetch(self):
        """
        Pretend we're calling the kiosk camera + pizza tracker.
        Return a list of dicts like: [{"id": "pizza123"}, ...]
        """
        # For demo, just return a single item.
        # In reality, you'd do something like:
        #   frames = self.kiosk_camera.get_frame()
        #   results = self.pizza_tracking_service.detect_pizza(frames)
        #   return results
        return [{"id": "pizza123"}]

    def _update_pizza_data(self, detection_results):
        """Update self.pizza_data based on new detections."""
        now = time.time()
        for result in detection_results:
            pid = result["id"]
            if pid not in self.pizza_data:
                self.pizza_data[pid] = {
                    "first_seen": now,
                    "went_partial": now,
                    "last_seen": now,
                }
            else:
                self.pizza_data[pid]["last_seen"] = now

    def _write_to_json(self):
        """Write self.pizza_data to the current daily JSON file."""
        try:
            with open(self.current_filepath, "w") as f:
                json.dump(self.pizza_data, f, indent=2)
        except Exception as e:
            logger.error(f"Error writing JSON file: {e}")

    def _rotate_file(self):
        """Move the old file to ~/.viam/capture and clear our in-memory data."""
        capture_dir = os.path.expanduser("~/.viam/capture")
        if not os.path.exists(capture_dir):
            logger.error(f"No capture directory: {capture_dir}")
            # Optionally clear in-memory data for a fresh start each day
            self.pizza_data = {}
            return
        
        if os.path.exists(self.current_filepath):
            new_path = os.path.join(capture_dir, os.path.basename(self.current_filepath))
            os.rename(self.current_filepath, new_path)
            logger.info(f"Moved {self.current_filepath} to {new_path}")

        # Optionally clear in-memory data for a fresh start each day
        self.pizza_data = {}

    def _get_daily_filepath(self, date_str: str) -> str:
        """Return a file path based on the current date, like /data/YYYY-MM-DD.json"""
        base_dir = "/data/daily_json"
        if not os.path.exists(base_dir):
            os.makedirs(base_dir)
        return os.path.join(base_dir, f"{date_str}-pizzas.json")


if __name__ == "__main__":
    asyncio.run(Module.run_from_registry())
