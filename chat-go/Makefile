.PHONY: build server client run-server clean

build: server client

server:
	go build -o bin/server ./cmd/server

client:
	go build -o bin/client ./cmd/client

run-server:
	go run ./cmd/server -addr :8080 -data ./data -workers 4

# Convenience: open a client in the current terminal.
run-client:
	go run ./cmd/client -addr localhost:8080

clean:
	rm -rf bin/ data/
