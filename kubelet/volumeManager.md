# volumeManager

volumeManager是kubelet中对volumes进行attached/mounted/unmounted/detached等操作的执行者。

代码版本：release-1.11   commit:901674

## volumeManager

### 调用

```golang
1. Run中启动volumeManager独立的协程
   go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop))
2. syncPod中阻塞等待Pod的attach和mount完成
   if !kl.podIsTerminated(pod) 
       if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
           // record event和log error
           return err
       }
   }
```

### 结构体

```golang
type volumeManager struct {
   // kube client，由DesiredStateOfWorldPopulator调用从API server获取PV和PVC
   kubeClient clientset.Interface

   // 用于访问各种不同volume插件
   volumePluginMgr *volume.VolumePluginMgr

   // 记录volumes的目标状态，如哪些volumes需要被attached，哪些pod引用volumes。
   // kubelet pod manager将会用到这个信息
   // 简称dsw
   desiredStateOfWorld cache.DesiredStateOfWorld

   // 记录volumes的当前状态，与desiredStateOfWorld相对应
   // kubelet pod manager同样会用到这个信息
   // 简称asw
   actualStateOfWorld cache.ActualStateOfWorld

   // 生成异步的attach/detach/mount/unmount操作
   operationExecutor operationexecutor.OperationExecutor
   
   // 调整actualStateOfWorld
   // 调谐器，通过operationExecutor的attach/detach/mount/unmount操作对volumes进行异步的周期性的调谐
   // 从 actualStateOfWorld 到 desiredStateOfWorld
   reconciler reconciler.Reconciler
   
   // 维护desiredStateOfWorld
   // 调用podManager异步地周期性地生成desiredStateOfWorld
   desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator
}
```

### 接口

```golang
type VolumeManager interface {
   // 启动volume manager，触发所有异步的loops
   Run(sourcesReady config.SourcesReady, stopCh <-chan struct{})

   // 同步阻塞地等待volumes被attached和mounted（从actualStateOfWorld中获取状态）
   // 等待时间超过podAttachAndMountTimeout时返回错误
   WaitForAttachAndMount(pod *v1.Pod) error

   // 返回Pod的已成功attached和mounted的volumes
   GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap

   // pv的annations中可定义pv.beta.kubernetes.io/gid以指定额外的user group id
   // 当pod.Spec.SecurityContext.SupplementalGroups也包含该gid时，该gid就算作pod的extraSupplementalGroups
   // kuberuntime在创建容器时将会用到这个信息，将用户组添加到容器中；与fsGroup搭配使用，fsGroup指定挂载volume的用户组
   GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64

   // 获取正在使用的volumes。"正在使用"的标准是volume在desiredStateOfWorld中（即当volume被attached前就算作in use），直到desiredStateOfWorld和actualStateOfWorld中不再有这个volume或者被unmounted
   GetVolumesInUse() []v1.UniqueVolumeName

   // kubelet启动后初始的actual states被全部同步完成
   ReconcilerStatesHasBeenSynced() bool

   // volume是否被attached
   VolumeIsAttached(volumeName v1.UniqueVolumeName) bool

   // 将dsw记录的volumes标记为in use
   MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName)
}
```

### 运行流程

volumeManager的启动会带起desiredStateOfWorldPopulator.Run和reconciler.Run两个协程

```golang
// Run

// kubelet的配置来源有三种：file/http/apiserver；传入的sourcesReady可以用于判断三种来源是否都已ready
go vm.desiredStateOfWorldPopulator.Run(sourcesReady, stopCh)
go vm.reconciler.Run(stopCh)
```

## desiredStateOfWorld

```golang
type desiredStateOfWorld struct {
   // dsw核心就是此map，用于记录每个volume被哪些pod使用
   volumesToMount map[v1.UniqueVolumeName]volumeToMount
   // 维护已知的volumePlugin。用于区分volume是否是attachable的，影响UniqueVolumeName的生成
   // attachable的volumeName格式为{pluginName}/{volumePlugin.GetVolumeName(volumeName)}，而非attachable的volumeName格式为{pluginName}/{podName}-{volumeSpec.Name}
   // 例如对于csiPlugin来说，最终的volumeName为kubernetes.io/csi/{csi.Driver}^{csi.VolumeHandle}
   volumePluginMgr *volume.VolumePluginMgr
   sync.RWMutex
}

type volumeToMount struct {
   // volumeName
   volumeName v1.UniqueVolumeName
   // 使用该volume的pods
   podsToMount map[types.UniquePodName]podToMount
   // volume是否是attachable的
   pluginIsAttachable bool
   // volume的gid，从annotation中获取
   volumeGidValue string
   // node.Status.VolumesInUse是否已包含本volume
   reportedInUse bool
}
```

## actualStateOfWorld

```golang
type actualStateOfWorld struct {
   // 节点名
   nodeName types.NodeName
   // 记录attached的volume
   attachedVolumes map[v1.UniqueVolumeName]attachedVolume
   // 维护所有volumePlugin
   volumePluginMgr *volume.VolumePluginMgr
   
   sync.RWMutex
}

type attachedVolume struct {
   // volumeName
   volumeName v1.UniqueVolumeName
   // 已成功mount的pod
   mountedPods map[volumetypes.UniquePodName]mountedPod
   // volume spec，unmount会用到以下的路径
   // /var/lib/kubelet/pods/{podUID}/volumes/{escapeQualifiedPluginName}/{volumeSpecName}/
   spec *volume.Spec
   // pluginName
   pluginName string
   // pluginIsAttachable
   pluginIsAttachable bool
   // volume是否被挂载在一个公共的mount path目录底下
   globallyMounted bool
   // volume attach的路径
   devicePath string
   // 块设备被挂载的路径
   deviceMountPath string
}
```


## desiredStateOfWorldPopulator

```golang
func (dswp *desiredStateOfWorldPopulator) Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) {
   // 周期性地执行populaterLoopFunc;hasAddedPods状态信息是reconciler将会用到
   // 而对于populaterLoopFunc本身，无论sources是否AllReady都不影响运行
   wait.PollUntil(dswp.loopSleepDuration, func() (bool, error) {
      done := sourcesReady.AllReady()
      dswp.populatorLoopFunc()()
      return done, nil
   }, stopCh)
   dswp.hasAddedPodsLock.Lock()
   dswp.hasAddedPods = true
   dswp.hasAddedPodsLock.Unlock()
   wait.Until(dswp.populatorLoopFunc(), dswp.loopSleepDuration, stopCh)
}

func (dswp *desiredStateOfWorldPopulator) populatorLoopFunc() func() {
   return func() {
      dswp.findAndAddNewPods()

      // findAndRemoveDeletedPods()是一个非常耗时的操作，所以通过设置调用间隔以限制调用速率
      if time.Since(dswp.timeOfLastGetPodStatus) < dswp.getPodStatusRetryDuration {
         return
      }
      dswp.findAndRemoveDeletedPods()
   }
}

func (dswp *desiredStateOfWorldPopulator) findAndAddNewPods() {
   // Map unique pod name to outer volume name to MountedVolume.
   mountedVolumesForPod := make(map[volumetypes.UniquePodName]map[string]cache.MountedVolume)
   // 如果开启了实时扩容功能，那么正在被使用中的pod volume，其容量会发生变化，所以先将asw中的moutedVolumes记录下来，具体用处下文分析
   if utilfeature.DefaultFeatureGate.Enabled(features.ExpandInUsePersistentVolumes) {
      for _, mountedVolume := range dswp.actualStateOfWorld.GetMountedVolumes() {
         mountedVolumes, exist := mountedVolumesForPod[mountedVolume.PodName]
         if !exist {
            mountedVolumes = make(map[string]cache.MountedVolume)
            mountedVolumesForPod[mountedVolume.PodName] = mountedVolumes
         }
         mountedVolumes[mountedVolume.OuterVolumeSpecName] = mountedVolume
      }
   }
   // 记录有resize过的volumes，因为一个volume的resize只要执行一次即可
   // 如果多个pod都使用同一个volume，为了避免多次执行，所以记录下volume
   processedVolumesForFSResize := sets.NewString()
   // kubelet的多个组件都会用到podManager，podManager维护节点上的pods
   for _, pod := range dswp.podManager.GetPods() {
      if dswp.isPodTerminated(pod) {
         // Do not (re)add volumes for terminated pods
         continue
      }
      dswp.processPodVolumes(pod, mountedVolumesForPod, processedVolumesForFSResize)
   }
}

func (dswp *desiredStateOfWorldPopulator) processPodVolumes(
   pod *v1.Pod,
   mountedVolumesForPod map[volumetypes.UniquePodName]map[string]cache.MountedVolume,
   processedVolumesForFSResize sets.String) {
   if pod == nil {
      return
   }

   uniquePodName := util.GetUniquePodName(pod)
   // 已经处理过的pod不再处理；已处理过的pod会记录在dswp.pods.processedPods中
   if dswp.podPreviouslyProcessed(uniquePodName) {
      return
   }

   allVolumesAdded := true
   // 生成两个volumeMap，分别对应普通的挂载卷和块设备
   mountsMap, devicesMap := dswp.makeVolumeMap(pod.Spec.Containers)

   // Process volume spec for each volume defined in pod
   for _, podVolume := range pod.Spec.Volumes {
       // 如果podVolume是pvc类型的，则根据pvc名字找到与其bound的pv，获取pv信息
      pvc, volumeSpec, volumeGidValue, err :=
         dswp.createVolumeSpec(podVolume, pod.Name, pod.Namespace, mountsMap, devicesMap)
      // error handle

      // 将volume信息记录在dsw中
      _, err = dswp.desiredStateOfWorld.AddPodToVolume(
         uniquePodName, pod, volumeSpec, podVolume.Name, volumeGidValue)
      // error handle
      
      // 如果开启volume扩容功能，那么会通过pvc.Status.Capacity[v1.ResourceStorage]和pvc.Spec.Capacity[v1.ResourceStorage]进行比较，
      // 来判断是否执行dswp.actualStateOfWorld.MarkFSResizeRequired
      if utilfeature.DefaultFeatureGate.Enabled(features.ExpandInUsePersistentVolumes) {
         dswp.checkVolumeFSResize(pod, podVolume, pvc, volumeSpec,
            uniquePodName, mountedVolumesForPod, processedVolumesForFSResize)
      }
   }

   // some of the volume additions may have failed, should not mark this pod as fully processed
   if allVolumesAdded {
       // 记录过已处理过的pod，即放入dswp.pods.processedPods中
      dswp.markPodProcessed(uniquePodName)
      // 当所有pod的所有volume添加后，个别类型volume可能需要remount，例如DownwardAPI
      dswp.actualStateOfWorld.MarkRemountRequired(uniquePodName)
   }

}

func (dswp *desiredStateOfWorldPopulator) findAndRemoveDeletedPods() {
   var runningPods []*kubecontainer.Pod

   runningPodsFetched := false
   // 从dsw中获取所有volumes，
   for _, volumeToMount := range dswp.desiredStateOfWorld.GetVolumesToMount() {
      pod, podExists := dswp.podManager.GetPodByUID(volumeToMount.Pod.UID)
      if podExists {
         // Skip running pods
         if !dswp.isPodTerminated(pod) {
            continue
         }
         if dswp.keepTerminatedPodVolumes {
            continue
         }
      }

      // 一次findAndRemoveDeletedPods中只执行一次dswp.kubeContainerRuntime.GetPods
      if !runningPodsFetched {
         var getPodsErr error
         runningPods, getPodsErr = dswp.kubeContainerRuntime.GetPods(false)
         // handle error

         runningPodsFetched = true
         dswp.timeOfLastGetPodStatus = time.Now()
      }

      runningContainers := false
      // 检查volume对应的pod是否存在，且其包含的container是否存在
      for _, runningPod := range runningPods {
         if runningPod.ID == volumeToMount.Pod.UID {
            if len(runningPod.Containers) > 0 {
               runningContainers = true
            }

            break
         }
      }

      if runningContainers {
        // 如果相关的（即使用了该volume的pod包含的）containers还存在，则暂时不从volumeManager中将其移除
         continue
      }

      if !dswp.actualStateOfWorld.VolumeExists(volumeToMount.VolumeName) && podExists {
         // 如果asw中不包含该volume了，且pod还存在
         continue
      }

      // dsw记录了volume和pod信息，即volume被使用在哪些pod中
      // 此方法在volume下移除pod信息，如果一个volume不被任何pod使用，则该volume也需要被删除
      dswp.desiredStateOfWorld.DeletePodFromVolume(
         volumeToMount.PodName, volumeToMount.VolumeName)
         //从dswp.pods.processedPods中移除
      dswp.deleteProcessedPod(volumeToMount.PodName)
   }
}
```

总结下：
```
1. 输入信息：
a) 处理的对象是podManager.GetPods()
b) actualStateOfWorld.GetMountedVolumes()，获取asw中已挂载的volume，用于resize处理
c) desiredStateOfWorld.GetVolumesToMount()，获取dsw中记录的volumeMount，即volume被哪个pod挂载信息，以便之后处理pod已不存在的情况下删除volumeToMount信息
d) podManager.GetPodByUID(...)，用于检查pod是否还存在

2. 输出操作：对新增的pod执行abc,对删除的pod执行d
a) 通过desiredStateOfWorld.AddPodToVolume(...)将pod的volume信息记录在dsw中（每个pod只处理一次)
b) 调用actualStateOfWorld.MarkFSResizeRequired(...)将需要扩容调整的volume做标记
c) 调用actualStateOfWorld.MarkRemountRequired(...)对需要remount的volume进行remount
d) 调用desiredStateOfWorld.DeletePodFromVolume(...)将pod的volume信息从dsw中删除
```

## reconciler

```golang
func (rc *reconciler) Run(stopCh <-chan struct{}) {
   wait.Until(rc.reconciliationLoopFunc(), rc.loopSleepDuration, stopCh)
}

func (rc *reconciler) reconciliationLoopFunc() func() {
   return func() {
       // 主要调谐方法，接下来重点分析
       // 按照顺序，对有需要的volume进行1) unmounted 2) attached/mounted 3) detached/unmounted
      rc.reconcile()

      if rc.populatorHasAddedPods() && !rc.StatesHasBeenSynced() {
         glog.Infof("Reconciler: start to sync state")
         // 通过遍历所有pods的volume目录，观察到当前节点上的真实情况，与dsw和asw做比较，已处理不一致的情况(kubelet重启时会遗留mounted volumes)
         rc.sync()
      }
   }
}

func (rc *reconciler) reconcile() {

   // 确保应该unmounted的volumes是unmounted的
   for _, mountedVolume := range rc.actualStateOfWorld.GetMountedVolumes() {
      if !rc.desiredStateOfWorld.PodExistsInVolume(mountedVolume.PodName, mountedVolume.VolumeName) {
         err := rc.operationExecutor.UnmountVolume(
            mountedVolume.MountedVolume, rc.actualStateOfWorld, rc.kubeletPodsDir)
         // handle error
      }
   }

   // 确保应该attached/mounted的volumes是attached的
   for _, volumeToMount := range rc.desiredStateOfWorld.GetVolumesToMount() {
      volMounted, devicePath, err := rc.actualStateOfWorld.PodExistsInVolume(volumeToMount.PodName, volumeToMount.VolumeName)
      volumeToMount.DevicePath = devicePath
      if cache.IsVolumeNotAttachedError(err) {
         if rc.controllerAttachDetachEnabled || !volumeToMount.PluginIsAttachable {
            // 如果controllerAttachDetachEnabled(即由controller-manager做attach操作)或volumePlugin不是attachable的，则等待volume被attach
            err := rc.operationExecutor.VerifyControllerAttachedVolume(
               volumeToMount.VolumeToMount,
               rc.nodeName,
               rc.actualStateOfWorld)
            // handle error
         } else {
            // 在kubelet内部对其attach
            volumeToAttach := operationexecutor.VolumeToAttach{
               VolumeName: volumeToMount.VolumeName,
               VolumeSpec: volumeToMount.VolumeSpec,
               NodeName:   rc.nodeName,
            }

            err := rc.operationExecutor.AttachVolume(volumeToAttach, rc.actualStateOfWorld)
            // handle error
         }
      } else if !volMounted || cache.IsRemountRequiredError(err) {
         // Volume is not mounted, or is already mounted, but requires remounting
         remountingLogStr := ""
         isRemount := cache.IsRemountRequiredError(err)
         err := rc.operationExecutor.MountVolume(
            rc.waitForAttachTimeout,
            volumeToMount.VolumeToMount,
            rc.actualStateOfWorld,
            isRemount)
         // handle error
      } else if cache.IsFSResizeRequiredError(err) &&
         utilfeature.DefaultFeatureGate.Enabled(features.ExpandInUsePersistentVolumes) {
         glog.V(4).Infof(volumeToMount.GenerateMsgDetailed("Starting operationExecutor.ExpandVolumeFSWithoutUnmounting", ""))
         err := rc.operationExecutor.ExpandVolumeFSWithoutUnmounting(
            volumeToMount.VolumeToMount,
            rc.actualStateOfWorld)
         // handle error
      }
   }

   // 确保detached/unmounted的volume是detached/unmounted的
   for _, attachedVolume := range rc.actualStateOfWorld.GetUnmountedVolumes() {
      if !rc.desiredStateOfWorld.VolumeExists(attachedVolume.VolumeName) &&
         !rc.operationExecutor.IsOperationPending(attachedVolume.VolumeName, nestedpendingoperations.EmptyUniquePodName) {
         if attachedVolume.GloballyMounted {
            err := rc.operationExecutor.UnmountDevice(
               attachedVolume.AttachedVolume, rc.actualStateOfWorld, rc.mounter)
            // handle error
         } else {
            // 由controller-manager进行detach或者不需要dettach
            if rc.controllerAttachDetachEnabled || !attachedVolume.PluginIsAttachable {
                // 直接从asw.attachedVolumes中删除
               rc.actualStateOfWorld.MarkVolumeAsDetached(attachedVolume.VolumeName, attachedVolume.NodeName)
            } else {
               // 由kubelet内部进行detach
               err := rc.operationExecutor.DetachVolume(
                  attachedVolume.AttachedVolume, false /* verifySafeToDetach */, rc.actualStateOfWorld)
               // handle error
            }
         }
      }
   }
}

func (rc *reconciler) sync() {
   defer rc.updateLastSyncTime()
   rc.syncStates()
}

func (rc *reconciler) syncStates() {
   // 遍历podsDir获取真实的volume挂载情况
   podVolumes, err := getVolumesFromPodDir(rc.kubeletPodsDir)
   // handle error
   volumesNeedUpdate := make(map[v1.UniqueVolumeName]*reconstructedVolume)
   volumeNeedReport := []v1.UniqueVolumeName{}
   for _, volume := range podVolumes {
      if rc.actualStateOfWorld.VolumeExistsWithSpecName(volume.podName, volume.volumeSpecName) {
         // 与asw一致则不做任何处理
         continue
      }
      volumeInDSW := rc.desiredStateOfWorld.VolumeExistsWithSpecName(volume.podName, volume.volumeSpecName)
      // 重建volume，调用rc.operationExecutor.ReconstructVolumeOperation
      reconstructedVolume, err := rc.reconstructVolume(volume)
      // handle error
      if volumeInDSW {
         // 如果在dsw中包含该volume，则需要将该volume上报为in use
         volumeNeedReport = append(volumeNeedReport, reconstructedVolume.volumeName)
         continue
      }
      // 更新volume
      volumesNeedUpdate[reconstructedVolume.volumeName] = reconstructedVolume
   }

   if len(volumesNeedUpdate) > 0 {
       // 更新volumes状态。从node.Status.VolumesAttached中获取devicePath，并调用actualStateOfWorld.MarkVolumeAsAttached/actualStateOfWorld.MarkVolumeAsMounted/actualStateOfWorld.MarkDeviceAsMounted
      if err = rc.updateStates(volumesNeedUpdate); err != nil {
         glog.Errorf("Error occurred during reconstruct volume from disk: %v", err)
      }
   }
   if len(volumeNeedReport) > 0 {
       // 将volume标记为in use
      rc.desiredStateOfWorld.MarkVolumesReportedInUse(volumeNeedReport)
   }
}
```

总结：
```
1. reconcile进行如下调谐
a) 对asw中存在但dsw中不存在的volume，调用operationExecutor.UnmountVolume
b) 对dsw中存在的volume，如果asw表明该volume未attach则调用operationExecutor.AttachVolume，
    如果asw表明该volume为unmounted或者remountRequired则调用operationExecutor.MountVolume，
    如果asw表明该volume为resizeRequired则调用operationExecutor.ExpandVolumeFSWithoutUnmounting
c) 对asw中的unmountedVolumes(即没有pod使用的volume)进行清理，调用operationExecutor.UnmountDevice或MarkVolumeAsDetached或operationExecutor.DetachVolume

2. sync进行如下调谐
a) 如果真实的mount信息与asw不符，则调用operationExecutor.ReconstructVolumeOperation根据遍历podsDir得到的信息对podVolume进行重建
b) 将确实的volume信息加入到asw中，并标记dsw中相应的volume为in use
```

## operationExecutor

只记下最为关键的MountVolume操作

```golang
func (oe *operationExecutor) MountVolume(
   waitForAttachTimeout time.Duration,
   volumeToMount VolumeToMount,
   actualStateOfWorld ActualStateOfWorldMounterUpdater,
   isRemount bool) error {
   // ...
   if fsVolume {
      // filesystem情况
      generatedOperations, err = oe.operationGenerator.GenerateMountVolumeFunc(
         waitForAttachTimeout, volumeToMount, actualStateOfWorld, isRemount)

   } else {
      // 块设备情况
      generatedOperations, err = oe.operationGenerator.GenerateMapVolumeFunc(
         waitForAttachTimeout, volumeToMount, actualStateOfWorld)
   }
   // 执行mount
   return oe.pendingOperations.Run(
      volumeToMount.VolumeName, podName, generatedOperations)
}

func (og *operationGenerator) GenerateMountVolumeFunc(
   waitForAttachTimeout time.Duration,
   volumeToMount VolumeToMount,
   actualStateOfWorld ActualStateOfWorldMounterUpdater,
   isRemount bool) (volumetypes.GeneratedOperations, error) {
   // Get mounter plugin
   volumePlugin, err :=
      og.volumePluginMgr.FindPluginBySpec(volumeToMount.VolumeSpec)
   // handle error

   // 检查nodeAffinity是否匹配
   affinityErr := checkNodeAffinity(og, volumeToMount, volumePlugin)
   // handle error
   
   // 调用volume插件的NewMounter生成具体的mounter工具
   volumeMounter, newMounterErr := volumePlugin.NewMounter(
      volumeToMount.VolumeSpec,
      volumeToMount.Pod,
      volume.VolumeOptions{})
   // handle error
   // 检查mount配置项是否支持
   mountCheckError := checkMountOptionSupport(og, volumeToMount, volumePlugin)

   // handl error

   // 获取attacher
   attachableVolumePlugin, _ :=
      og.volumePluginMgr.FindAttachablePluginBySpec(volumeToMount.VolumeSpec)
   var volumeAttacher volume.Attacher
   if attachableVolumePlugin != nil {
      volumeAttacher, _ = attachableVolumePlugin.NewAttacher()
   }
   // 挂载路径用户组
   var fsGroup *int64
   if volumeToMount.Pod.Spec.SecurityContext != nil &&
      volumeToMount.Pod.Spec.SecurityContext.FSGroup != nil {
      fsGroup = volumeToMount.Pod.Spec.SecurityContext.FSGroup
   }
   // 具体的挂载方法
   mountVolumeFunc := func() (error, error) {
      if volumeAttacher != nil {
         // 调用volumeAttacher，等待volume被attach
         // 对于csi来说，controller-manager的attachDetachController会调用csi的attacher生成volumeAttachment和更新ode.Status.VolumesAttached状态，然后由external-attacher进行attach操作并修改volumeAttachment
         // 之后由kubelet在此处等待attach（即监听volumeAttachment）。v1.11版本中此处存在bug，某些情况下kubelet会尝试获取错误的volumeAttachment，是由于其将node.Status.VolumesAttached中的devicePath作为volumeAttachment名了
         devicePath, err := volumeAttacher.WaitForAttach(
            volumeToMount.VolumeSpec, volumeToMount.DevicePath, volumeToMount.Pod, waitForAttachTimeout)
         // handle error

         // 将
         deviceMountPath, err :=
            volumeAttacher.GetDeviceMountPath(volumeToMount.VolumeSpec)

         // Mount device to global mount path
         err = volumeAttacher.MountDevice(
            volumeToMount.VolumeSpec,
            devicePath,
            deviceMountPath)
         // handle error

         // 记录device已mount
         markDeviceMountedErr := actualStateOfWorld.MarkDeviceAsMounted(
            volumeToMount.VolumeName, devicePath, deviceMountPath)
            // handle error

         // resize挂载卷
         resizeSimpleError, resizeDetailedError := og.resizeFileSystem(volumeToMount, devicePath, deviceMountPath, volumePlugin.GetPluginName())
         // handle error
      }

      if og.checkNodeCapabilitiesBeforeMount {
         // 检查是否可以mount
         // ...
      }

      // 执行mount操作
      mountErr := volumeMounter.SetUp(fsGroup)
      // handle error

      // 更新asw
      markVolMountedErr := actualStateOfWorld.MarkVolumeAsMounted(
         volumeToMount.PodName,
         volumeToMount.Pod.UID,
         volumeToMount.VolumeName,
         volumeMounter,
         nil,
         volumeToMount.OuterVolumeSpecName,
         volumeToMount.VolumeGidValue,
         volumeToMount.VolumeSpec)
      // handle error

      return nil, nil
   }

   eventRecorderFunc := // event记录方法

   return volumetypes.GeneratedOperations{
      OperationFunc:     mountVolumeFunc,
      EventRecorderFunc: eventRecorderFunc,
      CompleteFunc:      util.OperationCompleteHook(volumePlugin.GetPluginName(), "volume_mount"),
   }, nil
}
```