# flowcontroller

限流控制器，源码在`k8s.io/client-go/util/flowcontrol`


## interface

### RateLimiter

```go
type RateLimiter interface {
	// TryAccept returns true if a token is taken immediately. Otherwise,
	// it returns false.
	TryAccept() bool
	// Accept returns once a token becomes available.
	Accept()
	// Stop stops the rate limiter, subsequent calls to CanAccept will return false
	Stop()
	// QPS returns QPS of this rate limiter
	QPS() float32
	// Wait returns nil if a token is taken before the Context is done.
	Wait(ctx context.Context) error
}
```

### Clock

```go
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}
```

## struct

### tokenBucketRateLimiter

用令牌桶算法实现的限流器，初始化时填满（token数为burst），并以qps的速率往其中填充令牌。

```go
type tokenBucketRateLimiter struct {
	limiter *rate.Limiter
	clock   Clock
	qps     float32
}

func NewTokenBucketRateLimiter(qps float32, burst int) RateLimiter {
	limiter := rate.NewLimiter(rate.Limit(qps), burst)
	return newTokenBucketRateLimiter(limiter, realClock{}, qps)
}

func newTokenBucketRateLimiter(limiter *rate.Limiter, c Clock, qps float32) RateLimiter {
	return &tokenBucketRateLimiter{
		limiter: limiter,
		clock:   c,
		qps:     qps,
	}
}

func (t *tokenBucketRateLimiter) TryAccept() bool {
	return t.limiter.AllowN(t.clock.Now(), 1)
}

// Accept will block until a token becomes available
func (t *tokenBucketRateLimiter) Accept() {
	now := t.clock.Now()
	t.clock.Sleep(t.limiter.ReserveN(now, 1).DelayFrom(now))
}

func (t *tokenBucketRateLimiter) Stop() {
}

func (t *tokenBucketRateLimiter) QPS() float32 {
	return t.qps
}

func (t *tokenBucketRateLimiter) Wait(ctx context.Context) error {
	return t.limiter.Wait(ctx)
}

```

### realClock

```go
type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}
func (realClock) Sleep(d time.Duration) {
	time.Sleep(d)
}
```

## 依赖结构

### golang.org/x/time/rate

提供Allow/Reserve/Wait三种方法，其区别在token不足时的表现。
1. Allow: token不足时返回false
2. Reserve: token不足时返回一个reservation和等待的时间
3. Wait: token不足时阻塞以等待token可用或context被关闭

```go
// Each of the three methods consumes a single token.
// They differ in their behavior when no token is available.
// If no token is available, Allow returns false.
// If no token is available, Reserve returns a reservation for a future token
// and the amount of time the caller must wait before using it.
// If no token is available, Wait blocks until one can be obtained
// or its associated context.Context is canceled.
//
// The methods AllowN, ReserveN, and WaitN consume n tokens.
type Limiter struct {
	limit Limit
	burst int

	mu     sync.Mutex
	tokens float64
	// last is the last time the limiter's tokens field was updated
	last time.Time
	// lastEvent is the latest time of a rate-limited event (past or future)
	lastEvent time.Time
}
```

## 用法

### 例子

```go
func TestMultithreadedThrottling(t *testing.T) {
	// Bucket with 100QPS and no burst
	r := NewTokenBucketRateLimiter(100, 1)

	// channel to collect 100 tokens
	taken := make(chan bool, 100)

	// Set up goroutines to hammer the throttler
	startCh := make(chan bool)
	endCh := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			// wait for the starting signal
			<-startCh
			for {
				// get a token
				r.Accept()
				select {
				// try to add it to the taken channel
				case taken <- true:
					continue
				// if taken is full, notify and return
				default:
					endCh <- true
					return
				}
			}
		}()
	}

	// record wall time
	startTime := time.Now()
	// take the initial capacity so all tokens are the result of refill
	r.Accept()
	// start the thundering herd
	close(startCh)
	// wait for the first signal that we collected 100 tokens
	<-endCh
	// record wall time
	endTime := time.Now()

	// tolerate a 1% clock change because these things happen
	if duration := endTime.Sub(startTime); duration < (time.Second * 99 / 100) {
		// We shouldn't be able to get 100 tokens out of the bucket in less than 1 second of wall clock time, no matter what
		t.Errorf("Expected it to take at least 1 second to get 100 tokens, took %v", duration)
	} else {
		t.Logf("Took %v to get 100 tokens", duration)
	}
}
```