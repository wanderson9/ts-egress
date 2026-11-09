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
	name="$$project-app"; override=$$(mktemp); cookie=$$(mktemp); printf 'services:\n  ts-egress:\n    image: ts-egress:%s\n' "$$project" > "$$override"; export COMPOSE_PROJECT_NAME="$$project"; trap 'docker rm -f "$$name" >/dev/null 2>&1 || true; docker compose -p "$$project" -f compose.yaml -f "$$override" down --volumes --remove-orphans; rm -f "$$override" "$$cookie"' EXIT; \
	docker compose -p "$$project" -f compose.yaml config --quiet; \
	docker compose -p "$$project" -f compose.yaml -f "$$override" build; \
	docker compose -p "$$project" -f compose.yaml -f "$$override" run -d --name "$$name" \
		-p 127.0.0.1::8080 -p 127.0.0.1::80 -p 127.0.0.1::443 ts-egress; \
	port=$$(docker port "$$name" 8080/tcp | sed 's/.*://'); origin="http://127.0.0.1:$$port"; \
	ready=0; for i in $$(seq 1 30); do if curl --fail --silent "$$origin/api/setup/session" >/dev/null; then ready=1; break; fi; sleep 1; done; \
	test "$$ready" = 1; \
	curl --fail --silent "$$origin/api/setup/session" | grep -q '"claimed":false'; \
	code=$$(curl --silent --output /dev/null --write-out '%{http_code}' "$$origin/api/setup/status"); test "$$code" = 401; \
	code=$$(curl --silent --output /dev/null --write-out '%{http_code}' --cookie-jar "$$cookie" --header 'Content-Type: application/json' --header "Origin: $$origin" --header 'X-TS-Egress: 1' --data '{"password":"smoke-test-password"}' "$$origin/api/setup/claim"); test "$$code" = 204; \
	curl --fail --silent --cookie "$$cookie" "$$origin/api/setup/session" | grep -q '"authenticated":true'; \
	code=$$(curl --silent --output /dev/null --write-out '%{http_code}' --cookie "$$cookie" "$$origin/api/setup/status"); test "$$code" = 200; \
	public_port=$$(docker port "$$name" 80/tcp | sed 's/.*://'); code=$$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:$$public_port/api/routes"); test "$$code" = 404; \
	sleep 1; code=$$(curl --silent --output /dev/null --write-out '%{http_code}' --header 'Content-Type: application/json' --header "Origin: $$origin" --header 'X-TS-Egress: 1' --data '{"password":"wrong-password"}' "$$origin/api/setup/login"); test "$$code" = 403; \
	docker restart "$$name" >/dev/null; \
	port=$$(docker port "$$name" 8080/tcp | sed 's/.*://'); origin="http://127.0.0.1:$$port"; \
	ready=0; for i in $$(seq 1 30); do if curl --fail --silent "$$origin/api/setup/session" | grep -q '"claimed":true'; then ready=1; break; fi; sleep 1; done; test "$$ready" = 1; \
	code=$$(curl --silent --output /dev/null --write-out '%{http_code}' --cookie "$$cookie" "$$origin/api/setup/status"); test "$$code" = 401; \
	code=$$(curl --silent --output /dev/null --write-out '%{http_code}' --header 'Content-Type: application/json' --header "Origin: $$origin" --header 'X-TS-Egress: 1' --data '{"password":"smoke-test-password"}' "$$origin/api/setup/login"); test "$$code" = 204
