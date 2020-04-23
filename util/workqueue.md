# workqueue

工作队列，源码在`k8s.io/client-go/util/workqueue`

版本
```
branch release-1.18
commit 88ba1c9b80efb715ed1d436cce4c48bd0970378f
Date:   Sat Apr 4 08:59:45 2020 -0700
```


```
// Package workqueue provides a simple queue that supports the following
// features:
//  * Fair: items processed in the order in which they are added.
//  * Stingy: a single item will not be processed multiple times concurrently,
//      and if an item is added multiple times before it can be processed, it
//      will only be processed once.
//  * Multiple consumers and producers. In particular, it is allowed for an
//      item to be reenqueued while it is being processed.
//  * Shutdown notifications.
```

workqueue包提供一个简单的队列，支持：有序处理；单个item只处理一次；多个消费者和生产者（线程安全）；提供关闭通知。

## interface

### workqueue.Interface

最基础的workqueue方法

```go
type Interface interface {
	Add(item interface{})
	Len() int
	Get() (item interface{}, shutdown bool)
	Done(item interface{})
	ShutDown()
	ShuttingDown() bool
}
```

### workqueue.DelayingInterface

在workqueue.Interface的基础上增加了AddAfter

```go
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	AddAfter(item interface{}, duration time.Duration)
}

```

### workqueue.RateLimiter

```
type RateLimiter interface {
	// When gets an item and gets to decide how long that item should wait
	When(item interface{}) time.Duration
	// Forget indicates that an item is finished being retried.  Doesn't matter whether its for perm failing
	// or for success, we'll stop tracking it
	Forget(item interface{})
	// NumRequeues returns back how many failures the item has had
	NumRequeues(item interface{}) int
}
```

### workqueue.RateLimitingInterface

在workqueue.DelayingInterface的基础上增加了AddRateLimited/Forget/NumRequeues

```
type RateLimitingInterface interface {
	DelayingInterface

	// AddRateLimited adds an item to the workqueue after the rate limiter says it's ok
	AddRateLimited(item interface{})

	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for perm failing
	// or for success, we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you
	// still have to call `Done` on the queue.
	Forget(item interface{})

	// NumRequeues returns back how many times the item was requeued
	NumRequeues(item interface{}) int
}
```

## struct

### workqueue.Type

```go
// Type is a work queue (see the package comment).
type Type struct {
	// queue defines the order in which we will work on items. Every
	// element of queue should be in the dirty set and not in the
	// processing set.
	queue []t       // t定义为interface{}，无类型

	// dirty defines all of the items that need to be processed.
	dirty set       // 自定义了set，含has/insert/delete方法

	// Things that are currently being processed are in the processing set.
	// These things may be simultaneously in the dirty set. When we finish
	// processing something and remove it from this set, we'll check if
	// it's in the dirty set, and if so, add it to the queue.
	processing set

	cond *sync.Cond

	shuttingDown bool

	metrics queueMetrics

	unfinishedWorkUpdatePeriod time.Duration
	clock                      clock.Clock
}
```

分析：
1. queue是核心结构体，一个接口数组
2. dirty是待处理的事件
3. processing是正在处理的事件

具体方法实现如下：

```
// Add marks item as needing processing.
func (q *Type) Add(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	if q.shuttingDown {
		return
	}
	if q.dirty.has(item) {
		return
	}

	q.metrics.add(item)

	q.dirty.insert(item)
	if q.processing.has(item) {
		return
	}

	q.queue = append(q.queue, item)
	q.cond.Signal()
}

// Len returns the current queue length, for informational purposes only. You
// shouldn't e.g. gate a call to Add() or Get() on Len() being a particular
// value, that can't be synchronized properly.
func (q *Type) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

// Get blocks until it can return an item to be processed. If shutdown = true,
// the caller should end their goroutine. You must call Done with item when you
// have finished processing it.
func (q *Type) Get() (item interface{}, shutdown bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		// We must be shutting down.
		return nil, true
	}

	item, q.queue = q.queue[0], q.queue[1:]

	q.metrics.get(item)

	q.processing.insert(item)
	q.dirty.delete(item)

	return item, false
}

// Done marks item as done processing, and if it has been marked as dirty again
// while it was being processed, it will be re-added to the queue for
// re-processing.
func (q *Type) Done(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.metrics.done(item)

	q.processing.delete(item)
	if q.dirty.has(item) {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	}
}

// ShutDown will cause q to ignore all new items added to it. As soon as the
// worker goroutines have drained the existing items in the queue, they will be
// instructed to exit.
func (q *Type) ShutDown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.shuttingDown = true
	q.cond.Broadcast()
}

func (q *Type) ShuttingDown() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	return q.shuttingDown
}

func (q *Type) updateUnfinishedWorkLoop() {
	t := q.clock.NewTicker(q.unfinishedWorkUpdatePeriod)
	defer t.Stop()
	for range t.C() {
		if !func() bool {
			q.cond.L.Lock()
			defer q.cond.L.Unlock()
			if !q.shuttingDown {
				q.metrics.updateUnfinishedWork()
				return true
			}
			return false

		}() {
			return
		}
	}
}
```

#### Add(item interface{})

1. 如果dirty中已存在则返回，否则往dirty和metrics中插入item
2. 如果processing中已存在则返回，否则往queue中添加item

#### Get() (item interface{}, shutdown bool)

1. 等待queue长度非0或shuttingDown条件满足
2. 如果queue长度依旧是0，说明1触发了shuttingDown所以返回shuttingDown信息
3. 从queue中取出item（queue长度减一），metrics.add(item)，往processing中插入item，dirty中删除item

#### Done(item interface{})

1. metrics.down(item)，从processing中删除item
2. 如果dirty中此时又有了item，则往queue中插入item

#### item流程 （忽略metrics）

```
        如果dirty中没有                   如果processing中没有    
Add()         ---->             dirty          ---->              queue
                                                                     |
                                                                     |  Get()
                                                 从dirty和queue中移除 | 
                                                                     ↓
                                                                processing
                                                                     |
                                                                     |Done(item)
                                               从processing中移除    |
                                          如果dirty中存在则放入queue ↓
                                                                
```

1. dirty属于准备队列，每个item只存在一副本；也具备缓冲池的功能，相同item在此合并
2. queue相当于就绪队列，其中的item只会从dirty中获取，如果processing中已存在相同item，则不会从dirty中获取
3. processing相当于运行队列，能保证processing中每个item只存在一副本
4. 结合1/2情况，可知：存在于queue中的item必然存在于dirty中，而dirty中item不重复，所以queue中也不会重复
5. 结合之前的情况，可知：set(dirty) <= set(queue+processing)

#### 阻塞

1. 只有Get时通过cond来做等待
2. 其余地方直接对cond.L进行加锁解锁

### workqueue.delayingType

```
type delayingType struct {
	Interface

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	stopCh chan struct{}
	// stopOnce guarantees we only signal shutdown a single time
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	heartbeat clock.Ticker

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	waitingForAddCh chan *waitFor

	// metrics counts the number of retries
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
type waitFor struct {
	data    t
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
type waitForPriorityQueue []*waitFor
```

具体实现方法如下：

```
// AddAfter adds the given item to the work queue after the given delay
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}
```

另有waitingLoop()

```
// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
func (q *delayingType) waitingLoop() {
    ...

	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)

	for {

		now := q.clock.Now()

		// Add ready entries
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
		    ...
			q.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// Set up a wait for the first item's readyAt (if one exists)
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
            ...
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			return

		case <-q.heartbeat.C():
			// continue the loop, which will add ready items

		case <-nextReadyAt:
			// continue the loop, which will add ready items
			
		case waitEntry := <-q.waitingForAddCh:
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				q.Add(waitEntry.data)
			}
		}
	}
```

#### 分析

1. waitFor 存放了添加的事件和其就绪的时间
2. waitingForAddCh 已添加事件队列和事件可处理队列桥梁作用，缓冲长度是1000
3. waitingForQueue 一个最小堆，基于就绪时间存放事件
3. go waitingLoop()用于处理事件，Loop中每次操作都会尝试从waitingForQueue中取出最近的事件判断是否就绪，并且如果队列非空则取第一个事件的时间用作select的一个触发分支->nextReadyAt.其触发时机有以下这些
    1. stopCh  关闭
    2. heartbeat.C()  10s的ticker（仅仅是在一些奇怪的突发事件时触发）
    3. nextReadyAt
    4. waitingForAddCh  当有AddAfter调用，其中为了一次性从队列中取出多个值，而直接又写了一套for逻辑

### 多种RateLimiter

1. BucketRateLimiter   // 最基础的令牌桶算法实现的rateLimiter，没有
2. ItemBucketRateLimiter  // 给每个item一个limiter
    1. NewItemBucketRateLimiter(r rate.Limit, burst int) *ItemBucketRateLimiter
3. ItemExponentialFailureRateLimiter // 给每个item设置基础延迟和最大延迟（超时时间），每次失败以baseDelay*2^<num-failures>做limit
    1. NewItemExponentialFailureRateLimiter(baseDelay time.Duration, maxDelay time.Duration) RateLimiter
    2. DefaultItemBasedRateLimiter() RateLimiter
4. ItemFastSlowRateLimiter // 在一定次数内，fastDelay，否则slowDelay
    1. NewItemFastSlowRateLimiter(fastDelay, slowDelay time.Duration, maxFastAttempts int) RateLimiter
5. MaxOfRateLimiter  // 内嵌多种rateLimiter，返回最糟糕的结构
    1. NewMaxOfRateLimiter(limiters ...RateLimiter) RateLimiter

#### DefaultControllerRateLimiter

结合了一个exponential和bucket

```
// DefaultControllerRateLimiter is a no-arg constructor for a default rate limiter for a workqueue.  It has
// both overall and per-item rate limiting.  The overall is a token bucket and the per-item is exponential
func DefaultControllerRateLimiter() RateLimiter {
	return NewMaxOfRateLimiter(
		NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		// 10 qps, 100 bucket size.  This is only for retry speed and its only the overall factor (not per item)
		&BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}
```

### rateLimitingType

```
// rateLimitingType wraps an Interface and provides rateLimited re-enquing
type rateLimitingType struct {
	DelayingInterface

	rateLimiter RateLimiter
}

// AddRateLimited AddAfter's the item based on the time when the rate limiter says it's ok
func (q *rateLimitingType) AddRateLimited(item interface{}) {
	q.DelayingInterface.AddAfter(item, q.rateLimiter.When(item))
}

func (q *rateLimitingType) NumRequeues(item interface{}) int {
	return q.rateLimiter.NumRequeues(item)
}

func (q *rateLimitingType) Forget(item interface{}) {
	q.rateLimiter.Forget(item)
}
```


## constructs

### queue

```
// New constructs a new work queue (see the package comment).
func New() *Type {
	return NewNamed("")
}

func NewNamed(name string) *Type {
	rc := clock.RealClock{}
	return newQueue(
		rc,
		globalMetricsFactory.newQueueMetrics(name, rc),
		defaultUnfinishedWorkUpdatePeriod,
	)
}

func newQueue(c clock.Clock, metrics queueMetrics, updatePeriod time.Duration) *Type {
	t := &Type{
		clock:                      c,
		dirty:                      set{},
		processing:                 set{},
		cond:                       sync.NewCond(&sync.Mutex{}),
		metrics:                    metrics,
		unfinishedWorkUpdatePeriod: updatePeriod,
	}
	go t.updateUnfinishedWorkLoop()
	return t
}
```

### delaying_queue

```
// NewDelayingQueue constructs a new workqueue with delayed queuing ability
func NewDelayingQueue() DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, "")
}

// NewNamedDelayingQueue constructs a new named workqueue with delayed queuing ability
func NewNamedDelayingQueue(name string) DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, name)
}

// NewDelayingQueueWithCustomClock constructs a new named workqueue
// with ability to inject real or fake clock for testing purposes
func NewDelayingQueueWithCustomClock(clock clock.Clock, name string) DelayingInterface {
	ret := &delayingType{
		Interface:       NewNamed(name),
		clock:           clock,
		heartbeat:       clock.NewTicker(maxWait),
		stopCh:          make(chan struct{}),
		waitingForAddCh: make(chan *waitFor, 1000),
		metrics:         newRetryMetrics(name),
	}

	go ret.waitingLoop()

	return ret
}
```

### rate_limiting_queue

```
// NewRateLimitingQueue constructs a new workqueue with rateLimited queuing ability
// Remember to call Forget!  If you don't, you may end up tracking failures forever.
func NewRateLimitingQueue(rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelayingQueue(),
		rateLimiter:       rateLimiter,
	}
}

func NewNamedRateLimitingQueue(rateLimiter RateLimiter, name string) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewNamedDelayingQueue(name),
		rateLimiter:       rateLimiter,
	}
}
```

## 总结

最常见的带限流器的workqueue定义如下

```
queue := NewRateLimitingQueue(DefaultControllerRateLimiter())
```

用法：

```
go func() {
    queue.AddRateLimited(item)
}
go func() {
    item, _ := queue.Get()
    queue.Done(item)
    queue.Forget(item)  // 不Forget的话，item的失败次数增加，后续该item的delay增加
}
```