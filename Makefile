.PHONY: build run clean

build:
	go build -o powerctl .

run: build
	./powerctl

clean:
	rm -f powerctl
