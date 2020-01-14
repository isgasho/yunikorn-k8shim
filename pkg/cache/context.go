/*
Copyright 2020 Cloudera, Inc.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"fmt"
	"sync"

	"github.com/cloudera/yunikorn-k8shim/pkg/client"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"

	schedulercache "github.com/cloudera/yunikorn-k8shim/pkg/cache/external"
	"github.com/cloudera/yunikorn-k8shim/pkg/common"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/events"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/utils"
	"github.com/cloudera/yunikorn-k8shim/pkg/dispatcher"
	"github.com/cloudera/yunikorn-k8shim/pkg/log"
	plugin "github.com/cloudera/yunikorn-k8shim/pkg/plugin/predicates"

	"k8s.io/client-go/tools/cache"
)

// context maintains scheduling state, like apps and apps' tasks.
type Context struct {
	applications   map[string]*Application        // apps
	nodes          *schedulerNodes                // nodes
	schedulerCache *schedulercache.SchedulerCache // external cache
	apiProvider    client.APIProvider             // apis to interact with api-server, scheduler-core, etc
	predictor      *plugin.Predictor              // K8s predicates
	lock           *sync.RWMutex                  // lock
}

// Create a new context for the scheduler.
func NewContext(apis client.APIProvider) *Context {
	// create the context note that order is important:
	// volumebinder needs the informers
	// the cache needs informers and volumebinder
	// nodecontroller needs the cache
	// predictor need the cache, volumebinder and informers
	ctx := &Context{
		applications: make(map[string]*Application),
		apiProvider:  apis,
		lock:         &sync.RWMutex{},
	}

	// create the cache
	ctx.schedulerCache = schedulercache.NewSchedulerCache(apis.GetAPIs())

	// init the controllers and plugins (need the cache)
	ctx.nodes = newSchedulerNodes(apis.GetAPIs().SchedulerAPI, ctx.schedulerCache)
	ctx.predictor = plugin.NewPredictor(schedulercache.GetPluginArgs(), apis.IsTestingMode())

	return ctx
}

func (ctx *Context) AddSchedulingEventHandlers() {
	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.NodeInformerHandlers,
		AddFn:    ctx.addNode,
		UpdateFn: ctx.updateNode,
		DeleteFn: ctx.deleteNode,
	})

	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.PodInformerHandlers,
		FilterFn: ctx.filterPods,
		AddFn:    ctx.addPodToCache,
		UpdateFn: ctx.updatePodInCache,
		DeleteFn: ctx.removePodFromCache,
	})

	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.ConfigMapInformerHandlers,
		FilterFn: ctx.filterConfigMaps,
		AddFn:    ctx.addConfigMaps,
		UpdateFn: ctx.updateConfigMaps,
		DeleteFn: ctx.deleteConfigMaps,
	})
}

func (ctx *Context) addNode(obj interface{}) {
	node, err := convertToNode(obj)
	if err != nil {
		log.Logger.Error("node conversion failed", zap.Error(err))
		return
	}

	// add node to secondary scheduler cache
	log.Logger.Info("adding node to cache", zap.String("NodeName", node.Name))
	ctx.schedulerCache.AddNode(node)

	// add node to internal cache
	ctx.nodes.addNode(node)

	// post the event
	events.GetRecorder().Eventf(node, v1.EventTypeNormal, "Accepted",
		"node is accepted by the scheduler")
}

func (ctx *Context) updateNode(oldObj, newObj interface{}) {
	// we only trigger update when resource changes
	oldNode, err := convertToNode(oldObj)
	if err != nil {
		log.Logger.Error("old node conversion failed",
			zap.Error(err))
		return
	}

	newNode, err := convertToNode(newObj)
	if err != nil {
		log.Logger.Error("new node conversion failed",
			zap.Error(err))
		return
	}

	// update secondary cache
	log.Logger.Debug("updating node in cache",
		zap.String("OldNodeName", oldNode.Name))
	if err := ctx.schedulerCache.UpdateNode(oldNode, newNode); err != nil {
		log.Logger.Error("unable to update node in scheduler cache",
			zap.Error(err))
		return
	}

	// update primary cache
	ctx.nodes.updateNode(oldNode, newNode)
}

func (ctx *Context) deleteNode(obj interface{}) {
	node, err := convertToNode(obj)
	if err != nil {
		log.Logger.Error("node conversion failed", zap.Error(err))
		return
	}

	// delete node from secondary cache
	log.Logger.Debug("delete node from cache", zap.String("nodeName", node.Name))
	if err := ctx.schedulerCache.RemoveNode(node); err != nil {
		log.Logger.Error("unable to delete node from scheduler cache",
			zap.Error(err))
		return
	}

	// delete node from primary cache
	ctx.nodes.deleteNode(node)

	// post the event
	events.GetRecorder().Eventf(node, v1.EventTypeNormal, "Deleted",
		"node is deleted from the scheduler")
}

func (ctx *Context) addPodToCache(obj interface{}) {
	pod, err := utils.Convert2Pod(obj)
	if err != nil {
		log.Logger.Error("failed to add pod to cache", zap.Error(err))
		return
	}

	log.Logger.Info("adding pod to cache", zap.String("podName", pod.Name))
	if err := ctx.schedulerCache.AddPod(pod); err != nil {
		log.Logger.Error("add pod to scheduler cache failed",
			zap.String("podName", pod.Name),
			zap.Error(err))
	}
}

func (ctx *Context) removePodFromCache(obj interface{}) {
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*v1.Pod)
		if !ok {
			log.Logger.Error("Cannot convert to *v1.Pod", zap.Any("pod", obj))
			return
		}
	default:
		log.Logger.Error("Cannot convert to *v1.Pod", zap.Any("pod", obj))
		return
	}

	log.Logger.Info("removing pod from cache", zap.String("podName", pod.Name))
	if err := ctx.schedulerCache.RemovePod(pod); err != nil {
		log.Logger.Error("failed to remove pod from scheduler cache",
			zap.String("podName", pod.Name),
			zap.Error(err))
	}
}

func (ctx *Context) updatePodInCache(oldObj, newObj interface{}) {
	oldPod, err := utils.Convert2Pod(oldObj)
	if err != nil {
		log.Logger.Error("failed to update pod in cache", zap.Error(err))
		return
	}
	newPod, err := utils.Convert2Pod(newObj)
	if err != nil {
		log.Logger.Error("failed to update pod in cache", zap.Error(err))
		return
	}
	if err := ctx.schedulerCache.UpdatePod(oldPod, newPod); err != nil {
		log.Logger.Debug("failed to update pod in cache",
			zap.String("podName", oldPod.Name),
			zap.Error(err))
	}
}


// filter pods by scheduler name and state
func (ctx *Context) filterPods(obj interface{}) bool {
	switch obj := obj.(type) {
	case *v1.Pod:
		return utils.IsSchedulablePod(obj)
	default:
		return false
	}
}

// filter configMap for the scheduler
func (ctx *Context) filterConfigMaps(obj interface{}) bool {
	switch obj := obj.(type) {
	case *v1.ConfigMap:
		return obj.Name == common.DefaultConfigMapName
	default:
		return false
	}
}

// when detects the configMap for the scheduler is added, trigger hot-refresh
func (ctx *Context) addConfigMaps(obj interface{}) {
	log.Logger.Debug("configMap added")
	ctx.triggerReloadConfig()
}

// when detects the configMap for the scheduler is updated, trigger hot-refresh
func (ctx *Context) updateConfigMaps(obj, newObj interface{}) {
	log.Logger.Debug("trigger scheduler to reload configuration")
	// When update event is received, it is not guaranteed the data mounted to the pod
	// is also updated. This is because the actual update in pod's volume is ensured
	// by kubelet, kubelet is checking whether the mounted ConfigMap is fresh on every
	// periodic sync. As a result, the total delay from the moment when the ConfigMap
	// is updated to the moment when new keys are projected to the pod can be as long
	// as kubelet sync period + ttl of ConfigMaps cache in kubelet.
	// We trigger configuration reload, on yunikorn-core side, it keeps checking config
	// file state once this is called. And the actual reload happens when it detects
	// actual changes on the content.
	ctx.triggerReloadConfig()
}

// when detects the configMap for the scheduler is deleted, no operation needed here
// we assume there will be a consequent add operation after delete, so we treat it like a update.
func (ctx *Context) deleteConfigMaps(obj interface{}) {
	log.Logger.Debug("configMap deleted")
}

func (ctx *Context) triggerReloadConfig() {
	log.Logger.Info("trigger scheduler configuration reloading")
	clusterId := ctx.apiProvider.GetAPIs().Conf.ClusterID
	if err := ctx.apiProvider.GetAPIs().SchedulerAPI.ReloadConfiguration(clusterId); err != nil {
		log.Logger.Error("reload configuration failed", zap.Error(err))
	}
}

// evaluate given predicates based on current context
func (ctx *Context) IsPodFitNode(name string, node string) error {
	// simply skip if predicates are not enabled
	if !ctx.predictor.Enabled() {
		return nil
	}

	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		// if pod exists in cache, try to run predicates
		if targetNode := ctx.schedulerCache.GetNode(node); targetNode != nil {
			meta := ctx.predictor.GetPredicateMeta(pod, ctx.schedulerCache.GetNodesInfoMap())
			return ctx.predictor.Predicates(pod, meta, targetNode)
		}
	}
	return fmt.Errorf("predicates were not running because pod or node was not found in cache")
}

// call volume binder to bind pod volumes if necessary,
// internally, volume binder maintains a cache (podBindingCache) for pod volumes,
// and before calling this, they should have been updated by FindPodVolumes and AssumePodVolumes.
func (ctx *Context) bindPodVolumes(pod *v1.Pod) error {
	podKey := string(pod.UID)
	// the assumePodVolumes was done in scheduler-core, because these assumed pods are cached
	// during scheduling process as they have directly impact to other scheduling processes.
	// when assumePodVolumes was called, we caches the value if all pod volumes are bound in schedulerCache,
	// then here we just need to retrieve that value from cache, to skip bindings if volumes are already bound.
	if assumedPod, exist := ctx.schedulerCache.GetPod(podKey); exist {
		if ctx.schedulerCache.ArePodVolumesAllBound(podKey) {
			log.Logger.Info("Binding Pod Volumes skipped: all volumes already bound",
				zap.String("podName", pod.Name))
		} else {
			log.Logger.Info("Binding Pod Volumes", zap.String("podName", pod.Name))
			return ctx.apiProvider.GetAPIs().VolumeBinder.Binder.BindPodVolumes(assumedPod)
		}
	}
	return nil
}

// assume a pod will be running on a node, in scheduler, we maintain
// a cache where stores info for each node what pods are supposed to
// be running on it. And we keep this cache in-sync between core and the shim.
// this way, the core can make allocation decisions with consideration of
// other assumed pods before they are actually bound to the node (bound is slow).
func (ctx *Context) AssumePod(name string, node string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		// when add assumed pod, we make a copy of the pod to avoid
		// modifying its original reference. otherwise, it may have
		// race when some other go-routines accessing it in parallel.
		if targetNode := ctx.schedulerCache.GetNode(node); targetNode != nil {
			assumedPod := pod.DeepCopy()
			// assume pod volumes, this will update bindings info in cache
			// assume pod volumes before assuming the pod
			// this will update scheduler cache with essential PV/PVC binding info
			var allBound = true
			// volume builder might be null in UTs
			if ctx.apiProvider.GetAPIs().VolumeBinder != nil {
				var err error
				allBound, err = ctx.apiProvider.GetAPIs().VolumeBinder.Binder.AssumePodVolumes(pod, node)
				if err != nil {
					return err
				}
			}
			// assign the node name for pod
			assumedPod.Spec.NodeName = node
			return ctx.schedulerCache.AssumePod(assumedPod, allBound)
		}
	}
	return nil
}

// forget pod must be called when a pod is assumed to be running on a node,
// but then for some reason it is failed to bind or released.
func (ctx *Context) ForgetPod(name string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()

	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		log.Logger.Debug("forget pod", zap.String("pod", pod.Name))
		return ctx.schedulerCache.ForgetPod(pod)
	}
	log.Logger.Debug("unable to forget pod",
		zap.String("reason", fmt.Sprintf("pod %s not found in scheduler cache", name)))
	return nil
}

func (ctx *Context) UpdateApplication(app *Application) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	ctx.applications[app.applicationID] = app
}

// inform the scheduler that the application is completed,
// the complete state may further explained to completed_with_errors(failed) or successfully_completed,
// either way we need to release all allocations (if exists) for this application
func (ctx *Context) NotifyApplicationComplete(appID string) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if _, ok := ctx.GetApplication(appID); ok {
		ev := NewSimpleApplicationEvent(appID, events.CompleteApplication)
		dispatcher.Dispatch(ev)
	}
}

func (ctx *Context) AddApplication(request *AddApplicationRequest) *Application {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	app := NewApplication(
		request.Metadata.ApplicationID,
		request.Metadata.QueueName,
		request.Metadata.User,
		request.Metadata.Tags,
		ctx.apiProvider.GetAPIs().SchedulerAPI)

	// add into cache
	ctx.applications[app.applicationID] = app

	// trigger recovery
	if request.Recovery {
		if app.GetApplicationState() == events.States().Application.New {
			log.Logger.Info("start to recover the app",
				zap.String("appId", app.applicationID))
			dispatcher.Dispatch(NewSimpleApplicationEvent(app.applicationID, events.RecoverApplication))
		}
	}

	log.Logger.Info("app added",
		zap.String("appID", app.applicationID),
		zap.Bool("recovery", request.Recovery))
	return app
}

func (ctx *Context) GetApplication(appID string) (*Application, bool) {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if app, ok := ctx.applications[appID]; ok {
		return app, true
	}
	return nil, false
}

// this implements ApplicationManagementProtocol
func (ctx *Context) AddTask(request *AddTaskRequest) {
	if app, ok := ctx.GetApplication(request.Metadata.ApplicationID); ok {
		if _, err := app.GetTask(request.Metadata.TaskID); err != nil {
			task := NewTask(request.Metadata.TaskID, app, ctx, request.Metadata.Pod)
			// in recovery mode, task is considered as allocated
			if request.Recovery {
				task.setAllocated(request.Metadata.Pod.Spec.NodeName)
			}
			app.AddTask(&task)
			log.Logger.Info("task added",
				zap.String("appID", app.applicationID),
				zap.String("taskID", task.taskID))
		}
	}
}

func (ctx *Context) getTask(appID string, taskID string) (*Task, error) {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if app, ok := ctx.applications[appID]; ok {
		if task, err := app.GetTask(taskID); err == nil {
			return task, nil
		}
	}
	return nil, fmt.Errorf("application %s is not found in context", appID)
}

func (ctx *Context) SelectApplications(filter func(app *Application) bool) []*Application {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()

	apps := make([]*Application, 0)
	for _, app := range ctx.applications {
		if filter != nil && !filter(app) {
			continue
		}
		apps = append(apps, app)
	}

	return apps
}

func (ctx *Context) ApplicationEventHandler() func(obj interface{}) {
	return func(obj interface{}) {
		if event, ok := obj.(events.ApplicationEvent); ok {
			app, exist := ctx.GetApplication(event.GetApplicationID())
			if !exist {
				log.Logger.Error("failed to handle application event",
					zap.String("reason", "application not exist"))
				return
			}

			if app.canHandle(event) {
				if err := app.handle(event); err != nil {
					log.Logger.Error("failed to handle application event",
						zap.String("event", string(event.GetEvent())),
						zap.Error(err))
				}
			}
		}
	}
}

func (ctx *Context) TaskEventHandler() func(obj interface{}) {
	return func(obj interface{}) {
		if event, ok := obj.(events.TaskEvent); ok {
			task, err := ctx.getTask(event.GetApplicationID(), event.GetTaskID())
			if err != nil {
				log.Logger.Error("failed to handle application event", zap.Error(err))
				return
			}

			if task.canHandle(event) {
				if err = task.handle(event); err != nil {
					log.Logger.Error("failed to handle task event",
						zap.String("applicationID", task.applicationID),
						zap.String("taskID", task.taskID),
						zap.String("event", string(event.GetEvent())),
						zap.Error(err))
				}
			}
		}
	}
}

func (ctx *Context) SchedulerNodeEventHandler() func(obj interface{}) {
	if ctx != nil && ctx.nodes != nil {
		return ctx.nodes.schedulerNodeEventHandler()
	}
	// this is not required in some tests
	return nil
}

func (ctx *Context) Run(stopCh <-chan struct{}) {
	if ctx != nil {
		ctx.apiProvider.Run(stopCh)
	}
}
