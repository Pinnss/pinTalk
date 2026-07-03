# pintalk

Лёгкий 1-на-1 видеозвонок в браузере. Один Go-бинарь: веб-морда, сигналинг и
встроенный TURN. HTTPS на выбор — **по IP без домена** (самоподписанный серт),
**по домену** (Let's Encrypt) или **по публичному IP** (Let's Encrypt для IP).
Заточен под запуск на BananaPi R3 / OpenWrt, но работает на любом Linux и локально.

## Как работает

- Хост логинится по паролю, создаёт комнату, кидает ссылку гостю.
- Гость открывает ссылку, вводит имя, попадает в звонок.
- WebRTC P2P между браузерами; сервер только сигналинг и TURN-relay для случаев симметричного NAT.
- Демонстрация экрана (на десктопе), переключение камер, PiP-раскладка со свапом плиток.
- Graceful fallback: камера+микрофон → только микрофон → режим просмотра.

## Быстрый старт (без домена, в локальной сети)

Камера/микрофон в браузере работают только в «защищённом контексте» (HTTPS), поэтому
даже по IP нужен сертификат — pintalk сгенерит самоподписанный сам.

```bash
# 1. настройка (спросит пару вопросов, всё остальное сгенерит: пароль, секреты, серт)
pintalk init

# 2. запуск (порты 80/443 требуют прав — sudo, либо задай http_port/https_port 8080/8443)
sudo pintalk serve
```

Открой `https://<IP-этого-компа>` (его печатает `init`), нажми «всё равно продолжить»
на предупреждении браузера — и логинься. Чтобы убрать предупреждение, поставь один раз
локальный CA: он лежит на `http://<IP>/pintalk-ca.crt`.

### Один-командный установщик (Linux / systemd)

Скачает бинарь, проведёт мастер настройки, поставит systemd-сервис и откроет порты:

```bash
curl -fsSL https://raw.githubusercontent.com/Pinnss/pinTalk/main/deploy/install.sh | sudo sh
```

## Режимы TLS

| `tls.mode` | Когда | Домен | Предупреждение браузера |
|---|---|---|---|
| `selfsigned` | LAN / по IP, нет домена | не нужен | да (или поставить CA один раз) |
| `letsencrypt-ip` | публичный IP, нет домена | не нужен | нет (серт ~6 дней, автопродление) |
| `letsencrypt-domain` | есть домен на сервер | нужен | нет |

`init` сам подбирает режим по ответам. Пусто в `tls.mode` = авто: домен есть → `letsencrypt-domain`, иначе `selfsigned`.

## Запуск (dev, на ноутбуке)

```bash
# на 8080 без TLS — для локальной отладки (камера работает по http://localhost)
go run ./cmd/pintalk serve --dev
```

Открыть `http://localhost:8080`, залогиниться `pin / <твой пароль>`.

## Сборка

```bash
make build            # текущая платформа → bin/pintalk
make build-arm64      # BPI-R3 / aarch64 → bin/pintalk-linux-arm64
make build-amd64      # x86_64 → bin/pintalk-linux-amd64
```

## Установка / деплой

- **[docs/INSTALL.ru.md](docs/INSTALL.ru.md)** — установка на Linux (Ubuntu/Debian, systemd) и OpenWrt (RU)
- **[docs/INSTALL.en.md](docs/INSTALL.en.md)** — the same guide in English
- `deploy/README.md` — подробная шпаргалка по деплою на BananaPi R3 / OpenWrt

Готовые бинари (linux-amd64, linux-arm64) — в [Releases](https://github.com/Pinnss/pinTalk/releases).
