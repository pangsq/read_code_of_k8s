package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/cache"
)

func main() {
	useIndexer()
	useQueue()
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
