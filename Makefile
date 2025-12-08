.PHONY: build run clean check

build:
	go build -o powerctl ./src

run: build
	./powerctl

check: build
	golangci-lint run

clean:
	rm -f powerctl
