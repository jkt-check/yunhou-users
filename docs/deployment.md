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
   $EDITOR .env   # set STATE_HMAC_KEY, DATABASE_URL, GITHUB_*, etc.
   chmod 600 .env
   ```

5. **Create the Postgres role and database** (one-time, as `postgres` superuser)

   ```bash
   sudo -u postgres psql <<'SQL'
   CREATE DATABASE yunhou_users;
   CREATE USER yunhou WITH PASSWORD '<strong-password>';
   GRANT CONNECT ON DATABASE yunhou_users TO yunhou;
   \c yunhou_users
   GRANT USAGE ON SCHEMA public TO yunhou;
   GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO yunhou;
   GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO yunhou;
   ALTER DEFAULT PRIVILEGES IN SCHEMA public
     GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO yunhou;
   ALTER DEFAULT PRIVILEGES IN SCHEMA public
     GRANT USAGE, SELECT ON SEQUENCES TO yunhou;
   SQL
   ```

6. **Apply migrations**

   ```bash
   psql "$(grep ^DATABASE_URL .env | cut -d= -f2-)" -f migrations/001_init.sql
   ```

7. **Install Nginx config**

   ```bash
   sudo cp deploy/nginx.conf /etc/nginx/sites-available/yunhou-users
   sudo ln -sf /etc/nginx/sites-available/yunhou-users /etc/nginx/sites-enabled/
   sudo rm -f /etc/nginx/sites-enabled/default
   sudo nginx -t && sudo systemctl reload nginx
   ```

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
gunzip -c /var/backups/yunhou-users/db-20260617T030000Z.sql.gz \
  | psql "$(grep ^DATABASE_URL /opt/yunhou-users/.env | cut -d= -f2-)"
```

## Domain upgrade (later)

1. Buy a domain, point an A record at the VPS IP, wait for DNS to propagate.
2. Edit `/opt/yunhou-users/.env`: set `DOMAIN=api.yh.com`, update
   `GITHUB_CALLBACK_URL=https://api.yh.com/callback/github` (and any other
   provider callback URLs).
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

## Troubleshooting

| Symptom | First check |
|---|---|
| 502 from Nginx | `docker compose ps` — container not running? `docker compose logs --tail=200 app` |
| `/healthz` 503 | Postgres down. `psql "$(grep ^DATABASE_URL /opt/younhou-users/.env \| cut -d= -f2-)" -c 'select 1'` |
| Cert expired | `sudo certbot certificates` then `sudo certbot renew --dry-run` |
| Disk full | `df -h` and `du -sh /var/lib/docker /var/backups/yunhou-users /var/log` |
| OAuth callback fails | Check `GITHUB_CALLBACK_URL` matches the registered GitHub OAuth app callback |
