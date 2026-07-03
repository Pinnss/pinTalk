# Деплой pintalk на BananaPi R3 / OpenWrt 24.10

## Предусловия

Эта шпаргалка описывает деплой с доменом и Let's Encrypt (`tls.mode:
letsencrypt-domain`). Если домена нет — pintalk умеет `selfsigned` (по IP, с
самоподписанным сертом) и `letsencrypt-ip` (публичный IP без домена); подробности
в [../docs/INSTALL.ru.md](../docs/INSTALL.ru.md). Для домена нужно:

1. **Домен указывает на WAN-IP роутера** — `call.example.com` (или твой) должен резолвиться в публичный IP роутера. Без этого Let's Encrypt не выпустит сертификат.
2. **Порты доступны из интернета**: 80/TCP (ACME challenge), 443/TCP (HTTPS), 3478/UDP+TCP (TURN).
   - Если у провайдера CGNAT (МТС/Билайн на мобильных тарифах часто) — HTTP-01 не сработает. Тогда либо `selfsigned` + раздать CA, либо ставим за реверс-прокси с уже валидным сертом.
3. **Свободны 80 и 443** на роутере (на bpi-r3 это обычно так — LuCI висит на других портах либо только на LAN).

## Шаг 1. Сборка под aarch64

На разработческой машине:

```bash
make build-arm64
# → bin/pintalk-linux-arm64 (~12 МБ)
```

## Шаг 2. Bcrypt-хэш пароля

```bash
./bin/pintalk-linux-arm64 hash 'мой-надёжный-пароль'
# или временно в windows:
./bin/pintalk.exe hash 'мой-надёжный-пароль'
```

Скопируй вывод (начинается с `$2a$12$...`) — он пойдёт в config.yaml.

## Шаг 3. Конфиг

Проще всего — `pintalk init` (сам хеширует пароль и генерит секреты). Или скопируй
`config.example.yaml` локально как `config.yaml` и отредактируй:

```yaml
server:
  domain: call.example.com
  http_port: 80
  https_port: 443

tls:
  mode: letsencrypt-domain
  cert_cache: /etc/pintalk/certs

turn:
  enabled: true
  listen_ip: 0.0.0.0
  port: 3478
  external_ip: ""              # WAN IP роутера; "" — попробует слушать-IP
  realm: call.example.com
  shared_secret: "<openssl rand -hex 32>"
  cred_ttl_minutes: 60

hosts:
  - username: pin
    password_hash: "<сюда вставь bcrypt из шага 2>"
```

`external_ip` можно оставить пустым, если у роутера один WAN-IP — pion подставит его автоматически. Если за двойным NAT/CGNAT — обязательно укажи.

## Шаг 4. Заливка на роутер

```bash
ROUTER=root@192.168.2.1   # или WAN IP, как удобно

# каталоги
ssh $ROUTER 'mkdir -p /etc/pintalk/certs && chmod 700 /etc/pintalk/certs'

# бинарь
scp bin/pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk
ssh $ROUTER 'chmod +x /usr/bin/pintalk'

# конфиг (с реальными секретами)
scp config.yaml $ROUTER:/etc/pintalk/config.yaml
ssh $ROUTER 'chmod 600 /etc/pintalk/config.yaml'

# init.d
scp deploy/init.d/pintalk $ROUTER:/etc/init.d/pintalk
ssh $ROUTER 'chmod +x /etc/init.d/pintalk'

# открыть порты в файрволе
scp deploy/firewall-add.sh $ROUTER:/tmp/
ssh $ROUTER 'sh /tmp/firewall-add.sh && rm /tmp/firewall-add.sh'

# enable + start
ssh $ROUTER '/etc/init.d/pintalk enable && /etc/init.d/pintalk start'
```

## Шаг 5. Проверка

```bash
ssh $ROUTER 'logread -e pintalk | tail -30'
# должно быть:
#   [pintalk] dev mode  ← НЕ должно быть; мы не в dev
#   [pintalk] TURN listening on 0.0.0.0:3478 ...
#   [pintalk] http listening on :80 (ACME + redirect)
#   [pintalk] https listening on :443 (domain=call.example.com)
```

Открой в браузере `https://call.example.com` — должна появиться форма логина. Первый запрос за HTTPS будет с задержкой ~5–15 секунд: autocert получает серт у Let's Encrypt и кэширует в `/etc/pintalk/certs`.

## Обновление бинаря

```bash
make deploy ROUTER=root@192.168.2.1
```

(см. `Makefile` — заливает новый бинарь и перезапускает service.)

## Логи / отладка

```bash
ssh $ROUTER '/etc/init.d/pintalk status'
ssh $ROUTER 'logread -f -e pintalk'
ssh $ROUTER 'cat /etc/pintalk/config.yaml'      # проверить конфиг
ssh $ROUTER 'ls -la /etc/pintalk/certs'         # появились ли .pem
```

## Если autocert не получает сертификат

- Проверь, что `curl -v http://call.example.com/.well-known/acme-challenge/test` извне доходит до роутера и возвращает 404 (это сам pintalk).
- Проверь DNS: `dig +short call.example.com` должен совпадать с WAN-IP роутера: `ssh $ROUTER 'curl -s ifconfig.me'`.
- Если за CGNAT — HTTP-01 не работает. Варианты: переехать на VPS с публичным IP, или добавить DNS-01 (сейчас не реализовано, добавим если понадобится).

## Если WebRTC не соединяется

Открой DevTools → Console на обоих концах. Смотри `connectionstatechange` и `iceconnectionstate`. Типичные причины:
- TURN порт закрыт на WAN — проверь `nc -zv call.example.com 3478` снаружи.
- `external_ip` неправильный — TURN отдаёт ICE-кандидаты с приватным IP.
- Один из браузеров — Safari старой версии (бывают баги с TURN-TCP).
