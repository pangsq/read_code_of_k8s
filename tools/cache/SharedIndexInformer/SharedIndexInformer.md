# SharedIndexInformer

SharedIndexInformer是k8s的client-go中一个功能强大的工具，通过它可以在client端缓存apiserver中的资源实例状态，以提高对整集群管理、调谐的效率。

无论在controller-manager/scheduler等原生组件中还是用kubebuilder编写crd的controller中都会用到它，因此对它内部的实现进行了解是非常必要的。

SharedIndexInformer的运行大致可以表述成：
1. 利用ListWatch机制不断从apiserver处获取某一类型的资源实例，存入本地内存中的一个Indexer（带索引和其他丰富接口的缓存）
2. 并辅以一个Queue（按照先后顺序）对外进行通知（资源实例的变化——增删改 到来时触发用户事先自定义好的事件句柄）。
3. 一般情况下，触发的事件句柄会把资源变化事件放入一个workQueue，另有线程（可以有多个并行）从中拾取事件进行处理（基于本类型或其他类型资源的indexer作出正确的决策）。当然这是后话，此文暂不具体展开。

下文以三个层次对SharedIndexInformer进行分析，分别是：
1. 静态：数据结构，出于利用索引提供读性能、顺序访问、线程安全、缓冲、阻塞等待等目的而实现，如Indexer、Queue、RingGrowing等
2. 动态：运行机制，创建、初始化、运行、运行时的各种goroutine被带起的时机
3. 动静结合：完整的数据处理流程，从访问apiserver获取资源到分发、处理、存储资源的整个流程

：如下文出现代码均以release-1.18分支为例。

## 数据结构

### Store

Store，一个通用的对象存储和处理的接口，可以简单理解成：以一个map为核心（存取数据），并提供丰富的接口方法（Add/Update/Delete/List/ListKeys/Get/GetByKey/Replace/Resync）。

Store在client-go/tools/cache库中并没有直接的实现，而是延伸出了两种新接口：Indexer和Queue，分别作为带索引的Store和维护处理顺序的Store。在具体业务逻辑中，这两者往往结合使用。

Store/Indexer/Queue的继承关系与实现的结构体如下图所示：

<img src="store.png" width="1000" hegiht="600" align=center />

#### Indexer

实现Indexer的结构体是cache，而cache的核心在ThreadSafeStore（cache封装了ThreadSafeStore的相应方法，而Add/Update/Delete等方法实现上是先调用keyFunc获取对象的key再调用后者相应方法）。

keyFunc作用是计算某个对象的key，相同key的对象就可以确定是同一个对象实例的副本。由于底层数据存储的实现上是一个map，自然就可以用对象实例的新副本覆盖旧副本（或者如DeltaFIFO中那样保存不同版本）。

对于k8s的资源对象，普遍使用的keyFunc是MetaNamespaceKeyFunc，如对Pod就可以计算 `Pod的namespace/Pod的name` 作为key。


##### threadSafeMap

threadSafeMap实现了ThreadSafeStore，具体是如何实现的，有必要贴下其结构体。

```
type threadSafeMap struct {
	lock  sync.RWMutex
	items map[string]interface{}

	indexers Indexers
	indices Indices
}

type IndexFunc func(obj interface{}) ([]string, error)

type Index map[string]sets.String

type Indexers map[string]IndexFunc

type Indices map[string]Index
```

简单说明：
1. items作为存储对象key-value对的 核心结构
2. indexers存储一系列索引方法
3. indices存储通过将indexers中的索引方法作用在items上计算得出的索引key和索引values(此处有点绕，结合实例才方便理解)

举个例子：(模仿[nodeNameKeyIndex](https://github.com/kubernetes/kubernetes/blob/29e4e66b597e8fed0a41b77d99653941ae742103/pkg/controller/nodelifecycle/node_lifecycle_controller.go#L130:1)以nodename为索引)

假设此时items中含我们预设的Pod（精简了结构的），其key-value对如下所示：

```
"default/pod1" -> Pod{
    nodename: "node1"
}
"default/pod2" -> Pod{
    nodename: "node1"
}
```

同时该Indexer包含一个nodename索引，那么Indexers的key-value对如下所示：

```
"nodename" ->  nodeNameKeyIndex
```

nodeNameKeyIndex逻辑为：

```
func nodeNameKeyIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*Pod)
	if !ok {
		return []string{}, nil
	}
	if pod.nodename == "" {
		return []string{}, nil
	}
	return []string{pod.nodename}, nil
}
```

那么最终indices的数据组织如下：

```
"nodename" -> {
    "node1" -> { "default/pod1", "default/pod2" }
}
```

##### Indexer的使用例子

```golang
type Resource struct {
	Namespace string
	Name      string
	Nodename  string
}

func assertEquals(obj, target interface{}) {
	if obj != target {
		panic(fmt.Sprintf("%v is not equal to %v", obj, target))
	}
}

// cache.MetaNamespaceKeyFunc是client-go中常用的keyFunc(计算object的key)，将`namespace/name`作为对象的唯一键
func metaNamespaceKeyFunc(obj interface{}) (string, error) {
	if res, ok := obj.(*Resource); ok {
		return res.Namespace + "/" + res.Name, nil
	}
	return "", errors.New("Wrong resource type")
}

func useIndexer() {
	metaNamespaceKeyFunc := func(obj interface{}) (string, error) {
		if res, ok := obj.(*Resource); ok {
			return res.Namespace + "/" + res.Name, nil
		}
		return "", errors.New("Wrong resource type")
	}
	// 以nodename为键的索引
	indexByNodename := func(obj interface{}) ([]string, error) {
		res := obj.(*Resource)
		nodename := res.Nodename
		return []string{nodename}, nil
	}
	// 索引可以在初始化Indexer的时候放入，也可以通过AddIndexers添加
	resourceIndexer := cache.NewIndexer(cache.KeyFunc(metaNamespaceKeyFunc),
		cache.Indexers{"byNodename": indexByNodename})
	defaultRes1 := &Resource{
		Namespace: "default",
		Name:      "res1",
		Nodename:  "node1",
	}
	extendRes1 := &Resource{
		Namespace: "extend",
		Name:      "res1",
		Nodename:  "node1",
	}
	// listWatch的list会调用Replace将现存的事例批量导入到Indexer中
	// resourceVersion在目前client-go实现的几个Store中并没有实际使用到
	resourceIndexer.Replace([]interface{}{
		defaultRes1, extendRes1,
	}, "0")

	// 根据对象获取对象
	res, _, _ := resourceIndexer.Get(defaultRes1)
	assertEquals(res.(*Resource).Nodename, "node1")

	// 通过key获取对象
	res, _, _ = resourceIndexer.GetByKey("default/res1")
	assertEquals(res.(*Resource).Nodename, "node1")

	// 添加对象，如果已存在则是修改已有对象，实际实现上与Update并无二致
	newRes := &Resource{ // 构造与res1同一个对象（namespace和name一致即可判定为同一资源对象），仅修改nodename
		Namespace: "default",
		Name:      "res1",
		Nodename:  "node2",
	}
	resourceIndexer.Add(newRes)
	// 修改后，存在以node2为键的byNodename索引了
	reses, _ := resourceIndexer.ByIndex("byNodename", "node2")
	assertEquals(len(reses), 1)
	assertEquals(reses[0].(*Resource).Name, "res1")
}
```

#### Queue

实现Queue的结构体有FIFO和DeltaFIFO，两者差别在于前者存放对象的一个最新版本，而后者可以存放对象的多个历史版本。

Delta具体是何物，与对象本身有什么联系，通过阅读下面的代码很容易理解。

```
type Deltas []Delta

type Delta struct {
	Type   DeltaType
	Object interface{}
}

const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
	Replaced DeltaType = "Replaced"
	Sync DeltaType = "Sync"
)
```

FIFO和DeltaFIFO使用`queue []string`数组来维护先后顺序，并用sync.Cond来实现Pop时的阻塞（相较于异步非阻塞，同步阻塞的方法对于使用者的心智负担无疑是更小的）

##### Queue的使用例子

```
func useQueue() {
	queue := cache.NewDeltaFIFO(metaNamespaceKeyFunc, nil)
	lock := sync.Mutex{}
	cond := sync.NewCond(&lock)

	// ch := make(chan struct{})
	go func() {
		defaultRes1 := &Resource{
			Namespace: "default",
			Name:      "res1",
			Nodename:  "node1",
		}
		extendRes1 := &Resource{
			Namespace: "extend",
			Name:      "res1",
			Nodename:  "node1",
		}
		// Replace一般用于初始化
		queue.Replace([]interface{}{
			defaultRes1, extendRes1,
		}, "0")

		// 等待初始化放入(Sync)的资源被消费完毕
		lock.Lock()
		for !queue.HasSynced() {
			cond.Wait()
		}
		lock.Unlock()

		// 修改资源
		defaultRes2 := &Resource{
			Namespace: "default",
			Name:      "res2",
			Nodename:  "node1",
		}
		newDefaultRes2 := &Resource{
			Namespace: "default",
			Name:      "res2",
			Nodename:  "node2",
		}
		queue.Add(defaultRes2)
		queue.Update(newDefaultRes2)

		time.Sleep(1 * time.Second)
		queue.Close()
		// close()
	}()
	records := []string{}
	recordFunc := func(obj interface{}) error {
		deltas := obj.(cache.Deltas)
		// 取最新的状态
		res := deltas.Newest().Object.(*Resource)

		record := fmt.Sprintf("%v/%v is on %s , last change is %v, oldest change is %v",
			res.Namespace, res.Name, res.Nodename, deltas.Newest().Type, deltas.Oldest().Type)
		records = append(records, record)
		// fmt.Println(record)
		cond.Signal()
		return nil
	}
	for {
		_, err := queue.Pop(cache.PopProcessFunc(recordFunc))
		// 一般处理器会限流
		time.Sleep(10 * time.Millisecond)
		if err != nil {
			assertEquals(err, cache.ErrFIFOClosed)
			break
		}
	}
	// for _, rec := range records {
	// 	fmt.Println(rec)
	// }
	assertEquals(records[0], "default/res1 is on node1 , last change is Sync, oldest change is Sync")
	assertEquals(records[1], "extend/res1 is on node1 , last change is Sync, oldest change is Sync")
	assertEquals(records[2], "default/res2 is on node2 , last change is Updated, oldest change is Added")
}
```

### 其他

#### RingGrowing

一个可增长的环形buffer，作为读写速度不一致时的缓冲区。

## 运行机制

### 创建

### EventHandler

### Run

### Goroutine及函数调用链路


## 数据处理流程

// 图