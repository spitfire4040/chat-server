CARGO := $(HOME)/.cargo/bin/cargo

.PHONY: build server client run-server run-client clean

build: server client

server:
	$(CARGO) build --release --bin server

client:
	$(CARGO) build --release --bin client

run-server:
	$(CARGO) run --bin server -- --addr 0.0.0.0:8080 --data ./data --workers 4

run-client:
	$(CARGO) run --bin client -- --addr localhost:8080

clean:
	$(CARGO) clean
	rm -rf data/
