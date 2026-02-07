# Makefile for SAM build of DataPollerFunction
# SAM invokes: make build-DataPollerFunction ARTIFACTS_DIR=<path>
# The binary must be named "bootstrap" for the provided.al2023 runtime.

.PHONY: build-DataPollerFunction

build-DataPollerFunction:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-tags lambda.norpc \
		-ldflags="-s -w" \
		-o $(ARTIFACTS_DIR)/bootstrap \
		./
