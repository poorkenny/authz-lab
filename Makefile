.PHONY: ui build run dev-api dev-ui test docker docker-run

ui:
	cd web && npm ci --no-audit --no-fund && npm run build

build: ui
	go build -o bin/authz-lab ./cmd/server

run: build
	./bin/authz-lab

# Backend on :8080; run `make dev-ui` in another terminal for hot-reloading UI on :5173.
dev-api:
	go run ./cmd/server

dev-ui:
	cd web && npm run dev

test:
	go vet ./...
	go test ./...
	cd web && npm run typecheck

docker:
	docker build -t authz-lab .

docker-run: docker
	docker run --rm -p 10000:10000 authz-lab
