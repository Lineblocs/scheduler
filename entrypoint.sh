#! /bin/bash

echo "Running in $RUN_AS mode"

if [ "$RUN_AS" = "distributor" ]; then
  echo "Starting distributor..."
  ./bin/distributor
elif [ "$RUN_AS" = "worker-recordings" ]; then
  echo "Starting worker-recordings..."
  ./bin/worker-recordings
elif [ "$RUN_AS" = "worker-billing" ]; then
  echo "Starting worker-billing..."
  ./bin/worker-billing
elif [ "$RUN_AS" = "worker-workspace-tasks" ]; then
  echo "Starting worker-workspace-tasks..."
  ./bin/worker-workspace-tasks
elif [ "$RUN_AS" = "workspace-alerting" ]; then
  echo "Starting workspace-alerting..."
  ./bin/workspace-alerting
else
    echo "Invalid RUN_AS value: $RUN_AS. Please set it to 'distributor', 'worker-recordings', or 'worker-billing'."
    exit 1
fi