# replicationcontroller

branch: release-v1.18

commit: ff809a5d953ba778270ce8790b21d394821e1e28

## 入口方法

`startReplicaSetController`

## 源码文件结构

```bash
replicaset
├── BUILD
├── config
│   ├── BUILD
│   ├── doc.go
│   ├── OWNERS
│   ├── types.go
│   ├── v1alpha1
│   │   ├── BUILD
│   │   ├── conversion.go
│   │   ├── defaults.go
│   │   ├── doc.go
│   │   ├── register.go
│   │   ├── zz_generated.conversion.go
│   │   └── zz_generated.deepcopy.go
│   └── zz_generated.deepcopy.go
├── doc.go
├── init_test.go
├── OWNERS
├── replica_set.go
├── replica_set_test.go
├── replica_set_utils.go
└── replica_set_utils_test.go

2 directories, 20 files
```

## 结构体

1. schema.GroupVersionKind
2. kubeClient               // 用于连接apiserver的client
3. podControl               // pod的控制器，因为rs会控制关联的pod副本数
4. burstReplicas            // 限流器的令牌桶容量
5. syncHandler func(rsKey string) error   // for test
6. expectations             // rc预期的pod数
7. rsLister                 // shared informer产生的rs的store
8. rsListerSynced           // rs store synced的标识
9. podLister                // shared informer产生的pod的store
10. podListerSynced         // pod store synced的标识
11. queue workqueue.RateLimitingInterface  // 需要进行同步的item

```
// ReplicaSetController is responsible for synchronizing ReplicaSet objects stored
// in the system with actual running pods.
type ReplicaSetController struct {
	// GroupVersionKind indicates the controller type.
	// Different instances of this struct may handle different GVKs.
	// For example, this struct can be used (with adapters) to handle ReplicationController.
	schema.GroupVersionKind

	kubeClient clientset.Interface
	podControl controller.PodControlInterface

	// A ReplicaSet is temporarily suspended after creating/deleting these many replicas.
	// It resumes normal action after observing the watch events for them.
	burstReplicas int
	// To allow injection of syncReplicaSet for testing.
	syncHandler func(rsKey string) error

	// A TTLCache of pod creates/deletes each rc expects to see.
	expectations *controller.UIDTrackingControllerExpectations

	// A store of ReplicaSets, populated by the shared informer passed to NewReplicaSetController
	rsLister appslisters.ReplicaSetLister
	// rsListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rsListerSynced cache.InformerSynced

	// A store of pods, populated by the shared informer passed to NewReplicaSetController
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podListerSynced cache.InformerSynced

	// Controllers that need to be synced
	queue workqueue.RateLimitingInterface
}
```

## 流程分析

### 1. 创建ReplicaSetController

1. informer的创建
    1. ctx.InformerFactory.Apps().V1().ReplicaSets()
    2. ctx.InformerFactory.Core().V1().Pods()
2. clientset.Interface的创建
    1. ctx.ClientBuilder.ClientOrDie("replicaset-controller")
3. eventBroadcaster的创建和初始化
    1. eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "replicaset-controller"}) // 往这个recorder中发送的事件均注明事件来源是rs
4. RealPodControl的创建，传入kubeClient和3中创建的recorder
    1. 这个recorder是用来主动向apiserver上传event的（比如rsController使用realPodControl创建pod时会生成一个"create pod"的事件）
5. NewBaseController // 为了让rc能够服用代码，所以中间嵌入一层
6. 创建expectations(NewControllerExpectations())和queue(DefaultControllerRateLimiter())
7. rsInformer设置handler(addRS/updateRS/deleteRS), Lister(), HasSynced
8. podInformer设置handler(addPod/updatePod/deletePod), Lister(), HasSynced
9. 设置rsc.syncHandler = rsc.syncReplicaSet

```
func startReplicaSetController(ctx ControllerContext) (http.Handler, bool, error) {
	if !ctx.AvailableResources[schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}] {
		return nil, false, nil
	}
	go replicaset.NewReplicaSetController(
		ctx.InformerFactory.Apps().V1().ReplicaSets(),
		ctx.InformerFactory.Core().V1().Pods(),
		ctx.ClientBuilder.ClientOrDie("replicaset-controller"),
		replicaset.BurstReplicas,
	).Run(int(ctx.ComponentConfig.ReplicaSetController.ConcurrentRSSyncs), ctx.Stop)
	return nil, true, nil
}

// NewReplicaSetController configures a replica set controller with the specified event recorder
func NewReplicaSetController(rsInformer appsinformers.ReplicaSetInformer, podInformer coreinformers.PodInformer, kubeClient clientset.Interface, burstReplicas int) *ReplicaSetController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return NewBaseController(rsInformer, podInformer, kubeClient, burstReplicas,
		apps.SchemeGroupVersion.WithKind("ReplicaSet"),
		"replicaset_controller",
		"replicaset",
		controller.RealPodControl{
			KubeClient: kubeClient,
			Recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "replicaset-controller"}),
		},
	)
}
```


### 2. Run

1. cache.WaitForNamedCacheSync
2. 并行地开启多个worker
3. worker中执行loop,rsc.processNextWorkItem()
4. queue.Get() -> syncHandler(key.(string)) -> Forget/AddRateLimited -> queue.Done(key)


```
// Run begins watching and syncing.
func (rsc *ReplicaSetController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer rsc.queue.ShutDown()

	controllerName := strings.ToLower(rsc.Kind)
	klog.Infof("Starting %v controller", controllerName)
	defer klog.Infof("Shutting down %v controller", controllerName)

	if !cache.WaitForNamedCacheSync(rsc.Kind, stopCh, rsc.podListerSynced, rsc.rsListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(rsc.worker, time.Second, stopCh)
	}

	<-stopCh
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (rsc *ReplicaSetController) worker() {
	for rsc.processNextWorkItem() {
	}
}

func (rsc *ReplicaSetController) processNextWorkItem() bool {
	key, quit := rsc.queue.Get()
	if quit {
		return false
	}
	defer rsc.queue.Done(key)

	err := rsc.syncHandler(key.(string))
	if err == nil {
		rsc.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	rsc.queue.AddRateLimited(key)

	return true
}
```

### 3. eventHandler

#### 3.1 addRS

将新增的rs加入到queue中

1. addRS(obj)
    1. 类型转换
    2. log
    3. enqueueRS(rs)
2. enqueueRS(rs)
    1. key, err := controller.KeyFunc(rs)
    2. rsc.queue.Add(key)

#### 3.2 updateRS

将修改的rs加入到queue中

1. updateRS(old, cur)
    1. 类型转换
    2. 如果cur和old的UID不一致，则删除old
    3. 如果期望的副本数和要求的副本数不一致，则log
    4. enqueueRS(curRS)
2. enqueueRS(curRS)
    1. key, err := controller.KeyFunc(rs)
    2. rsc.queue.Add(key)

#### 3.3 deleteRS

从expectations中删除对应的rs；将删除的rs加入到queue中

1. deleteRS(obj)
    1. 类型转换
    2. 如果rs处于删除状态则直接返回
    3. key, err := controller.KeyFunc(rs)
    4. log
    5. rsc.expectations.DeleteExpectations(key)
    6. rsc.queue.Add(key)

#### 3.4 addPod

如果pod存在对应的controllerRef，则先降低相应的expectations再将对应rs加入到queue中；否则查找与pod匹配的rs，全加入到queue中

1. addPod(obj)
    1. 类型转换
    2. 如果DeletionTimestamp非nil -> rsc.deletePod(pod),return
    3. 获取pod的controllerRef
        1. 获取对应的rs // UID相同
        2. rsKey, err := controller.KeyFunc(rs)
        3. rsc.expectations.CreationObserved(rsKey)
        4. rsc.queue.Add(rsKey)
        5. return
    4. rss := rsc.getPodReplicaSets(pod)
    5. for rs in rss: rsc.enqueueRS(rs)
    6. return
2. getPodReplicaSets(pod)  // 如果pod是孤儿pod，则查找是否有rs愿意认养
    1. rsc.rsLister.GetPodReplicaSets(pod)
3. rsLister.GetPodReplicaSets(pod)
    1. 查找满足pod的label条件（即labelSelector匹配）的rs

#### 3.5 updatePod

pod更新事件处理比较复杂，需要做好：1.确定两者版本不一致 2.如果存在删除时间戳，则直接deletePod 3.将curPod对应的rs入队 4.如果设置了MinReadySeconds，则等待一段时间后再次将curPod对应的rs入队 5.curPod处于孤儿状态时，为其找到匹配的rs，并入队

1. updatePod(old, cur)
    1. old/cur的version相同则不作处理，直接返回
    2. 如果存在DeletionTimestamp，则直接rsc.deletePod(curPod)，并且如果两者label不一致，也会调用rsc.deletePod(oldPod)
    3. 找到两者的controllerRef，如果不一致，则将old对应的rs入队(rsc.enqueueRS(rs))
    4. 如果cur存在controllerRef，则rsc.enqueueRS(rs)
    5. 如果oldPod状态非Ready，curPod状态为Ready，rs.Spec.MinReadySeconds > 0 ，则调用rsc.enqueueRSAfter(rs, (time.Duration(rs.Spec.MinReadySeconds)*time.Second)+time.Second)，之后返回
    6. 如果labelChanged || controllerRefChanged ,尝试为此时处在孤儿状态的pod找到controllerRef，并入队 for rs in rss: rsc.enqueueRS(rs)，然后返回
2. enqueueRSAfter(rs, duration)
    1. key, err := controller.KeyFunc(rs)
    2. rsc.queue.AddAfter(key, duration)

#### 3.6 deletePod

如果pod已处于删除状态（一系列的类型转换可判断），则直接返回；
否则找到对应的rs入队，并调整expectations

1. deletePod(obj)
    1. 类型转化
    2. 如果pod已处于删除状态则直接返回
    3. 找到对应的controllerRef和rs
    4. rsKey, err := controller.KeyFunc(rs)
    5. rsc.expectations.DeletionObserved(rsKey, controller.PodKey(pod))
    6. rsc.queue.Add(rsKey)

### 4. syncReplicaSet

```
// invoked concurrently with the same key.
func (rsc *ReplicaSetController) syncReplicaSet(key string) error {
    ...
    // 1. 从rsLister中根据key找到rs
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	rs, err := rsc.rsLister.ReplicaSets(namespace).Get(name)
	if errors.IsNotFound(err) {
		klog.V(4).Infof("%v %v has been deleted", rsc.Kind, key)
		rsc.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		return err
	}
    
    // 2. 判断是否需要同步，获取rs的selector
	rsNeedsSync := rsc.expectations.SatisfiedExpectations(key)
	selector, err := metav1.LabelSelectorAsSelector(rs.Spec.Selector)

    // 3. 获取rs关联的pods，并忽略不活跃（PodSucceeded、PodFailed、DeletionTimestamp非nil）的pods；深拷贝它们
	allPods, err := rsc.podLister.Pods(rs.Namespace).List(labels.Everything())
	// Ignore inactive pods.
	filteredPods := controller.FilterActivePods(allPods)

	filteredPods, err = rsc.claimPods(rs, selector, filteredPods)

    // 4. 如果需要同步并且没有删除时间戳则调用rsc.manageReplicas(filteredPods, rs)，之后获取新状态
	var manageReplicasErr error
	if rsNeedsSync && rs.DeletionTimestamp == nil {
		manageReplicasErr = rsc.manageReplicas(filteredPods, rs)
	}
	rs = rs.DeepCopy()
	newStatus := calculateStatus(rs, filteredPods, manageReplicasErr)

	// 5. 更新rs的status
	updatedRS, err := updateReplicaSetStatus(rsc.kubeClient.AppsV1().ReplicaSets(rs.Namespace), rs, newStatus)

	// 6. 如果设置了MinReadySeconds，则在一段时间将rs再次入队
	if manageReplicasErr == nil && updatedRS.Spec.MinReadySeconds > 0 &&
		updatedRS.Status.ReadyReplicas == *(updatedRS.Spec.Replicas) &&
		updatedRS.Status.AvailableReplicas != *(updatedRS.Spec.Replicas) {
		rsc.queue.AddAfter(key, time.Duration(updatedRS.Spec.MinReadySeconds)*time.Second)
	}
	return manageReplicasErr
}
```

#### 4.1 expectations.SatisfiedExpectations(key)

```
func (r *ControllerExpectations) SatisfiedExpectations(controllerKey string) bool {
	if exp, exists, err := r.GetExpectations(controllerKey); exists {
		if exp.Fulfilled() {
			return true
		} else if exp.isExpired() {
			return true
		} else {
			return false
		}
	} else if err != nil {
		...
	} else {
		...
	}
	return true
}
```

Fulfilled() 说明expectation已被满足（无论是增加还是删除）

```
atomic.LoadInt64(&e.add) <= 0 && atomic.LoadInt64(&e.del) <= 0
```

isExpired() 代表expectation期望的操作已经超时（5分钟）

```
clock.RealClock{}.Since(exp.timestamp) > ExpectationsTimeout
```

#### 4.2 manageReplicas(filteredPods, rs)

1. 计算副本数差值 diff := len(filteredPods) - int(*(rs.Spec.Replicas))
2. 如果diff<0，说明需要创建pod（超过burstReplicas则降低为burstReplicas=500）
    1. expectations.ExpectCreations(rsKey, diff)
    2. 使用慢启动算法，调用podControl创建pods
    3. 对于失败的pods，rsc.expectations.CreationObserved(rsKey)
3. 如果diff>0，说明需要删除pod
    1. rsc.expectations.ExpectDeletions(rsKey, getPodKeys(podsToDelete))
    2. 同时启动多个goroutine，并行地进行delete；sc.expectations.DeletionObserved(rsKey, podKey)