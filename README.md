# pintalk

Лёгкий 1-на-1 видеозвонок в браузере. Один Go-бинарь, встроенный TURN, автоматический Let's Encrypt. Заточен под запуск на BananaPi R3 / OpenWrt.

## Как работает

- Хост логинится по паролю, создаёт комнату, кидает ссылку гостю.
- Гость открывает ссылку, вводит имя, попадает в звонок.
- WebRTC P2P между браузерами; сервер только сигналинг и TURN-relay для случаев симметричного NAT.
- Демонстрация экрана (на десктопе), переключение камер, PiP-раскладка со свапом плиток.
- Graceful fallback: камера+микрофон → только микрофон → режим просмотра.

## Запуск (dev, на ноутбуке)

```bash
# сгенерить bcrypt-хэш пароля
go run ./cmd/pintalk hash 'мой-пароль'

# скопировать пример конфига и подставить хэш + домен
cp config.example.yaml config.yaml
# отредактировать config.yaml

# запустить (на 8080 без TLS — для локальной отладки)
go run ./cmd/pintalk serve --dev
```

Открыть `http://localhost:8080`, залогиниться `pin / <твой пароль>`.

## Сборка под BPI-R3

```bash
make build-arm64
# → bin/pintalk-linux-arm64
```

## Установка / деплой

- **[docs/INSTALL.ru.md](docs/INSTALL.ru.md)** — установка на Linux (Ubuntu/Debian, systemd) и OpenWrt (RU)
- **[docs/INSTALL.en.md](docs/INSTALL.en.md)** — the same guide in English
- `deploy/README.md` — подробная шпаргалка по деплою на BananaPi R3 / OpenWrt

Готовые бинари (linux-amd64, linux-arm64) — в [Releases](https://github.com/Pinnss/pinTalk/releases).
