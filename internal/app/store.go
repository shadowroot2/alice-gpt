package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Conversation struct {
	Messages []Message `json:"messages"`
	LastID   int       `json:"last_id"`
	Reply    *Reply    `json:"reply,omitempty"`
	Turns    int       `json:"turns"`
	Pending  *Pending  `json:"pending,omitempty"`
}

type Pending struct {
	StartedAt time.Time `json:"started_at"`
	Question  string    `json:"question"`
	Answer    string    `json:"answer,omitempty"`
	Done      bool      `json:"done,omitempty"`
	Success   bool      `json:"success,omitempty"`
}

const pendingLockTTL = 40 * time.Second

type Store interface {
	Ping(context.Context) error
	Acquire(context.Context, string, string) (bool, error)
	Release(context.Context, string, string)
	Load(context.Context, string) (Conversation, error)
	Save(context.Context, string, Conversation) error
	Allow(context.Context, string, int) (bool, error)
}
type RedisStore struct {
	Client *redis.Client
	TTL    time.Duration
}

func (s *RedisStore) Ping(ctx context.Context) error { return s.Client.Ping(ctx).Err() }
func (s *RedisStore) Acquire(ctx context.Context, k, t string) (bool, error) {
	return s.Client.SetNX(ctx, k+":lock", t, pendingLockTTL).Result()
}

var unlock = redis.NewScript(`if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('del',KEYS[1]) else return 0 end`)

func (s *RedisStore) Release(ctx context.Context, k, t string) {
	_ = unlock.Run(ctx, s.Client, []string{k + ":lock"}, t).Err()
}
func (s *RedisStore) Load(ctx context.Context, k string) (Conversation, error) {
	c := Conversation{LastID: -1}
	b, err := s.Client.Get(ctx, k).Bytes()
	if errors.Is(err, redis.Nil) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid stored conversation")
	}
	return c, err
}
func (s *RedisStore) Save(ctx context.Context, k string, c Conversation) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.Client.Set(ctx, k, b, s.TTL).Err()
}

var rate = redis.NewScript(`local n=redis.call('incr',KEYS[1]); if n==1 then redis.call('expire',KEYS[1],60) end; return n`)

func (s *RedisStore) Allow(ctx context.Context, k string, max int) (bool, error) {
	n, err := rate.Run(ctx, s.Client, []string{k + ":rate"}).Int()
	return n <= max, err
}
