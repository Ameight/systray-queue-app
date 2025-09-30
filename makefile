export GOPROXY=https://proxy.golang.org

.PHONY: build-win
build-win:
	go build -ldflags="-H=windowsgui" -o systray-queue-app.exe


go mod tidy

# Сборка обычного бинаря
go build -o systray-queue-app