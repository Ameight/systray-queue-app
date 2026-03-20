export GOPROXY=https://proxy.golang.org

APP = systray-queue-app

.PHONY: build run clean

build:
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
	go build -o $(APP) .

run:
	go run .

clean:
	rm -f $(APP)
