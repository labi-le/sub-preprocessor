.DEFAULT_GOAL := run

.PHONY: run test fmt

run:
	go run .

test:
	go test ./...

fmt:
	go fmt ./...
