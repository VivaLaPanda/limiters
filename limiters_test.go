package limiters_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/hashicorp/consul/api"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/stretchr/testify/suite"
	"go.etcd.io/etcd/client/v3"

	l "github.com/mennanov/limiters"
)

type fakeClock struct {
	mu      sync.Mutex
	initial time.Time
	t       time.Time
}

func newFakeClock() *fakeClock {
	now := time.Now()
	return &fakeClock{t: now, initial: now}
}

func newFakeClockWithTime(t time.Time) *fakeClock {
	return &fakeClock{t: t, initial: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(d time.Duration) {
	if d == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.initial
}

type LimitersTestSuite struct {
	suite.Suite
	etcdClient   *clientv3.Client
	redisClient  *redis.Client
	consulClient *api.Client
	zkConn       *zk.Conn
	logger       *l.StdLogger
}

func (s *LimitersTestSuite) SetupSuite() {
	var err error
	s.etcdClient, err = clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(os.Getenv("ETCD_ENDPOINTS"), ","),
		DialTimeout: time.Second,
	})
	s.Require().NoError(err)
	s.redisClient = redis.NewClient(&redis.Options{
		Addr: os.Getenv("REDIS_ADDR"),
	})
	s.consulClient, err = api.NewClient(&api.Config{Address: os.Getenv("CONSUL_ADDR")})
	s.Require().NoError(err)
	s.zkConn, _, err = zk.Connect(strings.Split(os.Getenv("ZOOKEEPER_ENDPOINTS"), ","), time.Second)
	s.Require().NoError(err)
	s.logger = l.NewStdLogger()
}

func (s *LimitersTestSuite) TearDownSuite() {
	s.Assert().NoError(s.etcdClient.Close())
	s.Assert().NoError(s.redisClient.Close())
}

func TestBucketTestSuite(t *testing.T) {
	suite.Run(t, new(LimitersTestSuite))
}

// lockers returns all possible lockers (including noop).
func (s *LimitersTestSuite) lockers(generateKeys bool) []l.DistLocker {
	return append(s.distLockers(generateKeys), l.NewLockNoop())
}

// distLockers returns distributed lockers only.
func (s *LimitersTestSuite) distLockers(generateKeys bool) []l.DistLocker {
	randomKey := uuid.New().String()
	consulKey := randomKey
	etcdKey := randomKey
	zkKey := "/" + randomKey
	if !generateKeys {
		consulKey = "dist_locker"
		etcdKey = "dist_locker"
		zkKey = "/dist_locker"
	}
	consulLock, err := s.consulClient.LockKey(consulKey)
	s.Require().NoError(err)
	return []l.DistLocker{
		l.NewLockEtcd(s.etcdClient, etcdKey, s.logger),
		l.NewLockConsul(consulLock),
		l.NewLockZookeeper(zk.NewLock(s.zkConn, zkKey, zk.WorldACL(zk.PermAll))),
	}
}

func (s *LimitersTestSuite) TestLimitContextCancelled() {
	clock := newFakeClock()
	capacity := int64(2)
	rate := time.Second
	var limiters []interface{}
	for _, b := range s.tokenBuckets(capacity, rate, clock) {
		limiters = append(limiters, b)
	}
	for _, b := range s.leakyBuckets(capacity, rate, clock) {
		limiters = append(limiters, b)
	}
	for _, w := range s.fixedWindows(capacity, rate, clock) {
		limiters = append(limiters, w)
	}
	for _, w := range s.slidingWindows(capacity, rate, clock, 1e-9) {
		limiters = append(limiters, w)
	}
	for _, b := range s.concurrentBuffers(capacity, rate, clock) {
		limiters = append(limiters, b)
	}
	type rateLimiter interface {
		Limit(context.Context) (time.Duration, error)
	}
	type concurrentLimiter interface {
		Limit(context.Context, string) error
	}

	for _, limiter := range limiters {
		done1 := make(chan struct{})
		go func(limiter interface{}) {
			defer close(done1)
			// The context is expired shortly after it is created.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			switch lim := limiter.(type) {
			case rateLimiter:
				_, err := lim.Limit(ctx)
				s.Error(err, "%T", limiter)

			case concurrentLimiter:
				s.Error(lim.Limit(ctx, "key"), "%T", limiter)
			}

		}(limiter)
		done2 := make(chan struct{})
		go func(limiter interface{}) {
			defer close(done2)
			<-done1
			ctx := context.Background()
			switch lim := limiter.(type) {
			case rateLimiter:
				_, err := lim.Limit(ctx)
				s.NoError(err, "%T", limiter)

			case concurrentLimiter:
				s.NoError(lim.Limit(ctx, "key"), "%T", limiter)
			}
		}(limiter)
		// Verify that the second go routine succeeded calling the Limit() method.
		<-done2
	}
}
