.PHONY: test check run worker build macos-app test-macos-app

test:
	go test ./...

check:
	go test -race ./...
	go vet ./...
	staticcheck ./...
	git diff --check

run:
	go run ./cmd/controlplane

worker:
	go run ./cmd/worker -id local-demo-1 -name local-demo-1 -adapter demo.sleep

build:
	mkdir -p bin
	go build -trimpath -o bin/controlplane ./cmd/controlplane
	go build -trimpath -o bin/worker ./cmd/worker
	go build -trimpath -o bin/expctl ./cmd/expctl
	go build -trimpath -o bin/kube-adapter ./cmd/kube-adapter

macos-app:
	./scripts/build-macos-app.sh

test-macos-app: macos-app
	./scripts/test-macos-app.sh
