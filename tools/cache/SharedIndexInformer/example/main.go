package main

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"reflect"

	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	useIndexer()
	useQueue()
	curUser, _ := user.Current()
	kubeConfigFile := curUser.HomeDir + "/.kube/config"
	config, _ := clientcmd.BuildConfigFromFlags("", kubeConfigFile)
	clientset, _ := kubernetes.NewForConfig(config)
	informer := createSharedIndexInformer(clientset)
	runSharedIndexInformer(informer)
	informer, factory := createSharedIndexInformerByFactory(clientset)
	runSharedIndexInformerByFactory(informer, factory)
}

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
	// 设计一个简单的处理逻辑：仅仅负责将资源信息打印出来
	// 真正业务中常是将资源事例放入一个workqueue中
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
	assertEquals(records[0], "default/res1 is on node1 , last change is Sync, oldest change is Sync")     // 按照顺序，default/res1是最先被添加到Queue的
	assertEquals(records[1], "extend/res1 is on node1 , last change is Sync, oldest change is Sync")      // 按照顺序，extend/res1是第二个被添加到Queue的
	assertEquals(records[2], "default/res2 is on node2 , last change is Updated, oldest change is Added") // default/res2最后加入，Deltas中存在它的两个版本
}

func createSharedIndexInformer(c *kubernetes.Clientset) cache.SharedIndexInformer {
	namespace := v1.NamespaceAll
	podListWatcher := cache.NewListWatchFromClient(c.CoreV1().RESTClient(), "pods", namespace, fields.Everything())
	// 也可以选择下面这种写法
	// podListWatcher = &cache.ListWatch{
	// 	ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
	// 		return c.CoreV1().Pods(namespace).List(context.TODO(), options)
	// 	},
	// 	WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
	// 		return c.CoreV1().Pods(namespace).Watch(context.TODO(), options)
	// 	},
	// }
	informer := cache.NewSharedIndexInformer(podListWatcher, &v1.Pod{}, 0, cache.Indexers{})
	// go informer.Run(make(chan struct{})) // informer.Run本身是阻塞的，所以一般另起一个goroutine；暂时先不启动
	return informer
}

func createSharedIndexInformerByFactory(c *kubernetes.Clientset) (cache.SharedIndexInformer, informers.SharedInformerFactory) {
	factory := informers.NewSharedInformerFactoryWithOptions(c, 0, informers.WithNamespace(v1.NamespaceAll))
	// factory.Start(make(chan struct{})) // factory.Start非阻塞；暂时先不启动
	return factory.Core().V1().Pods().Informer(), factory
}

// 注册事件通知句柄，可以注册多组
func withEventHandler(informer cache.SharedIndexInformer) {
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, _ := cache.MetaNamespaceKeyFunc(obj)
			fmt.Println("add a pod ", key)
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, _ := cache.MetaNamespaceKeyFunc(new)
			fmt.Println("update a pod ", key)
		},
		DeleteFunc: func(obj interface{}) {
			key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			fmt.Println("delete a pod ", key)
		},
	})
}

func runSharedIndexInformer(informer cache.SharedIndexInformer) {
	withEventHandler(informer)
	ctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	fmt.Println("### Start Informer")
	// 一般情况下，例如常驻的controller-manager中的controller会传入一个永不close的channel
	go informer.Run(ctx.Done())
	// 由于informer刚启动时会从apiserver拉取大量当前的资源实例状态，所以总是等待这些这些资源实例被处理完毕(EventHandler)之后，再进行具体的业务逻辑
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		panic("timed out waiting for caches to sync")
	}
	<-ctx.Done()
	fmt.Println("### End Informer")
}

func runSharedIndexInformerByFactory(informer cache.SharedIndexInformer, factory informers.SharedInformerFactory) {
	withEventHandler(informer)
	ctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	fmt.Println("### Start Informer By Factory")
	// 一般情况下，例如常驻的controller-manager中的controller会传入一个永不close的channel
	// factory.Start会调用注册在factory中的所有informer的Run方法
	factory.Start(ctx.Done())
	// 由于informer刚启动时会从apiserver拉取大量当前的资源实例状态，所以总是等待这些这些资源实例被处理完毕(EventHandler)之后，再进行具体的业务逻辑
	if !factory.WaitForCacheSync(ctx.Done())[reflect.TypeOf(&v1.Pod{})] {
		panic("timed out waiting for caches to sync")
	}
	<-ctx.Done()
	fmt.Println("### End Informer")
}
