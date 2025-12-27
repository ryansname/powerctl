.PHONY: build run clean check

build:
	go build -o powerctl ./src

run: build
	./powerctl --force-enable --debug

check: build
	golangci-lint run
	go test ./...
	nix-build -A goModules

clean:
	rm -f powerctl
