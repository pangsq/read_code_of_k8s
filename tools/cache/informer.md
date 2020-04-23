# informer

## informer

例如podInformer

```
type podInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}
```

## sharedIndexInformer

### 结构体

核心组件是：
1. indexer Indexer  // 带索引本地cache（实现是threadSafeMap（map+rwmutex+indexers+indices））
2. controller       // 通过listWatcher拖取对象并放入一个DeltaFIFO
3. sharedProcessor  // 将DeltaFIFO中的对象分发到各个client

```
type sharedIndexInformer struct {
	indexer    Indexer
	controller Controller

	processor             *sharedProcessor
	cacheMutationDetector MutationDetector

	listerWatcher ListerWatcher

	objectType runtime.Object

	clock clock.Clock

	started, stopped bool
	startedLock      sync.Mutex

	blockDeltas sync.Mutex
}
```

### 实例化方法

:一般是通过informerFactory创建informer，再调用NewSharedIndexInformer

```
func NewSharedIndexInformer(lw ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration, indexers Indexers) SharedIndexInformer {
	realClock := &clock.RealClock{}
	sharedIndexInformer := &sharedIndexInformer{
		processor:                       &sharedProcessor{clock: realClock},
		indexer:                         NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, indexers),
		listerWatcher:                   lw,
		objectType:                      exampleObject,
		resyncCheckPeriod:               defaultEventHandlerResyncPeriod,
		defaultEventHandlerResyncPeriod: defaultEventHandlerResyncPeriod,
		cacheMutationDetector:           NewCacheMutationDetector(fmt.Sprintf("%T", exampleObject)),
		clock:                           realClock,
	}
	return sharedIndexInformer
}
```

传入参数

1. lw ListerWatcher
    1. List(options metav1.ListOptions) (runtime.Object, error)
    2. Watch(options metav1.ListOptions) (watch.Interface, error)

```
例如podInformer的listerWatcher
&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CoreV1().Pods(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.CoreV1().Pods(namespace).Watch(context.TODO(), options)
			},
		}
```
2. exampleObject runtime.Object   // 资源类型，例如&corev1.Pod{}
3. defaultEventHandlerResyncPeriod time.Duration // 缺省的event同步时间
    1. 用于resyncCheckPeriod和defaultEventHandlerResyncPeriod
    2. 具体逻辑之后分析
4. indexers Indexers // 索引，type Indexers map[string]IndexFunc

### 使用流程分析

1. 在启动前通过AddEventHandler注册handler（Add/Update/Delete）
    1. 生成listener
        1. determineNextResync(now) // 设置下次full resync的时间
    2. 向processor添加listener，如果已启动（listenersStarted）还要运行以下两个方法（started之后处理reflector获取事件的也是这俩方法）
        1. listener.run // 不断从nextCh中读取对象，执行handler的各个方法
        2. listener.pop // 维护一个可变长的环形缓冲区，不断从addCh读取对象，放入缓冲区中，再从缓冲区中取出，放入nextCh
2. sharedIndexInformer一般都是通过sharedInformerFactory和dynamicSharedInformerFactory创建并维护的，其启动也都是factory批量启动的：dynamicSharedInformerFactory/sharedInformerFactory .Start(stopCh <-chan struct{}) => go informer.Informer().Run(stopCh)
    1. 

#### 1. .AddEventHandler

```
func (s *sharedIndexInformer) AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) {
	s.startedLock.Lock()
	defer s.startedLock.Unlock()
    
    ... // 如果已stop则直接返回

	... // 调整resyncPeriod，不小于1s也不小于resyncCheckPeriod

    // 创建listener
	listener := newProcessListener(handler, resyncPeriod, determineResyncPeriod(resyncPeriod, s.resyncCheckPeriod), s.clock.Now(), initialBufferSize)
	// determineResyncPeriod的逻辑是取两者较大者（如任一为0则直接返回0）

    // 没有start，则直接添加
	if !s.started {
		s.processor.addListener(listener)
		return
	}

	// 已start，则停止blockDeltas后添加，并将已存在于indexer中的item作为Add事件触发
	s.blockDeltas.Lock()
	defer s.blockDeltas.Unlock()

	s.processor.addListener(listener)
	for _, item := range s.indexer.List() {
		listener.add(addNotification{newObj: item})
	}
}
```

#### 1.1 newProcessListener

```
func newProcessListener(handler ResourceEventHandler, requestedResyncPeriod, resyncPeriod time.Duration, now time.Time, bufferSize int) *processorListener {
	ret := &processorListener{
		nextCh:                make(chan interface{}),
		addCh:                 make(chan interface{}),
		handler:               handler,
		pendingNotifications:  *buffer.NewRingGrowing(bufferSize), // 目前传入的bufferSize是个定值 1024
		requestedResyncPeriod: requestedResyncPeriod,
		resyncPeriod:          resyncPeriod,
	}

    // 加锁，然后计算p.nextResync = now.Add(p.resyncPeriod)
	ret.determineNextResync(now)

	return ret
}
```

#### 1.2 .processor.addListener

```
func (p *sharedProcessor) addListener(listener *processorListener) {
	p.listenersLock.Lock()
	defer p.listenersLock.Unlock()

	p.addListenerLocked(listener)
	if p.listenersStarted {
		p.wg.Start(listener.run)
		p.wg.Start(listener.pop)
	}
}

func (p *sharedProcessor) addListenerLocked(listener *processorListener) {
	p.listeners = append(p.listeners, listener)
	p.syncingListeners = append(p.syncingListeners, listener)
}

// 非阻塞地运行一个方法，并在其他地方通过waitGroup等待此方法运行完毕
func (g *Group) Start(f func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		f()
	}()
}
```

#### 1.2.1 listener.run

```
func (p *processorListener) run() {
	stopCh := make(chan struct{})
	wait.Until(func() {
		for next := range p.nextCh { // nextCh是个没有缓冲的channel，不断从中取出对象
			switch notification := next.(type) {
			case updateNotification:
				p.handler.OnUpdate(notification.oldObj, notification.newObj)
			case addNotification:
				p.handler.OnAdd(notification.newObj)
			case deleteNotification:
				p.handler.OnDelete(notification.oldObj)
			default:
				utilruntime.HandleError(fmt.Errorf("unrecognized notification: %T", next))
			}
		}
		// the only way to get here is if the p.nextCh is empty and closed
		close(stopCh)  // 只要不关闭nextCh，则永远不会到达这
	}, 1*time.Second, stopCh) // wait.Until本意是每隔period（每次开始和结束等待下一轮时都会判断stopCh是否被激活）运行一遍func()，但此处由于stopCh在内部关闭，所以正常情况下如果一轮运行完毕（nextCh被关闭），不会进入第二轮； 此处这样写的目的是为了在func内部crash的时候隔1s接着运行
}
```

#### 1.2.2 listener.pop

```
func (p *processorListener) pop() {
	defer utilruntime.HandleCrash()
	defer close(p.nextCh) // Tell .run() to stop

	var nextCh chan<- interface{}
	var notification interface{}
	for {
		select {
		case nextCh <- notification:
			// Notification dispatched
			var ok bool
			notification, ok = p.pendingNotifications.ReadOne()
			if !ok { // Nothing to pop
				nextCh = nil // Disable this select case
			}
		case notificationToAdd, ok := <-p.addCh:
			if !ok {
				return
			}
			if notification == nil { // No notification to pop (and pendingNotifications is empty)
				// Optimize the case - skip adding to pendingNotifications
				notification = notificationToAdd
				nextCh = p.nextCh
			} else { // There is already a notification waiting to be dispatched
			    // 当存入的对象数达到当前缓冲区大小是，该缓冲区长度*2
				p.pendingNotifications.WriteOne(notificationToAdd)
			}
		}
	}
}
```

#### 2. .Run(stopCh <-chan struct{})

```
func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	fifo := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KnownObjects:          s.indexer,
		EmitDeltaTypeReplaced: true,
	})

	cfg := &Config{
		Queue:            fifo,
		ListerWatcher:    s.listerWatcher,
		ObjectType:       s.objectType,
		FullResyncPeriod: s.resyncCheckPeriod,
		RetryOnError:     false,
		ShouldResync:     s.processor.shouldResync,

		Process: s.HandleDeltas,
	}

	func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()

		s.controller = New(cfg)
		s.controller.(*controller).clock = s.clock
		s.started = true
	}()

	// Separate stop channel because Processor should be stopped strictly after controller
	processorStopCh := make(chan struct{})
	var wg wait.Group
	defer wg.Wait()              // Wait for Processor to stop
	defer close(processorStopCh) // Tell Processor to stop
	wg.StartWithChannel(processorStopCh, s.cacheMutationDetector.Run)
	wg.StartWithChannel(processorStopCh, s.processor.run)

	defer func() {
		s.startedLock.Lock()
		defer s.startedLock.Unlock()
		s.stopped = true // Don't want any new listeners
	}()
	s.controller.Run(stopCh)
}
```

### sharedIndexInformer

cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
