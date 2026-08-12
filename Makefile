.PHONY: build test lint control agent

build: control agent

control:
	mkdir -p bin
	go build -trimpath -o bin/relay-control ./cmd/relay-control

agent:
	mkdir -p bin
	go build -trimpath -o bin/relay-agent ./cmd/relay-agent

lint:
	go vet ./...
	npm run lint

test:
	go test ./...
	npm test
