BIN_DIR := bin

.PHONY: all build clean server agent run-server run-agent run-agent-config test

all: build

build: server agent

server:
	@mkdir -p $(BIN_DIR)
	GO111MODULE=on go build -o $(BIN_DIR)/sds-server ./cmd/sds-server

agent:
	@mkdir -p $(BIN_DIR)
	GO111MODULE=on go build -o $(BIN_DIR)/sds-agent ./cmd/sds-agent

run-server: server
	$(BIN_DIR)/sds-server -http :8500

run-agent: agent
	$(BIN_DIR)/sds-agent -server http://127.0.0.1:8500 -ns default -service demo -port 8080 -addr 127.0.0.1 -ttl 15s

run-agent-config: agent
	$(BIN_DIR)/sds-agent -config examples/agent.demo.json

test:
	go test ./...

clean:
	rm -rf $(BIN_DIR)
