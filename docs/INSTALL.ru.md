# pintalk — инструкция по установке

*Read this guide in English: [INSTALL.en.md](INSTALL.en.md).*

pintalk — это один статический Go-бинарь: веб-интерфейс, сигналинг, TURN-relay и
автоматические сертификаты Let's Encrypt встроены внутрь. Ни Docker, ни базы
данных, ни внешних зависимостей.

Инструкция покрывает две площадки:

- [Linux (Ubuntu / Debian, systemd)](#linux-ubuntu--debian)
- [OpenWrt (роутеры, например BananaPi R3)](#openwrt)

## Предусловия (для обеих платформ)

1. **Домен указывает на публичный IP** — например, `call.example.com` должен
   резолвиться в WAN-адрес машины. Иначе Let's Encrypt не выпустит сертификат.
2. **Порты доступны из интернета**:
   - `80/tcp` — ACME HTTP-01 челлендж + редирект на HTTPS
   - `443/tcp` — само приложение
   - `3478/udp` и `3478/tcp` — TURN-relay (нужен, когда собеседник за симметричным NAT / CGNAT)
3. **Нет CGNAT** на стороне сервера. Если провайдер держит вас за CGNAT — HTTP-01
   не сработает и входящие звонки не дойдут; хостите на VPS.

## Где взять бинарь

Скачайте из [GitHub Releases](https://github.com/Pinnss/pinTalk/releases):

| Файл | Платформа |
|---|---|
| `pintalk-linux-amd64` | Ubuntu / Debian / любой x86_64 Linux |
| `pintalk-linux-arm64` | ARM64: OpenWrt aarch64 (BPI-R3), Raspberry Pi 4/5 (64-бит ОС), ARM-серверы |

Бинари полностью статические (`CGO_ENABLED=0`), поэтому один и тот же файл
работает на любом дистрибутиве нужной архитектуры — отдельных сборок «под Ubuntu»
и «под Debian» не требуется.

Либо соберите из исходников (Go 1.25+):

```bash
git clone https://github.com/Pinnss/pinTalk.git && cd pinTalk
CGO_ENABLED=0 go build -ldflags="-s -w" -o pintalk ./cmd/pintalk          # текущая платформа
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o pintalk-linux-arm64 ./cmd/pintalk
```

## Конфигурация (для обеих платформ)

Сгенерируйте bcrypt-хэш пароля хоста (на любой машине, хоть на Windows):

```bash
./pintalk hash 'мой-надёжный-пароль'
# → $2a$12$...
```

Создайте `config.yaml` по образцу [`config.example.yaml`](../config.example.yaml):

```yaml
server:
  domain: call.example.com     # ваш домен
  listen_ip: ""                # "" = все интерфейсы; укажите WAN IP, если :80/:443 заняты на LAN
  http_port: 80
  https_port: 443
  cert_cache: /etc/pintalk/certs

turn:
  enabled: true
  listen_ip: 0.0.0.0
  port: 3478
  external_ip: ""              # WAN IP; "" — определится по домену автоматически
  realm: call.example.com
  shared_secret: "<вывод команды: openssl rand -hex 32>"
  cred_ttl_minutes: 60

hosts:
  - username: pin
    password_hash: "<сюда bcrypt-хэш $2a$12$... из шага выше>"
```

---

## Linux (Ubuntu / Debian)

Проверено на Ubuntu 22.04/24.04 и Debian 12; на любом дистрибутиве с systemd — так же.

### 1. Установить бинарь

```bash
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
pintalk --help   # проверка
```

### 2. Создать сервисного пользователя и каталог конфига

```bash
sudo useradd --system --home /etc/pintalk --shell /usr/sbin/nologin pintalk
sudo mkdir -p /etc/pintalk/certs
sudo cp config.yaml /etc/pintalk/config.yaml
sudo chown -R pintalk:pintalk /etc/pintalk
sudo chmod 700 /etc/pintalk/certs
sudo chmod 600 /etc/pintalk/config.yaml
```

### 3. Установить systemd-юнит

Готовый юнит лежит в репозитории: [`deploy/systemd/pintalk.service`](../deploy/systemd/pintalk.service):

```bash
sudo cp deploy/systemd/pintalk.service /etc/systemd/system/pintalk.service
sudo systemctl daemon-reload
sudo systemctl enable --now pintalk
```

Юнит запускает pintalk от непривилегированного пользователя `pintalk` и выдаёт
`CAP_NET_BIND_SERVICE`, чтобы слушать порты 80/443 без root.

### 4. Открыть файрвол (если включён)

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 3478/tcp
sudo ufw allow 3478/udp
```

### 5. Проверить

```bash
systemctl status pintalk
journalctl -u pintalk -f
# ожидаем:
#   [pintalk] TURN listening on 0.0.0.0:3478 ...
#   [pintalk] http listening on :80 (ACME + redirect)
#   [pintalk] https listening on :443 (domain=call.example.com)
```

Откройте `https://call.example.com` — должна появиться форма логина. Самый первый
HTTPS-запрос занимает ~5–15 секунд: autocert получает сертификат и кэширует его
в `/etc/pintalk/certs`.

### Обновление

```bash
sudo systemctl stop pintalk
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
sudo systemctl start pintalk
```

---

## OpenWrt

Проверено на OpenWrt 24.10 (BananaPi R3, aarch64). Используется штатный procd.

> Если LuCI занимает `:80/:443` на LAN-интерфейсе — укажите в `config.yaml`
> `server.listen_ip` = **WAN IP** роутера, чтобы сервисы не поссорились за порты.

### 1. Скопировать файлы на роутер

С рабочей машины (`ROUTER=root@192.168.1.1` — подставьте свой адрес):

```bash
ROUTER=root@192.168.1.1

# бинарь
scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk
ssh $ROUTER 'chmod +x /usr/bin/pintalk'

# конфиг (с заполненными секретами)
ssh $ROUTER 'mkdir -p /etc/pintalk/certs && chmod 700 /etc/pintalk/certs'
scp config.yaml $ROUTER:/etc/pintalk/config.yaml
ssh $ROUTER 'chmod 600 /etc/pintalk/config.yaml'

# procd init-скрипт
scp deploy/init.d/pintalk $ROUTER:/etc/init.d/pintalk
ssh $ROUTER 'chmod +x /etc/init.d/pintalk'
```

### 2. Открыть файрвол

В репозитории есть готовый скрипт, добавляющий три правила через UCI:

```bash
scp deploy/firewall-add.sh $ROUTER:/tmp/
ssh $ROUTER 'sh /tmp/firewall-add.sh && rm /tmp/firewall-add.sh'
```

(Эквивалент разрешения `80/tcp`, `443/tcp`, `3478/tcp+udp` из зоны WAN.)

### 3. Включить и запустить

```bash
ssh $ROUTER '/etc/init.d/pintalk enable && /etc/init.d/pintalk start'
```

### 4. Проверить

```bash
ssh $ROUTER '/etc/init.d/pintalk status'
ssh $ROUTER 'logread -e pintalk | tail -30'
```

Затем откройте `https://call.example.com` в браузере (первый запрос медленный — см. выше).

### Обновление

```bash
scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk.new
ssh $ROUTER 'mv /usr/bin/pintalk.new /usr/bin/pintalk && chmod +x /usr/bin/pintalk && /etc/init.d/pintalk restart'
```

(Либо `make deploy ROUTER=root@...`, если работаете из клона репозитория.)

---

## Если что-то не работает

**Сертификат не выпускается.**
Проверьте DNS: `dig +short call.example.com` должен совпадать с WAN IP
(`curl -s ifconfig.me` с сервера). Проверьте доступность 80 порта снаружи:
`curl -v http://call.example.com/.well-known/acme-challenge/test` должен вернуть
404 (его отдаёт сам pintalk). За CGNAT HTTP-01 не работает.

**Звонки соединяются в LAN, но не через интернет.**
Скорее всего закрыт TURN-порт — проверьте `nc -zv call.example.com 3478` снаружи.
Если сервер за NAT — укажите в `turn.external_ip` реальный WAN IP, иначе TURN
раздаёт ICE-кандидаты с приватным адресом.

**Нет кнопки демонстрации экрана.**
Захват экрана требует десктопного браузера (`getDisplayMedia`); на телефонах
кнопка скрыта намеренно.
