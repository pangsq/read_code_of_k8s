# ControllerManager

branch: release-v1.18

commit: ff809a5d953ba778270ce8790b21d394821e1e28

## 源码入口

### 1) `cmd/kube-controller-manager/controller-manager.go`

一个非常简洁标准的命令模样的入口

```go
func main() {
	rand.Seed(time.Now().UnixNano())

	command := app.NewControllerManagerCommand()

	// TODO: once we switch everything over to Cobra commands, we can go back to calling
	// utilflag.InitFlags() (by removing its pflag.Parse() call). For now, we have to set the
	// normalize func and add the go flag set by hand.
	// utilflag.InitFlags()
	logs.InitLogs()
	defer logs.FlushLogs()

	if err := command.Execute(); err != nil {
		os.Exit(1)
	}
}
```

具体实现逻辑明显在`NewControllerManagerCommand()`中

### 2）`cmd/kube-contoller-manager/app/`

#### 2.1) NewContollerManagerCommand()，在controllermanager.go中

1. 构造ControllerManager命令；初始化配置
2. 接下来运行 Run(c.Complete(), wait.NeverStop)  // 见 2.3

```go
// NewControllerManagerCommand creates a *cobra.Command object with default parameters
func NewControllerManagerCommand() *cobra.Command {
	s, err := options.NewKubeControllerManagerOptions() // 详情见2.2
	...

	cmd := &cobra.Command{
		Use: "kube-controller-manager",
		Long: ...
		Run: func(cmd *cobra.Command, args []string) {
			...

			c, err := s.Config(KnownControllers(), // 所有支持的controllers名：sets.StringKeySet(NewControllerInitializers(IncludeCloudLoops)) 
			  ControllersDisabledByDefault.List()) // 缺省被关闭的控制器，现在有"bootstrapsigner", "tokencleaner"
			  // 此处方法内部只有一个Validate(allContollers, disabledByDefaultControllers)用到了这两入参
			  // 注意到Run作为一个函数还没有到调用的时刻，cmd带哪些flags还得依赖从 `fs := cmd.Flags`开始的代码
			...

			if err := Run(c.Complete(), wait.NeverStop); err != nil { // 核心入口
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags(KnownControllers(), ControllersDisabledByDefault.List()) 
	// 给命令行组装flags，传入这俩参数是为了描述controllers这个配置项的时候提示总共有哪些controller以及哪些是默认禁用的
	// controllers默认是"*" 表示启用所有默认开启的controllers，如果配置"foo"表示开启foo，"-foo"表示关闭foo，以,分隔
	... // 其他一些初始化cmd

	return cmd
}
```

#### 2.2) NewKubeControllerManagerOptions(),在options/options.go中

1. 创建一份kubeControllerManager的默认配置；追溯其源头可以到`pkg/controller/apis/config/v1alpha1/defaults.go`中的SetDefaults_KubeControllerManagerConfiguration 查看各个缺省值
2. 此处展开可以了解一些controllerManager中有哪些配置项以及其可能的默认配置

```go
// NewKubeControllerManagerOptions creates a new KubeControllerManagerOptions with a default config.
func NewKubeControllerManagerOptions() (*KubeControllerManagerOptions, error) {
	componentConfig, err := NewDefaultComponentConfig(ports.InsecureKubeControllerManagerPort) // 又套了一层方法去实现一些通用的默认配置的初始化
	if err != nil {
		return nil, err
	}

	s := KubeControllerManagerOptions{
		Generic:         cmoptions.NewGenericControllerManagerConfigurationOptions(&componentConfig.Generic),
		KubeCloudShared: cmoptions.NewKubeCloudSharedOptions(&componentConfig.KubeCloudShared),
		ServiceController: &cmoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &componentConfig.ServiceController,
		},
		... // 一堆controller的key-value对
		... // 一些认证方面的配置
	}

	... // authentication/authorization/secureServing等默认配置

    // gc资源配置就在此处了，一般我们很少配置gc相关的配置，所以通过这里可以知道k8s中一般有哪些资源可能是需要gc的
	gcIgnoredResources := make([]garbagecollectorconfig.GroupResource, 0, len(garbagecollector.DefaultIgnoredResources()))  // 忽略gc的资源，缺省的只有一个events
	for r := range garbagecollector.DefaultIgnoredResources() {
		gcIgnoredResources = append(gcIgnoredResources, garbagecollectorconfig.GroupResource{Group: r.Group, Resource: r.Resource})
	}

	s.GarbageCollectorController.GCIgnoredResources = gcIgnoredResources
	s.Generic.LeaderElection.ResourceName = "kube-controller-manager"
	s.Generic.LeaderElection.ResourceNamespace = "kube-system"

	return &s, nil
}
```

#### 2.3) Run(c *config.CompletedConfig, stopCh <-chan struct{}),回到controllermanager.go

1. 源码总计有130+行，算是controllerManager业务逻辑的主干了
2. 具体功能有：
    1. 打印version
    2. 向/configz这个handler中放入controllerManager目前的配置内容
    3. 将选主(leaderelection)情况放到healthCheck中
    4. 创建mux，加入/healthz、/debug/pprof、/configz、/metrics等handler
    5. 根据配置使用安全或非安全的模式，如果是安全模式，有Authorization和Authentication，之后启动http server
    6. 定义run：初始化clientBuilder为了后续能获取controller的config和client；先初始化saController再初始化其他controller；启动相关的informer
    7. 如果不需要选主则直接执行run，否则选主成功才执行run

```go
// Run runs the KubeControllerManagerOptions.  This should never exit.
func Run(c *config.CompletedConfig, stopCh <-chan struct{}) error {
	... // 打印Version

	if cfgz, err := configz.New(ConfigzName); err == nil {
		cfgz.Set(c.ComponentConfig) // configz是一个map[string]*Config{}，把kubeController的配置项注册进去，key为kubecontrollermanager.config.k8s.io
		// controller server会持有一个路径为`/configz`的handler
	} else {
		klog.Errorf("unable to register configz: %v", err)
	}

	// Setup any healthz checks we will want to use.
	// 为了HA 所以集群中可能有多个controllerManager，因此controllerManager通过apiserver/etcd选主；
	// 此处将选主情况做进healthCheck中
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
		checks = append(checks, electionChecker)
	}

	// Start the controller manager HTTP server
	// unsecuredMux is the handler for these controller *after* authn/authz filters have been applied
	var unsecuredMux *mux.PathRecorderMux
	if c.SecureServing != nil {
	    // 1. 创建mux，基础path设置成"controller-manager" 2. 给mux加入/healthz、/debug/pprof、/configz、/metrics等handler
		unsecuredMux = genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, checks...)
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh returned by c.SecureServing.Serve
		// 开启server
		if _, err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			return err
		}
	}
	if c.InsecureServing != nil {
		unsecuredMux = genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, checks...)
		insecureSuperuserAuthn := server.AuthenticationInfo{Authenticator: &server.InsecureSuperuser{}}
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, nil, &insecureSuperuserAuthn)
		if err := c.InsecureServing.Serve(handler, 0, stopCh); err != nil {
			return err
		}
	}

	run := func(ctx context.Context) {
		rootClientBuilder := controller.SimpleControllerClientBuilder{
			ClientConfig: c.Kubeconfig,
		}
		var clientBuilder controller.ControllerClientBuilder // 通过clientBuilder可以通过name获取controller的config和client
		if c.ComponentConfig.KubeCloudShared.UseServiceAccountCredentials {
			... // 在云环境中的clientBuilder
		} else {
			clientBuilder = rootClientBuilder
		}
		controllerContext, err := CreateControllerContext(c, rootClientBuilder, clientBuilder, ctx.Done())
		if err != nil {
			klog.Fatalf("error building controller context: %v", err)
		}
		// 由于serverAccountToken的特殊性，必须先于其他controller启动
		saTokenControllerInitFunc := serviceAccountTokenControllerStarter{rootClientBuilder: rootClientBuilder}.startServiceAccountTokenController

        // 启动所有controllers。StartControllers见 2.4，NewControllerInitializers 见2.5
		if err := StartControllers(controllerContext, saTokenControllerInitFunc, NewControllerInitializers(controllerContext.LoopMode), unsecuredMux); err != nil {
			klog.Fatalf("error starting controllers: %v", err)
		}
        // 用于controller的informers
		controllerContext.InformerFactory.Start(controllerContext.Stop)
		// 用于typed资源本身或动态资源的metadata的informers；目前用到这个informerFactory的有ResourceQuota和GarbageCollector
		controllerContext.ObjectOrMetadataInformerFactory.Start(controllerContext.Stop)
		close(controllerContext.InformersStarted)

		select {} // 阻塞当前线程
	}

    // 如果没有启用选主（集群中只有一个controllerManager的情况下），直接run
	if !c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		run(context.TODO()) // 这里定义了一个Done() 为 <-chan struct{}，在select中作为一个永远不会触发的条件
		panic("unreachable")
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	rl, err := resourcelock.New(c.ComponentConfig.Generic.LeaderElection.ResourceLock,
		c.ComponentConfig.Generic.LeaderElection.ResourceNamespace,
		c.ComponentConfig.Generic.LeaderElection.ResourceName,
		c.LeaderElectionClient.CoreV1(),
		c.LeaderElectionClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: c.EventRecorder,
		})
	if err != nil {
		klog.Fatalf("error creating lock: %v", err)
	}
    
    // 选主，成为主才执行run
	leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: c.ComponentConfig.Generic.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   c.ComponentConfig.Generic.LeaderElection.RetryPeriod.Duration,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				klog.Fatalf("leaderelection lost")
			},
		},
		WatchDog: electionChecker,
		Name:     "kube-controller-manager",
	})
	panic("unreachable")
}
```

#### 2.4) StartControllers,在controolermanager.go中

1. 通过传入的ctx和initFunc启动saTokenController
2. 通过传入的ctx和controllers，调用每个controller的initFunc分别对其进行初始化
3. 往mux中添加一些debug的handler

```go
// StartControllers starts a set of controllers with a specified ControllerContext
func StartControllers(ctx ControllerContext, startSATokenController InitFunc, controllers map[string]InitFunc, unsecuredMux *mux.PathRecorderMux) error {
	// Always start the SA token controller first using a full-power client, since it needs to mint tokens for the rest
	// If this fails, just return here and fail since other controllers won't be able to get credentials.
	if _, _, err := startSATokenController(ctx); err != nil {
		return err
	}

	// Initialize the cloud provider with a reference to the clientBuilder only after token controller
	// has started in case the cloud provider uses the client builder.
	if ctx.Cloud != nil {
		ctx.Cloud.Initialize(ctx.ClientBuilder, ctx.Stop)
	}

	for controllerName, initFn := range controllers {
		if !ctx.IsControllerEnabled(controllerName) {
			klog.Warningf("%q is disabled", controllerName)
			continue
		}

		time.Sleep(wait.Jitter(ctx.ComponentConfig.Generic.ControllerStartInterval.Duration, ControllerStartJitter))

		klog.V(1).Infof("Starting %q", controllerName)
		debugHandler, started, err := initFn(ctx)
		if err != nil {
			klog.Errorf("Error starting %q", controllerName)
			return err
		}
		if !started {
			klog.Warningf("Skipping %q", controllerName)
			continue
		}
		if debugHandler != nil && unsecuredMux != nil {
			basePath := "/debug/controllers/" + controllerName
			unsecuredMux.UnlistedHandle(basePath, http.StripPrefix(basePath, debugHandler))
			unsecuredMux.UnlistedHandlePrefix(basePath+"/", http.StripPrefix(basePath, debugHandler))
		}
		klog.Infof("Started %q", controllerName)
	}

	return nil
}
```

#### 2.5) NewControllerInitializers,在controllermanager.go中

此处定义了每个controller的初始化方法，其内容大同小异，所以2.6选取比较简单的namespace进行分析

```go
// NewControllerInitializers is a public map of named controller groups (you can start more than one in an init func)
// paired to their InitFunc.  This allows for structured downstream composition and subdivision.
func NewControllerInitializers(loopMode ControllerLoopMode) map[string]InitFunc {
	controllers := map[string]InitFunc{}
	controllers["endpoint"] = startEndpointController
	controllers["endpointslice"] = startEndpointSliceController
	controllers["replicationcontroller"] = startReplicationController
	controllers["podgc"] = startPodGCController
	controllers["resourcequota"] = startResourceQuotaController
	controllers["namespace"] = startNamespaceController
	controllers["serviceaccount"] = startServiceAccountController
	controllers["garbagecollector"] = startGarbageCollectorController
	controllers["daemonset"] = startDaemonSetController
	controllers["job"] = startJobController
	controllers["deployment"] = startDeploymentController
	controllers["replicaset"] = startReplicaSetController
	controllers["horizontalpodautoscaling"] = startHPAController
	controllers["disruption"] = startDisruptionController
	controllers["statefulset"] = startStatefulSetController
	controllers["cronjob"] = startCronJobController
	controllers["csrsigning"] = startCSRSigningController
	controllers["csrapproving"] = startCSRApprovingController
	controllers["csrcleaner"] = startCSRCleanerController
	controllers["ttl"] = startTTLController
	controllers["bootstrapsigner"] = startBootstrapSignerController
	controllers["tokencleaner"] = startTokenCleanerController
	controllers["nodeipam"] = startNodeIpamController
	controllers["nodelifecycle"] = startNodeLifecycleController
	if loopMode == IncludeCloudLoops {
		controllers["service"] = startServiceController
		controllers["route"] = startRouteController
		controllers["cloud-node-lifecycle"] = startCloudNodeLifecycleController
		// TODO: volume controller into the IncludeCloudLoops only set.
	}
	controllers["persistentvolume-binder"] = startPersistentVolumeBinderController
	controllers["attachdetach"] = startAttachDetachController
	controllers["persistentvolume-expander"] = startVolumeExpandController
	controllers["clusterrole-aggregation"] = startClusterRoleAggregrationController
	controllers["pvc-protection"] = startPVCProtectionController
	controllers["pv-protection"] = startPVProtectionController
	controllers["ttl-after-finished"] = startTTLAfterFinishedController
	controllers["root-ca-cert-publisher"] = startRootCACertPublisher

	return controllers
}
```

#### 2.6) startNamespaceController,在core.go中

1. startNamespaceController初始化了用于namespace-controller的client(就是一个访问apiserver的kube-client实例，设置了UserAgent)，然后调用startModifiedNamespaceController
2. startModifiedNamespaceController中初始化了真正的namespace控制器，然后起了一个goroutine去管理namespace
3. 启用多个goroutine（缺省为10个），每个goroutine中一个worker处理具体逻辑
4. worker中最外围是一个loop，循环往复地执行处理逻辑
    1. 尝试从队列中获取一个key
    2. 根据这个key尝试同步namespace信息:syncNamespaceFromKey；由于namespace的操作只有delete（在enqueueNamespace这个方法中删除了除delete namespace外的其他操作）了，所以此处的同步即从apiserver中删除key对应的namespace，具体代码在`func (d *namespacedResourcesDeleter) deleteAllContent(ns *v1.Namespace) (int64, error)`
    3. 如果成功，从队列中删除这个key并返回
    4. 如果不成功，判断是什么类型的错误
        1. 如果是deletion.ResourcesRemainingError（namespace下还有其他资源），则在一段时间后加入队列
        2. 如果不是，则直接将key重新加入队列

```go
func startNamespaceController(ctx ControllerContext) (http.Handler, bool, error) {
	// the namespace cleanup controller is very chatty.  It makes lots of discovery calls and then it makes lots of delete calls
	// the ratelimiter negatively affects its speed.  Deleting 100 total items in a namespace (that's only a few of each resource
	// including events), takes ~10 seconds by default.
	nsKubeconfig := ctx.ClientBuilder.ConfigOrDie("namespace-controller")
	nsKubeconfig.QPS *= 20
	nsKubeconfig.Burst *= 100
	namespaceKubeClient := clientset.NewForConfigOrDie(nsKubeconfig)
	return startModifiedNamespaceController(ctx, namespaceKubeClient, nsKubeconfig)
}

func startModifiedNamespaceController(ctx ControllerContext, namespaceKubeClient clientset.Interface, nsKubeconfig *restclient.Config) (http.Handler, bool, error) {

	metadataClient, err := metadata.NewForConfig(nsKubeconfig)
	if err != nil {
		return nil, true, err
	}

	discoverResourcesFn := namespaceKubeClient.Discovery().ServerPreferredNamespacedResources

	namespaceController := namespacecontroller.NewNamespaceController(
		namespaceKubeClient,
		metadataClient,
		discoverResourcesFn,
		ctx.InformerFactory.Core().V1().Namespaces(),
		ctx.ComponentConfig.NamespaceController.NamespaceSyncPeriod.Duration,
		v1.FinalizerKubernetes,
	)
	go namespaceController.Run(int(ctx.ComponentConfig.NamespaceController.ConcurrentNamespaceSyncs), ctx.Stop)

	return nil, true, nil
}
```

来到`namespace_controller.go`

```go
// Run starts observing the system with the specified number of workers.
func (nm *NamespaceController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer nm.queue.ShutDown()

	klog.Infof("Starting namespace controller")
	defer klog.Infof("Shutting down namespace controller")

	if !cache.WaitForNamedCacheSync("namespace", stopCh, nm.listerSynced) {
		return
	}

	klog.V(5).Info("Starting workers of namespace controller")
	for i := 0; i < workers; i++ {
		go wait.Until(nm.worker, time.Second, stopCh)
	}
	<-stopCh
}

// worker processes the queue of namespace objects.
// Each namespace can be in the queue at most once.
// The system ensures that no two workers can process
// the same namespace at the same time.
func (nm *NamespaceController) worker() {
	workFunc := func() bool {
		key, quit := nm.queue.Get()
		if quit {
			return true
		}
		defer nm.queue.Done(key)

		err := nm.syncNamespaceFromKey(key.(string))
		if err == nil {
			// no error, forget this entry and return
			nm.queue.Forget(key)
			return false
		}

		if estimate, ok := err.(*deletion.ResourcesRemainingError); ok {
			t := estimate.Estimate/2 + 1
			klog.V(4).Infof("Content remaining in namespace %s, waiting %d seconds", key, t)
			nm.queue.AddAfter(key, time.Duration(t)*time.Second)
		} else {
			// rather than wait for a full resync, re-add the namespace to the queue to be processed
			nm.queue.AddRateLimited(key)
			utilruntime.HandleError(fmt.Errorf("deletion of namespace %v failed: %v", key, err))
		}
		return false
	}

	for {
		quit := workFunc()

		if quit {
			return
		}
	}
}
```

#### 2.7) 参见client-go的例子

官方提供的一个client使用workqueue的例子很好地描述了controller的大致实现，可以作为一个非常标准的模板用于参考。

`https://github.com/kubernetes/client-go/blob/master/examples/workqueue/main.go`

