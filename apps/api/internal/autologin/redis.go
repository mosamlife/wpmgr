package autologin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
)

// RedisPayload is the JSON value stored at autologin:<nonce_id> in Redis. It
// mirrors the columns the consume path needs so a Redis hit can complete
// without touching Postgres (the PG row is then marked consumed_at out of
// band for audit completeness).
type RedisPayload struct {
	TenantID          uuid.UUID `json:"tenant_id"`
	SiteID            uuid.UUID `json:"site_id"`
	TargetWPUserLogin string    `json:"target_wp_user_login"`
}

// NonceStore is the Redis-backed hot-path store (interface so tests can swap a
// fake). Set EX'es the value for the JWT lifetime; ConsumeOnce uses GETDEL so
// exactly ONE caller can win the race even if Redis is shared by many API
// instances. Missing key -> (RedisPayload{}, false, nil). Underlying transport
// errors return err and the caller falls back to Postgres.
type NonceStore interface {
	Set(ctx context.Context, nonceID string, payload RedisPayload, ttl time.Duration) error
	ConsumeOnce(ctx context.Context, nonceID string) (RedisPayload, bool, error)
}

// RedigoStore is the production NonceStore over the existing redigo pool the
// session manager already provisions (ADR-024 reuse). The Redis GETDEL command
// (Redis 6.2+) is used so the read and delete are atomic and a single API
// can never accidentally consume the same nonce twice.
type RedigoStore struct {
	pool *redis.Pool
}

// NewRedigoStore wires a RedigoStore over an already-built redigo pool.
func NewRedigoStore(pool *redis.Pool) *RedigoStore { return &RedigoStore{pool: pool} }

// Set persists the payload at key autologin:<nonceID> with the given TTL.
func (s *RedigoStore) Set(ctx context.Context, nonceID string, payload RedisPayload, ttl time.Duration) error {
	if s == nil || s.pool == nil {
		return errors.New("redis pool not configured")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal redis payload: %w", err)
	}
	conn, err := s.getConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// EX is in whole seconds; round up so the TTL never truncates below the
	// caller-intended value.
	exSec := int64(ttl.Round(time.Second) / time.Second)
	if exSec < 1 {
		exSec = 1
	}
	if _, err := conn.Do("SET", RedisKeyPrefix+nonceID, raw, "EX", exSec); err != nil {
		return fmt.Errorf("redis SET: %w", err)
	}
	return nil
}

// ConsumeOnce executes GETDEL to atomically read and delete the key, returning
// the parsed payload, a found flag, and any transport/marshal error. A missing
// key is found=false with nil err (Redis miss, not an error).
func (s *RedigoStore) ConsumeOnce(ctx context.Context, nonceID string) (RedisPayload, bool, error) {
	if s == nil || s.pool == nil {
		return RedisPayload{}, false, errors.New("redis pool not configured")
	}
	conn, err := s.getConn(ctx)
	if err != nil {
		return RedisPayload{}, false, err
	}
	defer func() { _ = conn.Close() }()
	raw, err := redis.Bytes(conn.Do("GETDEL", RedisKeyPrefix+nonceID))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			return RedisPayload{}, false, nil
		}
		return RedisPayload{}, false, fmt.Errorf("redis GETDEL: %w", err)
	}
	var out RedisPayload
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return RedisPayload{}, false, fmt.Errorf("decode redis payload: %w", uerr)
	}
	return out, true, nil
}

// getConn pulls a context-aware connection from the redigo pool.
func (s *RedigoStore) getConn(ctx context.Context) (redis.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.pool.GetContext(ctx)
}

// NoopStore is the disabled NonceStore used when Redis is not configured. It
// records nothing on Set and returns (empty, false, nil) on ConsumeOnce, which
// forces the service into the Postgres fallback path on every consume.
type NoopStore struct{}

// Set is a no-op.
func (NoopStore) Set(context.Context, string, RedisPayload, time.Duration) error { return nil }

// ConsumeOnce always returns "not found, no error" so the caller falls through.
func (NoopStore) ConsumeOnce(context.Context, string) (RedisPayload, bool, error) {
	return RedisPayload{}, false, nil
}
