# SharedIndexInformer

SharedIndexInformer是k8s的client-go中一个功能强大的工具，通过它可以在client端缓存apiserver中的资源实例状态，以提高对整集群管理、调谐的效率。无论在controller-manager/scheduler等原生组件中还是用kubebuilder编写crd的controller中都会用到它，因此对它内部的实现进行了解是非常必要的。

SharedIndexInformer的实现大致可以表述成：利用ListWatch机制不断从apiserver处获取某一类型的资源实例，存入本地内存中的一个Indexer（带索引和其他丰富接口的缓存），并辅以一个Queue（按照先后顺序）对外进行通知（资源实例的变化——增删改 到来时触发用户事先自定义好的事件句柄）。

一般情况下，触发的事件句柄会把资源变化事件放入一个workQueue，另有线程（可以有多个并行）从中拾取事件进行处理（基于本类型或其他类型资源的indexer作出正确的决策）。当然这是后话，此文暂不具体展开。

下文以三个层次对SharedIndexInformer进行分析，分别是：
1. 静态：数据结构，出于利用索引提供读性能、顺序访问、线程安全、缓冲、阻塞等待等目的而实现，如Indexer、Queue、RingGrowing等
2. 动态：运行机制，创建、初始化、运行、运行时的各种goroutine被带起的时机
3. 动静结合：完整的数据处理流程，从访问apiserver获取资源到分发、处理、存储资源的整个流程


## 数据结构


1. Store
2. Indexer
3. Queue
// Indexer使用例子 合并同一个实例，提供线程安全，阻塞（同步的方式更易于减轻心智负担）
// Queue 合并同一个实例，维护先后顺序，提供线程安全，阻塞
// RingGrowing 提供缓冲

## 运行机制

// 创建 New 或者 factory
// 注册eventHandler
// Run factory的run或者直接run
// 表


## 数据处理流程

// 流程图或WPS制图