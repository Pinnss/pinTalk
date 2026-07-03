# pintalk — инструкция по установке

*Read this guide in English: [INSTALL.en.md](INSTALL.en.md).*

pintalk — это один статический Go-бинарь: веб-интерфейс, сигналинг и TURN-relay
внутри. Ни Docker, ни базы данных, ни внешних зависимостей.

HTTPS обязателен (без него браузер не даёт доступ к камере/микрофону), но домен —
**нет**. Есть три режима сертификата:

| `tls.mode` | Когда использовать | Домен | Предупреждение браузера |
|---|---|---|---|
| `selfsigned` | локальная сеть / по IP | не нужен | да (или поставить CA один раз) |
| `letsencrypt-ip` | публичный IP, домена нет | не нужен | нет (серт ~6 дней, автопродление) |
| `letsencrypt-domain` | есть домен на сервер | нужен | нет |

---

## Способ 1. Установщик в одну команду (Linux + systemd)

Самый быстрый путь. Скрипт скачает нужный бинарь, проведёт мастер настройки,
поставит systemd-сервис и откроет порты:

```bash
curl -fsSL https://raw.githubusercontent.com/Pinnss/pinTalk/main/deploy/install.sh | sudo sh
```

Мастер спросит, есть ли у вас домен / публичный IP, и сам подберёт режим, придумает
пароль (или примет ваш), сгенерит секреты и сертификат. Дальше — откройте
напечатанную ссылку и логиньтесь.

Обновление — повторный запуск той же команды (конфиг остаётся на месте).

---

## Способ 2. Вручную

### Шаг 1. Взять бинарь

Скачайте из [GitHub Releases](https://github.com/Pinnss/pinTalk/releases):

| Файл | Платформа |
|---|---|
| `pintalk-linux-amd64` | Ubuntu / Debian / любой x86_64 Linux |
| `pintalk-linux-arm64` | ARM64: OpenWrt aarch64 (BPI-R3), Raspberry Pi 4/5, ARM-серверы |

Бинари статические (`CGO_ENABLED=0`) — один файл на любой дистрибутив нужной
архитектуры. Или соберите из исходников (Go 1.25+):

```bash
git clone https://github.com/Pinnss/pinTalk.git && cd pinTalk
make build-amd64   # → bin/pintalk-linux-amd64
make build-arm64   # → bin/pintalk-linux-arm64
```

### Шаг 2. Сгенерировать конфиг

Самый простой способ — мастер (сам хеширует пароль и создаёт секреты):

```bash
pintalk init
```

Он задаст несколько вопросов и запишет `config.yaml`. Хотите вручную — скопируйте
[`config.example.yaml`](../config.example.yaml) и заполните. Минимальный конфиг для
**локальной сети без домена**:

```yaml
server:
  listen_ip: ""          # "" = все интерфейсы
  http_port: 80
  https_port: 443

tls:
  mode: selfsigned
  cert_cache: /etc/pintalk/certs

turn:
  enabled: false         # в LAN P2P работает напрямую, TURN не нужен

hosts:
  - username: pin
    password_hash: "<вывод: pintalk hash 'мой-пароль'>"
```

Для **публичного IP без домена** — `tls.mode: letsencrypt-ip`, `tls.public_ip: <ваш IP>`,
`turn.enabled: true`, `turn.external_ip: <ваш IP>`. Для **домена** — `tls.mode:
letsencrypt-domain`, `server.domain: call.example.com`.

### Что нужно для каждого режима

- **selfsigned** — ничего снаружи; работает даже без интернета. Порт 443 (и 80 —
  для скачивания CA и редиректа) слушаются локально.
- **letsencrypt-domain** — домен резолвится в WAN-IP сервера; порты `80/tcp` и
  `443/tcp` открыты из интернета; **нет CGNAT** (иначе ACME HTTP-01 не пройдёт).
- **letsencrypt-ip** — публичный IP; порты `80/tcp` и `443/tcp` открыты; серт
  короткоживущий (~6 дней), pintalk продлевает его сам.
- **TURN** (`3478/tcp`+`3478/udp`) — нужен, когда собеседник за симметричным NAT /
  CGNAT. В чистом LAN можно не открывать.

---

## Linux (Ubuntu / Debian, systemd) — вручную

Проверено на Ubuntu 22.04/24.04 и Debian 12.

### 1. Установить бинарь

```bash
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
pintalk --help
```

### 2. Каталог конфига + сервисный пользователь

```bash
sudo useradd --system --home /etc/pintalk --shell /usr/sbin/nologin pintalk
sudo mkdir -p /etc/pintalk/certs
# сгенерить конфиг сразу в /etc/pintalk (cert-cache — абсолютный путь для systemd):
sudo pintalk init --config /etc/pintalk/config.yaml --cert-cache /etc/pintalk/certs
sudo chown -R pintalk:pintalk /etc/pintalk
sudo chmod 700 /etc/pintalk/certs
sudo chmod 600 /etc/pintalk/config.yaml
```

### 3. systemd-юнит

```bash
sudo cp deploy/systemd/pintalk.service /etc/systemd/system/pintalk.service
sudo systemctl daemon-reload
sudo systemctl enable --now pintalk
```

Юнит запускает pintalk от непривилегированного пользователя `pintalk` и выдаёт
`CAP_NET_BIND_SERVICE` для портов 80/443 без root.

### 4. Файрвол (для letsencrypt-* и TURN)

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
# self-signed:        [pintalk] https listening on :443 (self-signed TLS)
# letsencrypt-ip:     [pintalk] https listening on :443 (letsencrypt IP=<ваш IP>)
# letsencrypt-domain: [pintalk] https listening on :443 (letsencrypt domain=call.example.com)
```

Откройте напечатанную мастером ссылку. В режиме `letsencrypt-*` самый первый
HTTPS-запрос идёт ~5–15 секунд (получение сертификата).

### Обновление

```bash
sudo systemctl stop pintalk
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
sudo systemctl start pintalk
```

---

## OpenWrt

Проверено на OpenWrt 24.10 (BananaPi R3, aarch64). Штатный procd.

> Если LuCI занимает `:80/:443` на LAN — укажите в `config.yaml`
> `server.listen_ip` = **WAN IP** роутера.

### 1. Скопировать файлы на роутер

```bash
ROUTER=root@192.168.1.1

scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk
ssh $ROUTER 'chmod +x /usr/bin/pintalk'

# конфиг: проще сгенерить локально мастером, затем залить
pintalk init --config ./config.yaml --cert-cache /etc/pintalk/certs
ssh $ROUTER 'mkdir -p /etc/pintalk/certs && chmod 700 /etc/pintalk/certs'
scp config.yaml $ROUTER:/etc/pintalk/config.yaml
ssh $ROUTER 'chmod 600 /etc/pintalk/config.yaml'

scp deploy/init.d/pintalk $ROUTER:/etc/init.d/pintalk
ssh $ROUTER 'chmod +x /etc/init.d/pintalk'
```

### 2. Файрвол (для letsencrypt-* / TURN)

```bash
scp deploy/firewall-add.sh $ROUTER:/tmp/
ssh $ROUTER 'sh /tmp/firewall-add.sh && rm /tmp/firewall-add.sh'
```

### 3. Включить и запустить

```bash
ssh $ROUTER '/etc/init.d/pintalk enable && /etc/init.d/pintalk start'
ssh $ROUTER 'logread -e pintalk | tail -30'
```

### Обновление

```bash
make deploy ROUTER=root@192.168.2.1
```

---

## Если что-то не работает

**Браузер ругается на сертификат (режим `selfsigned`).**
Так и задумано — серт самоподписанный. Нажмите «Дополнительно → Всё равно
продолжить». Чтобы убрать предупреждение навсегда, откройте
`http://<хост>/pintalk-ca.crt`, скачайте и установите этот CA в доверенные на
каждом устройстве (телефоны/ПК). После этого — зелёный замок.

**Гостю тоже нужно нажать «продолжить».**
Да, в `selfsigned` предупреждение видит каждый, кто открывает ссылку впервые.
Либо раздайте им CA-файл, либо — если хотите совсем без предупреждений — заведите
домен (`letsencrypt-domain`) или получите серт на публичный IP (`letsencrypt-ip`).

**Сертификат Let's Encrypt не выпускается.**
Для домена: `dig +short call.example.com` должен совпадать с WAN-IP
(`curl -s ifconfig.me` с сервера), а `curl -v http://<host>/.well-known/acme-challenge/test`
снаружи — доходить до pintalk (404). Для IP: IP должен быть публичным и доступным
на порту 80. За CGNAT HTTP-01 не работает — берите VPS.

**Звонки соединяются в LAN, но не через интернет.**
Скорее всего закрыт TURN-порт — проверьте `nc -zv <host> 3478` снаружи. Если
сервер за NAT — укажите в `turn.external_ip` реальный WAN-IP.

**Нет кнопки демонстрации экрана.**
Захват экрана требует десктопного браузера (`getDisplayMedia`); на телефонах
кнопка скрыта намеренно.
