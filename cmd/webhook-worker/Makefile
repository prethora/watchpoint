# Makefile for SAM build of WebhookWorkerFunction
# SAM invokes: make build-WebhookWorkerFunction ARTIFACTS_DIR=<path>
# The binary must be named "bootstrap" for the provided.al2023 runtime.

.PHONY: build-WebhookWorkerFunction

build-WebhookWorkerFunction:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-tags lambda.norpc \
		-ldflags="-s -w" \
		-o $(ARTIFACTS_DIR)/bootstrap \
		./
