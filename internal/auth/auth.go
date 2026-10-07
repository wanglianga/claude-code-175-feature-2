// Package auth 提供基于 Redis 的会话令牌认证(演示用: 用手机号登录换 token)。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const sessionPrefix = "deposit:session:"
const SessionTTL = 12 * time.Hour

type Session struct {
	Token string       `json:"token"`
	Actor domain.Actor `json:"actor"`
}

type Manager struct {
	rdb  *redis.Client
	pool *pgxpool.Pool
}

func NewManager(rdb *redis.Client, pool *pgxpool.Pool) *Manager {
	return &Manager{rdb: rdb, pool: pool}
}

// Login 按手机号查找用户并签发会话。演示系统不设密码, 生产环境应替换为密码/验证码校验。
func (m *Manager) Login(ctx context.Context, phone string) (*Session, error) {
	var id, name, role string
	err := m.pool.QueryRow(ctx,
		`SELECT id::text, display_name, role FROM users WHERE phone=$1`, phone).
		Scan(&id, &name, &role)
	if err != nil {
		return nil, fmt.Errorf("%w: 用户不存在", errors.New("NOT_FOUND"))
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	sess := Session{Token: token, Actor: domain.Actor{ID: id, Role: role, Name: name}}
	raw, _ := json.Marshal(sess)
	if err := m.rdb.Set(ctx, sessionPrefix+token, raw, SessionTTL).Err(); err != nil {
		return nil, err
	}
	return &sess, nil
}

// PutFixedSession 供 seed 写入固定演示 token。
func (m *Manager) PutFixedSession(ctx context.Context, token, userID, name, role string) error {
	sess := Session{Token: token, Actor: domain.Actor{ID: userID, Role: role, Name: name}}
	raw, _ := json.Marshal(sess)
	return m.rdb.Set(ctx, sessionPrefix+token, raw, SessionTTL*20).Err()
}

func (m *Manager) Resolve(ctx context.Context, token string) (*Session, error) {
	raw, err := m.rdb.Get(ctx, sessionPrefix+token).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errors.New("UNAUTHORIZED")
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
