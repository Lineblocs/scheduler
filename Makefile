.DEFAULT_GOAL:=help
SHELL:=/bin/sh

# Variables
BINARY_DIR=bin
DISTRIBUTOR_BINARY=$(BINARY_DIR)/distributor
WORKER_BINARY=$(BINARY_DIR)/worker

.PHONY: help
help: # Show help for each of the Makefile recipes.
	@grep -E '^[a-zA-Z0-9 -]+:.*#' Makefile | sort | while read -r l; do printf "\033[1;32m---------------------------------------------------------\n$$(echo $$l | cut -f 1 -d':')\033[00m:$$(echo $$l | cut -f 2- -d'#')\n"; done; printf "\033[1;32m---------------------------------------------------------\n";

########################################################################################################################
##@ Common
########################################################################################################################

.PHONY: all
all: build # Build both distributor and worker binaries

.PHONY: build
build: # Build both distributor and worker binaries
	@echo "Building binaries..."
	mkdir -p $(BINARY_DIR)
	go build -o $(DISTRIBUTOR_BINARY) ./cmd/distributor/main.go
	go build -o $(WORKER_BINARY) ./cmd/worker/main.go
	@echo "Binaries available in ./bin"

.PHONY: run-distributor
run-distributor: # Runs the distributor locally using go run
	go run -race ./cmd/distributor/main.go

.PHONY: run-worker
run-worker: # Runs the worker locally using go run
	go run -race ./cmd/worker/main.go

########################################################################################################################
##@ Setup
########################################################################################################################

.PHONY: clean
clean: # Remove build binaries and tidy go modules
	rm -rf $(BINARY_DIR)
	go mod tidy

.PHONY: update
update: # Install and update go modules
	go get -t -u ./...

.PHONY: list
list: # List modules that have updates
	go install github.com/icholy/gomajor@latest
	gomajor list

########################################################################################################################
##@ Tests
########################################################################################################################

.PHONY: mock
mock: # Regenerate all mocks using mockery
	go install github.com/vektra/mockery/v2@latest
	rm -rf mocks/
	mockery --all --recursive --dir ./repository --output ./mocks
	mockery --all --recursive --dir ./handlers/billing --output ./mocks

.PHONY: test
test: # Run tests with tparse formatting and coverage
	go install github.com/mfridman/tparse@latest
	go mod tidy
	GOEXPERIMENT=nocoverageredesign PROFILE=local go test -json -cover ./... | tparse -all -pass

########################################################################################################################
##@ Code Style
########################################################################################################################

.PHONY: lint
lint: # Run golangci-lint for code quality checks
	golangci-lint run

.PHONY: format
format: # Format code, imports, and fix struct field alignment
	go fmt ./...
	go install golang.org/x/tools/cmd/goimports@latest
	go install golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@latest
	goimports -w .
	fieldalignment -fix ./...

.PHONY: check
check: # Run static analysis (vet and staticcheck)
	go vet ./...
	go install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./...