# cache

缓存，源码在`k8s.io/client-go/tools/cache`

版本
```
branch release-1.18
commit 88ba1c9b80efb715ed1d436cce4c48bd0970378f
Date:   Sat Apr 4 08:59:45 2020 -0700
```


```
// Package cache is a client-side caching mechanism. It is useful for
// reducing the number of server calls you'd otherwise need to make.
// Reflector watches a server and updates a Store. Two stores are provided;
// one that simply caches objects (for example, to allow a scheduler to
// list currently available nodes), and one that additionally acts as
// a FIFO queue (for example, to allow a scheduler to process incoming
// pods).
```

提供两种store：1.单纯的缓存objects；2.在1的基础上表现为FIFO队列(先进先出)

## store

### Interface

#### Store

```
type Store interface {

	// Add adds the given object to the accumulator associated with the given object's key
	Add(obj interface{}) error

	// Update updates the given object in the accumulator associated with the given object's key
	Update(obj interface{}) error

	// Delete deletes the given object from the accumulator associated with the given object's key
	Delete(obj interface{}) error

	// List returns a list of all the currently non-empty accumulators
	List() []interface{}

	// ListKeys returns a list of all the keys currently associated with non-empty accumulators
	ListKeys() []string

	// Get returns the accumulator associated with the given object's key
	Get(obj interface{}) (item interface{}, exists bool, err error)

	// GetByKey returns the accumulator associated with the given key
	GetByKey(key string) (item interface{}, exists bool, err error)

	// Replace will delete the contents of the store, using instead the
	// given list. Store takes ownership of the list, you should not reference
	// it after calling this function.
	Replace([]interface{}, string) error

	// Resync is meaningless in the terms appearing here but has
	// meaning in some implementations that have non-trivial
	// additional behavior (e.g., DeltaFIFO).
	Resync() error
}
```

#### ThreadSafeStore

作为Store的实现cache的成员变量，相对于Store而言，函数参数多了key string

```
type ThreadSafeStore interface {
	Add(key string, obj interface{})
	Update(key string, obj interface{})
	Delete(key string)
	Get(key string) (item interface{}, exists bool)
	List() []interface{}
	ListKeys() []string
	Replace(map[string]interface{}, string)
	Index(indexName string, obj interface{}) ([]interface{}, error)
	IndexKeys(indexName, indexKey string) ([]string, error)
	ListIndexFuncValues(name string) []string
	ByIndex(indexName, indexKey string) ([]interface{}, error)
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	AddIndexers(newIndexers Indexers) error
	// Resync is a no-op and is deprecated
	Resync() error
}
```

#### Indexer

扩展了Store的功能，方法与ThreadSafeStore相同。
```
type Indexer interface {
	Store
	// Index returns the stored objects whose set of indexed values
	// intersects the set of indexed values of the given object, for
	// the named index
	Index(indexName string, obj interface{}) ([]interface{}, error)
	// IndexKeys returns the storage keys of the stored objects whose
	// set of indexed values for the named index includes the given
	// indexed value
	IndexKeys(indexName, indexedValue string) ([]string, error)
	// ListIndexFuncValues returns all the indexed values of the given index
	ListIndexFuncValues(indexName string) []string
	// ByIndex returns the stored objects whose set of indexed values
	// for the named index includes the given indexed value
	ByIndex(indexName, indexedValue string) ([]interface{}, error)
	// GetIndexer return the indexers
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	AddIndexers(newIndexers Indexers) error
}
```

#### Controller

一个不断更新的缓存，用于sharedIndexInformer的实现。
1. 通过Reflector从ListerWatcher中获取对象并放入Queue）；
2. 并不断从Queue中Pop（fifo顺序）出object执行ProcessFunc


```
// Controller is a low-level controller that is parameterized by a
// Config and used in sharedIndexInformer.
type Controller interface {
	// Run does two things.  One is to construct and run a Reflector
	// to pump objects/notifications from the Config's ListerWatcher
	// to the Config's Queue and possibly invoke the occasional Resync
	// on that Queue.  The other is to repeatedly Pop from the Queue
	// and process with the Config's ProcessFunc.  Both of these
	// continue until `stopCh` is closed.
	Run(stopCh <-chan struct{})

	// HasSynced delegates to the Config's Queue
	HasSynced() bool

	// LastSyncResourceVersion delegates to the Reflector when there
	// is one, otherwise returns the empty string
	LastSyncResourceVersion() string
}
```

### Struct

#### cache

```
type cache struct {
	// cacheStorage bears the burden of thread safety for the cache
	cacheStorage ThreadSafeStore
	// keyFunc is used to make the key for objects stored in and retrieved from items, and
	// should be deterministic.
	keyFunc KeyFunc
}
```

#### threadSafeMap

```
type threadSafeMap struct {
	lock  sync.RWMutex
	items map[string]interface{}

	// indexers maps a name to an IndexFunc
	indexers Indexers
	// indices maps a name to an Index
	indices Indices
}
```

#### Indexers

Index:一个索引map，key为索引，value为sets.String（store中的key）
IndexFunc:计算obj索引的方法
Indexers:IndexFunc的集合map
Indices:Index的map

```
// Index maps the indexed value to a set of keys in the store that match on that value
type Index map[string]sets.String

type IndexFunc func(obj interface{}) ([]string, error)

// Indexers maps a name to a IndexFunc
type Indexers map[string]IndexFunc

// Indices maps a name to an Index
type Indices map[string]Index
```

#### Config

用于辅助实例化之后的controller。
ps: 构造者模式？

```
/ Config contains all the settings for one of these low-level controllers.
type Config struct {
	// The queue for your objects - has to be a DeltaFIFO due to
	// assumptions in the implementation. Your Process() function
	// should accept the output of this Queue's Pop() method.
	Queue

	// Something that can list and watch your objects.
	ListerWatcher

	// Something that can process a popped Deltas.
	Process ProcessFunc

	// ObjectType is an example object of the type this controller is
	// expected to handle.  Only the type needs to be right, except
	// that when that is `unstructured.Unstructured` the object's
	// `"apiVersion"` and `"kind"` must also be right.
	ObjectType runtime.Object

	// FullResyncPeriod is the period at which ShouldResync is considered.
	FullResyncPeriod time.Duration

	// ShouldResync is periodically used by the reflector to determine
	// whether to Resync the Queue. If ShouldResync is `nil` or
	// returns true, it means the reflector should proceed with the
	// resync.
	ShouldResync ShouldResyncFunc

	// If true, when Process() returns an error, re-enqueue the object.
	// TODO: add interface to let you inject a delay/backoff or drop
	//       the object completely if desired. Pass the object in
	//       question to this interface as a parameter.  This is probably moot
	//       now that this functionality appears at a higher level.
	RetryOnError bool
}
```

#### controller

1. config         Config       // 实例化时传入的配置
2. reflector      *Reflector   // Run时初始化
3. reflectorMutex sync.RWMutex // Run时初始化
4. clock          clock.Clock  // 计时器

controller本身的结构很简单，也并不直接生成一个存储数据的Store或Queue；
存放数据的Queue依旧在Config中，因此可知在Queue在实例化controller之前就必须准备好，而reflector/reflectorMutex是为了在之后的Run过程从listerWatcher中获取objects。

```
// `*controller` implements Controller
type controller struct {
	config         Config
	reflector      *Reflector
	reflectorMutex sync.RWMutex
	clock          clock.Clock
}
```

##### Run

1. 起一个goroutine，等待stopCh，以关闭Queue
2. 创建reflector，主要传入ListerWatcher方法、关注的对象类型、存放对象的Queue、FullResync的周期
3. 启动reflector的Run：通过ListAndWatch从apiserver中获取符合关注类型的objects
4. 启动controller的processLoop：不断从Queue中Pop出对象以调用config.Process

```
func (c *controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go func() {
		<-stopCh
		c.config.Queue.Close()
	}()
	r := NewReflector(
		c.config.ListerWatcher,
		c.config.ObjectType,
		c.config.Queue,
		c.config.FullResyncPeriod,
	)
	r.ShouldResync = c.config.ShouldResync
	r.clock = c.clock

	c.reflectorMutex.Lock()
	c.reflector = r
	c.reflectorMutex.Unlock()

	var wg wait.Group
	defer wg.Wait()

	wg.StartWithChannel(stopCh, r.Run)

	wait.Until(c.processLoop, time.Second, stopCh)
}

func (c *controller) processLoop() {
	for {
		obj, err := c.config.Queue.Pop(PopProcessFunc(c.config.Process))
		if err != nil {
			if err == ErrFIFOClosed {
				return
			}
			if c.config.RetryOnError {
				// This is the safe way to re-enqueue.
				c.config.Queue.AddIfNotPresent(obj)
			}
		}
	}
}

```

### Method

#### MetaNamespaceKeyFunc

从API objects中提取namespace/name作为object的key

```
func MetaNamespaceKeyFunc(obj interface{}) (string, error) {
	if key, ok := obj.(ExplicitKey); ok {
		return string(key), nil
	}
	meta, err := meta.Accessor(obj)
	if err != nil {
		return "", fmt.Errorf("object has no meta: %v", err)
	}
	if len(meta.GetNamespace()) > 0 {
		return meta.GetNamespace() + "/" + meta.GetName(), nil
	}
	return meta.GetName(), nil
}
```

### 实例化

```
func NewInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
) (Store, Controller) {
	// This will hold the client state, as we know it.
	clientState := NewStore(DeletionHandlingMetaNamespaceKeyFunc)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState)
}


func NewIndexerInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
	indexers Indexers,
) (Indexer, Controller) {
	// This will hold the client state, as we know it.
	clientState := NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, indexers)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState)
}
```

## fifo

### 接口

#### Queue

相较于Store，最显著的特点是作为一个队列新增了Pop方法，注释中提到此方法是阻塞的（直到队列中存在至少一个object或队列被close）

```
type Queue interface {
	Store

	Pop(PopProcessFunc) (interface{}, error)

	AddIfNotPresent(interface{}) error

	HasSynced() bool

	Close()
}

type PopProcessFunc func(interface{}) error
```

fifo.go提供了一个Pop使用的示例，如下。其思路是不对object做任何处理，直接返回，若queue被close则返回nil。

```
func Pop(queue Queue) interface{} {
	var result interface{}
	queue.Pop(func(obj interface{}) error {
		result = obj
		return nil
	})
	return result
}
```

### struct

#### FIFO

其用途：
1. 每个object只处理一次(exactly once)：针对同一个item的多个adds/updates也能保证处于待处理队列的item最终只被处理一次
2. 处理object时会处理最新的版本
3. 不处理已被删除的object
4. 不会周期性地重复处理objects

结构体的成员：
1. lock sync.RWMutex  // 读写锁
2. cond sync.Cond     // 实现阻塞的Pop
3. items map[string]interface{} // 存储object的key-value对（key通过keyFunc算出来,key作为了object的标示，而value对应了object的版本），保证了每个object只有一份value（版本）存在
4. queue []string    // 存储object的keys，保证了先后顺序
5. populated bool    // 一般FIFO或者其他Store在初始化的时候都会通过调用Replace将从其他地方取来的一堆数据塞入队列中，此时整个FIFO是不建议写的；通过HasSynced()来判断是否初始化完毕且通过Replace插入的数据已全部消费完
6. initialPopulationCount int // 初始化时通过Replace插入的items数目，未被消费的
7. keyFunc KeyFunc   // store包中常见的计算object键的方法
8. closed bool       // 是否被关闭
9. closedLock sync.Mutex // close时会做一些操作(cond.Broadcast(),即唤醒当前所有阻塞的Pop)，加锁以同步

```
type FIFO struct {
	lock sync.RWMutex
	cond sync.Cond
	// We depend on the property that every key in `items` is also in `queue`
	items map[string]interface{}
	queue []string

	// populated is true if the first batch of items inserted by Replace() has been populated
	// or Delete/Add/Update was called first.
	populated bool
	// initialPopulationCount is the number of items inserted by the first call of Replace()
	initialPopulationCount int

	// keyFunc is used to make the key used for queued item insertion and retrieval, and
	// should be deterministic.
	keyFunc KeyFunc

	// Indication the queue is closed.
	// Used to indicate a queue is closed so a control loop can exit when a queue is empty.
	// Currently, not used to gate any of CRED operations.
	closed     bool
	closedLock sync.Mutex
}
```

##### Add/AddIfNotPresent/Update

```
func (f *FIFO) Add(obj interface{}) error {
	id, err := f.keyFunc(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	if _, exists := f.items[id]; !exists {
		f.queue = append(f.queue, id)
	}
	f.items[id] = obj
	f.cond.Broadcast()
	return nil
}
```

若key已存在于items时，则直接替换items中key对应的value，不需要修改queue；而key不存在时，将key添加到queue尾部，同时往items塞入对应的key-value。

Update方法同Add方法。

而AddIfNotPresent(obj interface{})在key已存在时不会更新items

##### Delete

```
func (f *FIFO) Delete(obj interface{}) error {
	id, err := f.keyFunc(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	delete(f.items, id)
	return err
}
```

只删除items中的值，不处理queue。代码读到这可能会有疑问：1. queue残留的key被读到，但又从items取不到对应object怎么办？ 2. 后续相同的object加入的时候，queue中是否就存在了多个相同的key？

##### List/ListKeys/Get/GetByKey

全都是直接操作items，不关心queue。

##### Pop

1. 当queue队列为空格时，利用cond.Wait()阻塞等待
2. 取queue头部元素，并判断在items中是否存在（由于Delete会删除items中元素而残留key在queue，所以此处解决了Delete的疑问1），若不存在，进入下一轮循环
3. 将元素从items中删除，通过传入的process处理item，若处理失败，则将元素重新加入

```
func (f *FIFO) Pop(process PopProcessFunc) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
			// When Close() is called, the f.closed is set and the condition is broadcasted.
			// Which causes this loop to continue and return from the Pop().
			if f.IsClosed() {
				return nil, ErrFIFOClosed
			}

			f.cond.Wait()
		}
		id := f.queue[0]
		f.queue = f.queue[1:]
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
		item, ok := f.items[id]
		if !ok {
			// Item may have been deleted subsequently.
			continue
		}
		delete(f.items, id)
		err := process(item)
		if e, ok := err.(ErrRequeue); ok {
			f.addIfNotPresent(id, item)
			err = e.Err
		}
		return item, err
	}
}
```

##### Resync

确保items中的每个key都存在于queue，注释中申明此操作应该是no-op，即正常情况下本方法中的逻辑不会对items和queue进行任何改变。

##### HasSynced

```
// HasSynced returns true if an Add/Update/Delete/AddIfNotPresent are called first,
// or an Update called first but the first batch of items inserted by Replace() has been popped
// 联系上下文，此处的"or an Update called"是笔误，应为"or a Replace called"
func (f *FIFO) HasSynced() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.populated && f.initialPopulationCount == 0
}
```

#### deltaFIFO

与fifo的不同之处有两点：
1. store中的value存的不再只是单个object了，而是这个object的一个Delta values切片。说人话 就是会以数组形式存放object的所有版本，并附带这个版本相较于上个版本的变化（Added/Updated/Deleted/Replaced/Sync）。
2. 多一个叫Sync的方法，令object作用在accumulator（组合多个delta形成最新版本的object?）。具体看实现的方法了。
 
注释中表示deltaFIFO也有4个用途：
1. 处理每个object change(delta)最多一次
2. 每次处理一个object时，能够看到自从上一次处理之后这个object发生的所有变化
3. 能够处理object的删除
4. 能够周期性的重复处理相同的objects

结构体成员：
1. lock sync.RWMutex   // 读写锁
2. cond sync.Cond      // 实现读时的阻塞等待
3. items map[string]Deltas  // key-value对中，key是keyFunc计算object得出的键值，value是Delta数组
4. queue []string       // 顺序存放object的key
5. populated bool       // 如果使用Replace初始化，则表示初始化的数据是否已全部被消费掉；如果不是则直接为true
6. initialPopulationCount int //使用Replace初始化时，插入的数据
7. keyFunc KeyFunc     //计算object对应key的方法
8. knownObjects KeyListerGetter
9. closed bool
10. closedLock sync.Mutex
11. emitDeltaTypeReplaced bool // 当触发Replace()时DeltaType是选择Replaced或者Sync （区别见后文），为了做向后兼容；不同类型的delta处理上的区别可参见sharedIndexerInformer的handleDeltas


```
type DeltaFIFO struct {
	// lock/cond protects access to 'items' and 'queue'.
	lock sync.RWMutex
	cond sync.Cond

	// We depend on the property that items in the set are in
	// the queue and vice versa, and that all Deltas in this
	// map have at least one Delta.
	items map[string]Deltas
	queue []string

	// populated is true if the first batch of items inserted by Replace() has been populatedAdded/Updated/Deleted/Replaced/Sync
	// or Delete/Add/Update was called first.
	populated bool
	// initialPopulationCount is the number of items inserted by the first call of Replace()
	initialPopulationCount int

	// keyFunc is used to make the key used for queued item
	// insertion and retrieval, and should be deterministic.
	keyFunc KeyFunc

	// knownObjects list keys that are "known" --- affecting Delete(),
	// Replace(), and Resync()
	knownObjects KeyListerGetter

	// Indication the queue is closed.
	// Used to indicate a queue is closed so a control loop can exit when a queue is empty.
	// Currently, not used to gate any of CRED operations.
	closed     bool
	closedLock sync.Mutex

	// emitDeltaTypeReplaced is whether to emit the Replaced or Sync
	// DeltaType when Replace() is called (to preserve backwards compat).
	emitDeltaTypeReplaced bool
}
```

Deltas结构

```
// Change type definition
const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
	// Replaced is emitted when we encountered watch errors and had to do a
	// relist. We don't know if the replaced object has changed.
	//
	// NOTE: Previous versions of DeltaFIFO would use Sync for Replace events
	// as well. Hence, Replaced is only emitted when the option
	// EmitDeltaTypeReplaced is true.
	Replaced DeltaType = "Replaced"
	// Sync is for synthetic events during a periodic resync.
	Sync DeltaType = "Sync"
)

type Deltas []Delta

type Delta struct {
	Type   DeltaType
	Object interface{}
}
```

##### Add/AddIfNotPresent/Update

对于Add和Update来说，都调用了queueActionLocked，但传入了不同的DeltaType。

中间有一步骤newDeltas = dedupDeltas(newDeltas)旨在合并最新的两个Delta（实际上仅在两者都是Deleted的情况下进行合并，并保留非DeletedFinalStateUnknown的最新Delta）

```
// queueActionLocked appends to the delta list for the object.
// Caller must lock first.
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}

	newDeltas := append(f.items[id], Delta{actionType, obj})
	newDeltas = dedupDeltas(newDeltas)

	if len(newDeltas) > 0 {
		if _, exists := f.items[id]; !exists {
			f.queue = append(f.queue, id)
		}
		f.items[id] = newDeltas
		f.cond.Broadcast()
	} else {
		delete(f.items, id)
	}
	return nil
}

func dedupDeltas(deltas Deltas) Deltas {
	n := len(deltas)
	if n < 2 {
		return deltas
	}
	a := &deltas[n-1]
	b := &deltas[n-2]
	if out := isDup(a, b); out != nil {
		d := append(Deltas{}, deltas[:n-2]...)
		return append(d, *out)
	}
	return deltas
}

func isDup(a, b *Delta) *Delta {
	if out := isDeletionDup(a, b); out != nil {
		return out
	}
	// TODO: Detect other duplicate situations? Are there any?
	return nil
}


func isDeletionDup(a, b *Delta) *Delta {
	if b.Type != Deleted || a.Type != Deleted {
		return nil
	}
	// Do more sophisticated checks, or is this sufficient?
	if _, ok := b.Object.(DeletedFinalStateUnknown); ok {
		return a
	}
	return b
}
```

与Add/Update和之前的fifo有很大不同的在于，AddIfNotPresent的入参必须是Deltas类型的，因此此方法仅建议用在Pop()方法中（fifo中此方法一般也仅用于Pop()中）。

##### List/ListKeys/Get/GetByKey

1. List() []interface{} 返回object数组。
2. ListKeys() []string 返回key数组。
3. Get(obj interface{}) (item interface{}, exists bool, err error) 返回Deltas
4. GetByKey(key string) (item interface{}, exists bool, err error) 返回Deltas

##### Pop

与fifo的Pop方法逻辑相同。

##### Delete

当knownObjects和items中都不存在这个obj时直接返回nil，即不需要处理这个deletion；否则将附加了deleted标示的object存入Deltas中

```
func (f *DeltaFIFO) Delete(obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	if f.knownObjects == nil {
		if _, exists := f.items[id]; !exists {
			return nil
		}
	} else {
		_, exists, err := f.knownObjects.GetByKey(id)
		_, itemsExist := f.items[id]
		if err == nil && !exists && !itemsExist {
			return nil
		}
	}

	return f.queueActionLocked(Deleted, obj)
}
```

##### Replace

1. 根据emitDeltaTypeReplaced决定actionType是Sync或Replaced（为了向后兼容，因此时间线上应该是先有Replaced后有Sync）
2. 将object的key塞入keys（一个hashset），并调用queueActionLocked(action, item)放入items和queue
3. 处理knownObjects==nil的情况，向items中加入list的item，并清理items中不在list的item
    1. 遍历items
    2. 如果keys中已包含item的key则continue（说明下一步开始只处理非list中的item，而忽略本次由list加入的item）
    3. 取这些oldItem（非list加入的）的最新版本，并标识为DeletedFinalStateUnknown，调用queueActionLocked
    4. 置位populated，更新initialPopulationCount
4. 处理knownObjects!=nil的情况，向items中加入list的item，并清理knownObjects中不在list的item
    1. 遍历knownKeys
    2. 如果keys中已包含item的key则continue（说明下一步开始只处理非list中的knownKeys，而忽略本次由list加入的item）
    3. 取到不处于list的knownObject，并标识为DeletedFinalStateUnknown，调用queueActionLocked
    4. 置位populated，更新initialPopulationCount

```
func (f *DeltaFIFO) Replace(list []interface{}, resourceVersion string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	keys := make(sets.String, len(list))
    1. 选择actionType
	// keep backwards compat for old clients
	action := Sync
	if f.emitDeltaTypeReplaced {
		action = Replaced
	}
    2. 将object放入keys和items、queue
	for _, item := range list {
		key, err := f.KeyOf(item)
		if err != nil {
			return KeyError{item, err}
		}
		keys.Insert(key)
		if err := f.queueActionLocked(action, item); err != nil {
			return fmt.Errorf("couldn't enqueue object: %v", err)
		}
	}
    3. 
	if f.knownObjects == nil {
		// Do deletion detection against our own list.
		queuedDeletions := 0
		for k, oldItem := range f.items {
			if keys.Has(k) {
				continue
			}
			var deletedObj interface{}
			if n := oldItem.Newest(); n != nil {
				deletedObj = n.Object
			}
			queuedDeletions++
			if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
				return err
			}
		}

		if !f.populated {
			f.populated = true
			// While there shouldn't be any queued deletions in the initial
			// population of the queue, it's better to be on the safe side.
			f.initialPopulationCount = len(list) + queuedDeletions
		}

		return nil
	}
    4. 
	// Detect deletions not already in the queue.
	knownKeys := f.knownObjects.ListKeys()
	queuedDeletions := 0
	for _, k := range knownKeys {
		if keys.Has(k) {
			continue
		}

		deletedObj, exists, err := f.knownObjects.GetByKey(k)
		if err != nil {
			deletedObj = nil
			klog.Errorf("Unexpected error %v during lookup of key %v, placing DeleteFinalStateUnknown marker without object", err, k)
		} else if !exists {
			deletedObj = nil
			klog.Infof("Key %v does not exist in known objects store, placing DeleteFinalStateUnknown marker without object", k)
		}
		queuedDeletions++
		if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
			return err
		}
	}

	if !f.populated {
		f.populated = true
		f.initialPopulationCount = len(list) + queuedDeletions
	}

	return nil
}
```

##### Resync

将存在于knownObjects但不存在于items中的object同步过去；如果knownObjects为nil则不做任何事情。

