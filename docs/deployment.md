# Deployment Runbook

Single-VPS deployment for Yunhou Users. Target: Ubuntu 24.04 with PostgreSQL
already running on `127.0.0.1:5432`. The app runs in Docker, Nginx + Certbot
on the host terminate TLS.

## First-time setup

1. **Install host packages**

   ```bash
   sudo apt update
   sudo apt install -y nginx certbot python3-certbot-nginx postgresql-client
   sudo systemctl enable --now nginx
   ```

2. **Set up the deploy directory**

   ```bash
   sudo mkdir -p /opt/yunhou-users
   sudo chown -R "$USER":"$USER" /opt/yunhou-users
   cd /opt/yunhou-users
   git clone git@github.com:yunhou/users.git .
   ```

3. **Generate RSA keys**

   ```bash
   mkdir -p keys
   openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
   openssl rsa -pubout -in keys/private.pem -out keys/public.pem
   chmod 600 keys/private.pem
   ```

4. **Configure environment**

   ```bash
   cp .env.example .env
   $EDITOR .env   # set DATABASE_URL, RSA key paths; add channel webhook secrets if you accept those channels
   chmod 600 .env
   ```

5. **Create the Postgres role and database** (one-time, as `postgres` superuser)

   ```bash
   sudo -u postgres psql -c "CREATE DATABASE yunhou_users"
   sudo -u postgres psql -c "CREATE USER yunhou WITH PASSWORD '<strong-password>'"
   sudo -u postgres psql -c "GRANT CONNECT ON DATABASE yunhou_users TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "GRANT USAGE ON SCHEMA public TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "GRANT CREATE ON SCHEMA public TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO yunhou"
   sudo -u postgres psql -d yunhou_users -c "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO yunhou"
   ```

6. **Apply migrations**

   ```bash
   # The standalone cmd/migrate binary owns the _migrations ledger.
   # It applies pending files in lexicographic order and skips anything
   # already recorded, so re-running is always a no-op. See
   # migrations/README.md for the DDL rules each migration must follow.
   docker compose run --rm migrate         # apply pending
   docker compose run --rm migrate -status # inspect ledger
   ```

7. **Install Nginx config**

   ```bash
   sudo cp deploy/nginx.conf /etc/nginx/sites-available/yunhou-users
   sudo ln -sf /etc/nginx/sites-available/yunhou-users /etc/nginx/sites-enabled/
   sudo rm -f /etc/nginx/sites-enabled/default
   sudo nginx -t && sudo systemctl reload nginx
   ```

   > **Client IPs & rate limiting:** the app pins gin's trusted proxies to
   > loopback + RFC1918 (`cmd/server/main.go`) so per-IP rate-limit buckets
   > key on the real client IP from `X-Forwarded-For`. This assumes nginx is
   > on the same host (or reached via the docker bridge). If you later put a
   > public-IP hop in front (off-host nginx, CDN, WAF), every user will share
   > one bucket keyed on that hop's IP — extend the trusted-proxies list in
   > `cmd/server/main.go` to include that hop.

8. **Open firewall**

   ```bash
   sudo ufw allow 22/tcp
   sudo ufw allow 80/tcp
   sudo ufw allow 443/tcp
   sudo ufw enable   # if not already enabled
   ```

9. **Start the app**

   ```bash
   docker compose up -d --build
   curl -fsS http://127.0.0.1:8080/healthz
   ```

10. **Install cron jobs**

    ```bash
    sudo install -m 644 ops/logrotate.conf /etc/logrotate.d/docker-yunhou-users
    echo '0 3 * * * ubuntu /opt/yunhou-users/ops/backup.sh >> /var/log/yunhou-users-backup.log 2>&1' \
      | sudo tee /etc/cron.d/yunhou-users-backup
    ```

## Daily operations

### Deploy a new version

```bash
cd /opt/yunhou-users
./deploy/deploy.sh
```

The script: `git pull` → `docker compose build` → `docker compose up -d` →
5-second wait → check container is `running` → `curl /healthz`. Any failure
exits non-zero; the previous container stays up.

To roll back: `git checkout <previous-tag-or-sha> && ./deploy/deploy.sh`.

### View logs

```bash
docker compose logs -f app               # app stdout/stderr
sudo tail -f /var/log/nginx/access.log   # Nginx access
sudo tail -f /var/log/nginx/error.log    # Nginx errors
tail -f /var/log/yunhou-users-backup.log # backup run history
```

### Check status

```bash
docker compose ps                 # container health
curl -s http://127.0.0.1:8080/healthz | jq .
sudo systemctl status nginx
```

### Restore from backup

```bash
# Run as postgres superuser to ensure proper ownership
gunzip -c /var/backups/yunhou-users/db-20260617T030000Z.sql.gz \
  | sudo -u postgres psql "$(grep ^DATABASE_URL /opt/yunhou-users/.env | cut -d= -f2-)"
```

## Domain upgrade (later)

1. Buy a domain, point an A record at the VPS IP, wait for DNS to propagate.
2. v1 uses the **GitHub OAuth Authorization Code flow** exclusively (`/auth/github/redirect` → `/auth/github/callback`). Callback URLs are stored per app in `apps.config.oauth_providers.github.callback_urls` (HTTPS only, except `http://localhost` / `http://127.0.0.1` / `http://[::1]` for dev). When you point a new domain at the API, you must add the corresponding `redirect_uri` to that whitelist before the BFF can issue a successful GitHub login. The server itself has no `DOMAIN` env var — the hostname is consumed only by the Nginx config (`server_name`).
3. Edit `/etc/nginx/sites-available/yunhou-users`:
   - Replace the 80 `server` block with:

     ```nginx
     server {
         listen 80;
         server_name api.yh.com;
         location /.well-known/acme-challenge/ { root /var/www/certbot; }
         location / { return 301 https://$host$request_uri; }
     }
     ```

   - Uncomment the 443 `server` block, set `server_name` to `api.yh.com`.
4. Issue the cert (Certbot will edit Nginx in place):

   ```bash
   sudo certbot --nginx -d api.yh.com
   ```
5. Install the cert renewal cron:

   ```bash
   echo '0 4 * * 1 root /opt/yunhou-users/ops/renew-cert.sh >> /var/log/yunhou-users-cert.log 2>&1' \
     | sudo tee /etc/cron.d/yunhou-users-cert
   ```
6. Redeploy to pick up the new env: `./deploy/deploy.sh`.

## Relay(kaya 远程控制 WebSocket)

Relay 为 kaya 客户端与受控设备之间提供 WebSocket 房间路由。未配置
`RELAY_TICKET_SECRET` 时整个模块关闭(下方两个端点均 404),无需任何操作。

**两个端点**

| 端点 | 说明 |
|---|---|
| `POST /relay/ticket` | 已登录用户换取一次性 WS ticket(HMAC 签名,TTL 5 分钟;entitlement 校验 + 0.5/s、burst 30 限流) |
| `GET /relay/ws` | WebSocket 长连接,凭 ticket 完成 hello 握手后进入房间路由 |

**环境变量**

| 变量 | 必填 | 说明 |
|---|---|---|
| `RELAY_TICKET_SECRET` | 启用时必填 | ticket HMAC 密钥。生成:`openssl rand -hex 32` |
| `RELAY_TICKET_SECRET_PREVIOUS` | 否 | 轮换期的旧密钥(只验证、不签发)。轮换流程:新密钥写入 `RELAY_TICKET_SECRET`,旧密钥挪到本变量,等旧 ticket 全部过期(TTL 5 分钟)后清空 |
| `RELAY_ALLOWED_ORIGINS` | 否 | WS 握手 Origin 白名单(逗号分隔,如 `https://www.yunhouai.com`)。空 = fail-closed:拒绝一切带 Origin 的握手;不带 Origin 的 native device 不受影响 |
| `APP_ENV` | 是 | 指标标签(`relay_*` Prometheus 指标带 `env` 标签区分环境) |

**nginx 依赖**:`/relay/ws` 必须走 `deploy/nginx.conf` 里的独立
`location = /relay/ws` 块 —— `proxy_http_version 1.1` + `Upgrade`/`Connection`
透传 + `proxy_read_timeout 120s`(大于 30s ping 周期,否则 keepalive 间隙被
nginx 掐断)。启用 443 server 块时同样需要复制该 location(模板内有注释提醒)。

**单实例约束(spec §11)**:房间状态全部在进程内存中,relay 只允许单实例
部署。水平扩容需要外部协调(粘性会话 + 跨实例路由),当前版本不支持——
不要对 `:8080` 起多副本。

**`/metrics` 暴露面提醒**:Prometheus 指标与 `/metrics` 端点只应绑定内网
/loopback,不要经 nginx 暴露到公网 —— `relay_*` 指标包含在线连接数等运营数据。
nginx 默认配置(`location /` 只代理到应用)不单独放行 `/metrics`,保持现状即可。

**优雅停机**:SIGTERM/SIGINT → handler 层对 `/relay/ws` 新握手返回 503 →
全部在线连接收到 `closed shutdown` 帧 → 等待 flush(≤5s 预算)→ 强制关闭。
客户端应把 `closed shutdown` 视为可重连信号,走 ticket 换新后重连。

## Troubleshooting

| Symptom | First check |
|---|---|
| 502 from Nginx | `docker compose ps` — container not running? `docker compose logs --tail=200 app` |
| `/healthz` 503 | Postgres down. `psql "$(grep ^DATABASE_URL /opt/yunhou-users/.env | cut -d= -f2-)" -c 'select 1'` |
| Cert expired | `sudo certbot certificates` then `sudo certbot renew --dry-run` |
| Disk full | `df -h` and `du -sh /var/lib/docker /var/backups/yunhou-users /var/log` |
| `401 invalid provider token` from `/auth/login` | The token sent by the consumer app is expired/revoked; the OAuth provider's userinfo endpoint rejected it. Refresh the provider token on the consumer app and retry. |
| Webhook `400 invalid signature` | Check the channel's secret env var matches the value registered in the channel's merchant console |
