# Makefile for SAM build of EmailWorkerFunction
# SAM invokes: make build-EmailWorkerFunction ARTIFACTS_DIR=<path>
# The binary must be named "bootstrap" for the provided.al2023 runtime.

.PHONY: build-EmailWorkerFunction

build-EmailWorkerFunction:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-tags lambda.norpc \
		-ldflags="-s -w" \
		-o $(ARTIFACTS_DIR)/bootstrap \
		./
