package service

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// relayTicketAudience 是 relay ticket 的唯一 aud;access_token 的 aud 是
// app_id,二者互不相通,access_token 不能冒充 relay ticket。
const relayTicketAudience = "relay"

// relayTicketLeeway 是校验 exp/iat 时的时钟偏移宽限(spec §4:30s)。
const relayTicketLeeway = 30 * time.Second

// RelayTicketService 签发/校验 relay ticket(HMAC-SHA256 JWS)。
// 校验是纯本地验签,不查库、无状态(spec §4)。secret 支持轮换:
// 校验时先尝当前 secret,失败再尝 prevSecret。
type RelayTicketService struct {
	secret     []byte
	prevSecret []byte
	ttl        time.Duration
	now        func() time.Time // 测试可注入;nil 用 time.Now
}

func NewRelayTicketService(secret, prevSecret string, ttl time.Duration) *RelayTicketService {
	return &RelayTicketService{
		secret:     []byte(secret),
		prevSecret: []byte(prevSecret),
		ttl:        ttl,
		now:        time.Now,
	}
}

// Issue 为 userID 签发一张 relay ticket,返回 token 与有效期秒数。
func (s *RelayTicketService) Issue(userID string) (string, int, error) {
	now := s.now()
	claims := jwt.RegisteredClaims{
		Issuer:    "yunhou-users", // token.go 无 issuer 常量,沿用其字面值
		Audience:  jwt.ClaimStrings{relayTicketAudience},
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		ID:        uuid.New().String(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", 0, err
	}
	return tok, int(s.ttl.Seconds()), nil
}

// Verify 校验 ticket,成功返回 user_id 与 exp;任何失败统一为
// ErrRelayTicketInvalid(不泄露具体原因)。
func (s *RelayTicketService) Verify(token string) (string, time.Time, error) {
	parse := func(secret []byte) (*jwt.RegisteredClaims, error) {
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(token, claims,
			func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, ErrRelayTicketInvalid
				}
				return secret, nil
			},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithAudience(relayTicketAudience),
			jwt.WithIssuer("yunhou-users"),
			jwt.WithLeeway(relayTicketLeeway),
			jwt.WithExpirationRequired(),
		)
		return claims, err
	}

	claims, err := parse(s.secret)
	if err != nil && len(s.prevSecret) > 0 {
		claims, err = parse(s.prevSecret)
	}
	if err != nil || claims.Subject == "" {
		return "", time.Time{}, ErrRelayTicketInvalid
	}
	return claims.Subject, claims.ExpiresAt.Time, nil
}
