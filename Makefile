.DEFAULT_GOAL:=help
SHELL:=/bin/sh

.PHONY: help
help: # Show help for each of the Makefile recipes.
	@grep -E '^[a-zA-Z0-9 -]+:.*#' Makefile | sort | while read -r l; do printf "\033[1;32m---------------------------------------------------------\n$$(echo $$l | cut -f 1 -d':')\033[00m:$$(echo $$l | cut -f 2- -d'#')\n"; done; printf "\033[1;32m---------------------------------------------------------\n";

########################################################################################################################
##@ Common
########################################################################################################################

.PHONY: all
all: # Runs build and run
	build run

.PHONY: build
build: # Build the go application
	go build main.go


.PHONY: run
run: # Runs the application locally
	go run -race -ldflags=-extldflags=-Wl,-ld_classic ./

########################################################################################################################
##@ Setup
########################################################################################################################

.PHONY: clean
clean: # Add missing and remove unused go modules
	go mod tidy

.PHONY: update
update: # Install and update go modules
	go get -t -u ./...

.PHONY: list
list: # List modules that have updates
	go install github.com/icholy/gomajor@latest | gomajor list

########################################################################################################################
##@ Tests
########################################################################################################################

.PHONY: mock
mock: # Install mock module and updates all mocks files
	go install github.com/vektra/mockery/v2@latest
	rm -rf internal/mocks
	mockery

.PHONY: test
test: # Runs all the tests in the application and returns if they passed or failed, along with a coverage percentage
	go install github.com/mfridman/tparse@latest | go mod tidy
	GOEXPERIMENT=nocoverageredesign PROFILE=local go test -json -cover ./... | tparse -all -pass

########################################################################################################################
##@ Code Style
########################################################################################################################

.PHONY: lint
lint: #Shows possible errors in the code
	golangci-lint run

.PHONY: format
format: # Format the code and imports
	go fmt ./...
	goimports -w .
	fieldalignment -fix ./...

.PHONY: check
check:
	go vet ./...
	staticcheck ./...
