.DEFAULT_GOAL := run

BENCH_DIR := benchmarks

.PHONY: run test fmt race bench lint dc-up

run:
	go run .

test:
	go test ./...

fmt:
	go fmt ./...

race:
	CGO_ENABLED=1 go test -race ./...

bench:
	@mkdir -p $(BENCH_DIR)
	@out="$(BENCH_DIR)/bench-$$(date -u +%Y%m%dT%H%M%SZ).txt"; \
	go test -run '^$$' -bench . -benchmem ./... | tee "$$out"; \
	printf '\nSaved benchmark output to %s\n' "$$out"

lint:
	golangci-lint run

dc-up:
	docker compose up -d

dc-ps:
	docker compose ps

dc-logs:
	docker compose logs -f