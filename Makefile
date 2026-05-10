.PHONY: dev build build-arm64 hash deploy clean tidy

# Локальный запуск без TLS на :8080
dev:
	go run ./cmd/pintalk serve --dev

# Сборка под текущую платформу (для отладки)
build:
	go build -o bin/pintalk ./cmd/pintalk

# Production-сборка под BPI-R3 (ARMv8 / aarch64 / Linux)
build-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-ldflags="-s -w" \
		-o bin/pintalk-linux-arm64 ./cmd/pintalk

# bcrypt-хэш для config.yaml
hash:
	@read -p 'Password: ' p; go run ./cmd/pintalk hash "$$p"

# Заливка бинаря на роутер (требует, чтобы ROUTER был задан, напр. ROUTER=root@192.168.2.1)
ROUTER ?= root@192.168.2.1
deploy: build-arm64
	scp bin/pintalk-linux-arm64 $(ROUTER):/usr/bin/pintalk.new
	ssh $(ROUTER) 'mv /usr/bin/pintalk.new /usr/bin/pintalk && chmod +x /usr/bin/pintalk && /etc/init.d/pintalk restart'

tidy:
	go mod tidy

clean:
	rm -rf bin/
