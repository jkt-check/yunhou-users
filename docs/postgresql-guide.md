# PostgreSQL 设置指南（零基础版）

本指南带你从零开始配置 PostgreSQL，让集成测试能跑通。

---

## 第一步：检查 PostgreSQL 是否已安装并运行

在终端执行：

```bash
pg_isready
```

如果看到类似 `accepting connections` 的输出，说明 PostgreSQL 正在运行。

如果没有，启动它：

```bash
sudo systemctl start postgresql
```

---

## 第二步：创建数据库

```bash
psql -h localhost -U postgres -c "CREATE DATABASE yunhou_users;"
```

如果报错说数据库已存在，说明之前创建过，可以跳过这步。

验证数据库是否创建成功：

```bash
psql -h localhost -U postgres -c "SELECT datname FROM pg_database WHERE datname = 'yunhou_users';"
```

应该输出 `yunhou_users`。

---

## 第三步：执行数据库迁移（建表）

```bash
# 必须按顺序执行：001 创建核心表，002 简化订阅系统（依赖 001），003 添加支付/退款/Webhook 表
psql -h localhost -U postgres -d yunhou_users -f migrations/001_init.sql
psql -h localhost -U postgres -d yunhou_users -f migrations/002_simplify_plans.sql
psql -h localhost -U postgres -d yunhou_users -f migrations/003_payments.sql
```

这会创建 11 张表：

- 核心（001）：`users`、`social_identities`、`apps`、`subscriptions`、`sessions`
- 订阅（002）：`plans`
- 支付/退款/Webhook（003）：`orders`、`payments`、`refunds`、`webhook_events`、`audit_log`

验证表是否创建成功：

```bash
psql -h localhost -U postgres -d yunhou_users -c "\dt"
```

应该看到：

```
 apps
 audit_log
 orders
 payments
 plans
 refunds
 sessions
 social_identities
 subscriptions
 users
 webhook_events
```

---

## 第四步：生成 RSA 密钥对

项目使用 RSA256 签名 JWT，需要一对密钥文件：

```bash
make generate-keys
```

这会在 `keys/` 目录下生成 `private.pem` 和 `public.pem`。

验证文件存在：

```bash
ls -la keys/
```

---

## 第五步：设置环境变量

你需要设置以下环境变量。最简单的方式是编辑项目根目录的 `.env` 文件：

```bash
nano .env
```

填入以下内容（必需项）：

```env
DATABASE_URL=postgres://postgres@localhost/yunhou_users?sslmode=disable
```

> `DATABASE_URL` 是唯一必需的变量。其他都有合理默认值（详见 README.md）：
> - `PORT` 默认 `8080`
> - `RSA_PRIVATE_KEY_PATH` 默认 `keys/private.pem`
> - `RSA_PUBLIC_KEY_PATH` 默认 `keys/public.pem`
> - `JWT_ACCESS_TTL` 默认 `15m`
> - `JWT_REFRESH_TTL` 默认 `168h`（7 天）
>
> 支付渠道的 Webhook 密钥（`STRIPE_WEBHOOK_SECRET`、`WECHAT_PAY_API_V3_KEY`、`ALIPAY_PUBLIC_KEY_PATH`）只在接入对应渠道时才需要配置。`GITHUB_*` / `GOOGLE_*` 是**预留**配置项，当前直接登录模式不消费它们。

---

## 第六步：运行集成测试

所有环境准备好后，执行：

```bash
go test -race -count=1 ./tests/integration/
```

如果看到 `ok` 就说明全部通过。

---

## 常见问题

### 问题 1：`psql: error: FATAL: Peer authentication failed`

你的 PostgreSQL 使用了 peer 认证，需要加 `-h localhost` 用 TCP 连接：

```bash
# 错误写法（peer 认证会失败）
psql -U postgres -c "..."

# 正确写法（走 TCP，用密码或 trust 认证）
psql -h localhost -U postgres -c "..."
```

### 问题 2：`FATAL: role "postgres" does not exist`

PostgreSQL 没有默认的 postgres 用户，需要先创建：

```bash
sudo -u postgres createuser -s postgres
```

如果 sudo 需要密码但你在 WSL 中不方便输入，可以尝试：

```bash
# 先切换到 postgres 系统用户
sudo su - postgres
# 然后在 postgres 用户下执行
createuser -s postgres
exit
```

### 问题 3：`psql: error: FATAL: password authentication failed`

PostgreSQL 要求密码。两种解决方式：

**方式 A**：在连接字符串中加密码

```bash
psql -h localhost -U postgres -d yunhou_users -f migrations/001_init.sql
psql -h localhost -U postgres -d yunhou_users -f migrations/002_simplify_plans.sql
psql -h localhost -U postgres -d yunhou_users -f migrations/003_payments.sql
# 会提示输入密码，输入你设定的 postgres 密码
```

**方式 B**：临时设为 trust 模式（仅开发环境！）

编辑 `/etc/postgresql/16/main/pg_hba.conf`，把：
```
local   all   postgres   peer
```
改为：
```
local   all   postgres   trust
```
然后重启：
```bash
sudo systemctl restart postgresql
```

> ⚠️ trust 模式意味着任何人都可以无密码连接，**仅用于本地开发**。

### 问题 4：集成测试报 `connect db: ...`

说明程序连不上数据库。检查：

1. PostgreSQL 是否在运行：`pg_isready`
2. `DATABASE_URL` 是否正确
3. 当前用户的连接权限：`psql -h localhost -U postgres -c "SELECT 1"`

### 问题 5：集成测试报 `relation "users" already exists`

说明表已经存在，是正常的。如果想清空重来：

```bash
psql -h localhost -U postgres -d yunhou_users -c "DROP TABLE IF EXISTS sessions, subscriptions, social_identities, apps, users CASCADE;"
```

然后重新执行第三步的迁移命令。

---

## 常用 psql 命令速查

```bash
# 连接到数据库（进入交互式命令行）
psql -h localhost -U postgres -d yunhou_users

# 进入后可以执行的命令：
\dt            # 列出所有表
\d users       # 查看 users 表结构
SELECT * FROM users;   # 查看所有用户
SELECT count(*) FROM sessions;  # 查看 sessions 表有多少行
\q             # 退出

# 不进入交互式，直接执行一条 SQL
psql -h localhost -U postgres -d yunhou_users -c "SELECT count(*) FROM users;"

# 清空所有表的数据（不删表结构，测试间常用）
psql -h localhost -U postgres -d yunhou_users -c "DELETE FROM sessions; DELETE FROM subscriptions; DELETE FROM social_identities; DELETE FROM apps; DELETE FROM users;"
```

---

## 一键验证脚本

把以下内容保存为 `check_db.sh`，运行后可以一步确认数据库是否就绪：

```bash
#!/bin/bash
set -e

echo "检查 PostgreSQL..."
pg_isready

echo "检查数据库..."
psql -h localhost -U postgres -c "SELECT 1 FROM pg_database WHERE datname = 'yunhou_users';" | grep -q 1 && echo "✓ 数据库 yunhou_users 存在" || echo "✗ 数据库不存在，请执行: psql -h localhost -U postgres -c 'CREATE DATABASE yunhou_users;'"

echo "检查表..."
TABLE_COUNT=$(psql -h localhost -U postgres -d yunhou_users -t -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")
echo "  公共表数量: $TABLE_COUNT (需要 11 张：001_init + 002_simplify_plans + 003_payments)"

echo "检查密钥..."
[ -f keys/private.pem ] && echo "✓ private.pem 存在" || echo "✗ 缺少 private.pem，请执行: make generate-keys"
[ -f keys/public.pem ] && echo "✓ public.pem 存在" || echo "✗ 缺少 public.pem，请执行: make generate-keys"

echo "检查环境变量..."
[ -n "$DATABASE_URL" ] && echo "✓ DATABASE_URL 已设置" || echo "✗ DATABASE_URL 未设置，程序将无法启动"

echo ""
echo "如果全部 ✓，运行集成测试:"
echo "  go test -race -count=1 ./tests/integration/"
```
