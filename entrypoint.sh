#! /bin/bash

echo "Running in $RUN_AS mode"
if [ "$RUN_AS" = "distributor" ]; then
  echo "Starting distributor..."
  ./bin/distributor
elif [ "$RUN_AS" = "worker" ]; then
  echo "Starting worker..."
  ./bin/worker
else
    echo "Invalid RUN_AS value: $RUN_AS. Please set it to 'distributor' or 'worker'."
    exit 1
fi