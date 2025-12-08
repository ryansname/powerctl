.PHONY: build run clean

build:
	go build -o powerctl ./src

run: build
	./powerctl

clean:
	rm -f powerctl
