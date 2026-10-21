.PHONY: build frontend test bench check smoke
frontend:
	cd web && npm ci && npm run build
build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/ts-egress ./cmd/ts-egress
test:
	go test -race ./...
	cd web && npm test
	cd web && npm run format:check
bench:
	go test -run '^$$' -bench . -benchmem ./internal/gateway
check:
	go vet ./...
	go test -race ./...
	cd web && npm ci && npm run format:check && npm test && npm run build
smoke:
	@set -eu; project="ts-egress-smoke-$$$$"; \
	trap 'docker compose -p "$$project" -f compose.yaml down --volumes --remove-orphans' EXIT; \
	docker compose -p "$$project" -f compose.yaml config --quiet; \
	docker compose -p "$$project" -f compose.yaml build; \
	docker compose -p "$$project" -f compose.yaml run -d --name "$$project"-app \
		-p 127.0.0.1:18080:8080 -p 18080:80 -p 18443:443 ts-egress; \
	ready=0; for i in $$(seq 1 30); do if curl --fail --silent http://127.0.0.1:18080/api/setup/session >/dev/null; then ready=1; break; fi; sleep 1; done; \
	test "$$ready" = 1; curl --fail --silent http://127.0.0.1:18080/api/setup/session | grep -q '"claimed":false'
