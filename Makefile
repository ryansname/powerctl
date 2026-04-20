.PHONY: build run run-multiplus clean check

build:
	go build -o powerctl ./src

run: build
	./powerctl --force-enable --debug

run-multiplus: build
	./powerctl --multiplus-only --debug

check: build
	golangci-lint run
	go test ./...
	nix-build -A goModules --no-out-link

clean:
	rm -f powerctl
