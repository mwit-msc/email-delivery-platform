.PHONY: dev test integration build down

dev:
	./scripts/dev-init.sh
	docker compose up --build -d

test:
	go test ./...

integration:
	EDP_TEST_DATABASE_URL=postgres://edp:edp-local-only@localhost:15432/edp?sslmode=disable go test ./internal/edp -count=1 -race

build:
	go build -o edp ./cmd/edp

down:
	docker compose down
