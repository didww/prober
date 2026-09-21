# prober — native traceroute probers (prober-agent) reporting to a central
# service (prober-backend) that serves a web UI.
#
# Placeholder name "prober"; see docs/ARCHITECTURE.md. Two binaries, both
# static so they run in a Debian 13 distroless image.

VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all
all: build

## web: build the Vue SPA into web/dist, where go:embed picks it up
.PHONY: web
web:
	cd web && npm ci && npm run build
	touch web/dist/.gitkeep

.PHONY: build
build: web
	go build -ldflags '$(LDFLAGS)' -o prober-agent ./cmd/prober-agent
	go build -ldflags '$(LDFLAGS)' -o prober-backend ./cmd/prober-backend

## build-go: rebuild only the binaries, reusing the existing web/dist
.PHONY: build-go
build-go:
	go build -ldflags '$(LDFLAGS)' -o prober-agent ./cmd/prober-agent
	go build -ldflags '$(LDFLAGS)' -o prober-backend ./cmd/prober-backend

## dev-web: Vite dev server with HMR, proxying /api to the backend on :8080
.PHONY: dev-web
dev-web:
	cd web && npm run dev

## proto: regenerate the gRPC stubs from api/proto (needs buf via `go run`)
.PHONY: proto
proto:
	cd api && go run github.com/bufbuild/buf/cmd/buf generate

## test: everything that runs without special privileges
.PHONY: test
test:
	go test ./...

## test-trace: the raw-socket integration tests (trace engine + the backend
## monitoring end-to-end test), which need raw sockets.
## Runs them as root inside an unprivileged user+network namespace, so no
## sudo and no capability on the binary is required. This is what CI runs.
.PHONY: test-trace
test-trace:
	unshare -Urn sh -c 'ip link set lo up && go test -count=1 ./internal/trace/... ./internal/backend/...'

.PHONY: check
check:
	@unformatted=$$(gofmt -l cmd internal); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	$(MAKE) test
	$(MAKE) test-trace

## chart-check: lint both charts and prove the backend chart renders a config
## the binary accepts (config.Load rejects unknown keys).
.PHONY: chart-check
chart-check: build-go
	helm lint charts/prober-backend charts/prober-agent
	helm template t charts/prober-backend --set tls.secretName=x \
		--set-json 'config.agents=[{"site":"fra","token":"t"}]' \
		| awk '/config\.yml: \|/{f=1;next} /^[^ ]/{f=0} f{sub(/^    /,"");print}' \
		> /tmp/prober-chart-config.yml
	./prober-backend -config /tmp/prober-chart-config.yml -check-config

.PHONY: clean
clean:
	rm -f prober-agent prober-backend

## dev: run backend and agent together locally over real TLS (dev certs).
## Grants the agent CAP_NET_RAW via sudo setcap (may prompt once), then runs
## both and shuts them down cleanly on Ctrl+C. Open http://127.0.0.1:8080.
.PHONY: dev
dev: build
	@./dev/run.sh
