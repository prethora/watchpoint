# Makefile for SAM build of ApiFunction
# SAM invokes: make build-ApiFunction ARTIFACTS_DIR=<path>
# The binary must be named "bootstrap" for the provided.al2023 runtime.

.PHONY: build-ApiFunction

build-ApiFunction:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-tags lambda.norpc \
		-ldflags="-s -w" \
		-o $(ARTIFACTS_DIR)/bootstrap \
		./
