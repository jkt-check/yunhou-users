-- Migration: 020_refresh_reuse_grace
-- Description: refresh-token 轮换宽限窗口（grace window）支持。
--
-- 背景：kaya 等移动端客户端在弱网下会遇到"轮换已提交但响应丢失"——客户端
-- 拿着旧 refresh token 重试，恰好撞上重用检测（ErrSessionAlreadyRevoked）
-- 被当成重放攻击吊销整个 family。服务端无法把第一次轮换的 token 重发
-- （库里只存 hash），正确做法是沿 rotated_to 链找到继任 session 并再次轮换。
--
-- revoked_at 记录吊销时间，rotated_to 指向同一次轮换产生的继任 session，
-- 二者共同支撑 RefreshToken 的 grace 窗口两段判定：
--   revoked_at 在窗口内且 rotated_to 非空 → 合法重试，沿链轮换继任 session
--   否则（窗口外 / 无链接）→ 真重放，吊销 family + 401
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS rotated_to UUID;

DO $$
BEGIN
    ALTER TABLE sessions ADD CONSTRAINT sessions_rotated_to_fkey
        FOREIGN KEY (rotated_to) REFERENCES sessions(id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_sessions_rotated_to
    ON sessions(rotated_to) WHERE rotated_to IS NOT NULL;
