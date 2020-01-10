package general

import (
	"fmt"

	"github.com/cloudera/yunikorn-k8shim/pkg/cache"
	"github.com/cloudera/yunikorn-k8shim/pkg/cache/apis"
	"github.com/cloudera/yunikorn-k8shim/pkg/cache/protocols"
	"github.com/cloudera/yunikorn-k8shim/pkg/common"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/events"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/utils"
	"github.com/cloudera/yunikorn-k8shim/pkg/dispatcher"
	"github.com/cloudera/yunikorn-k8shim/pkg/log"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sCache "k8s.io/client-go/tools/cache"
)

// generic app management service watches events from all the pods,
// it recognize apps by reading pod's spec labels, if there are proper info such as
// applicationID, queue name found, and claim it as an app or a app task,
// then report them to scheduler cache by calling am protocol
type GenericAppManagementService struct {
	// am protocol is used to sync info with scheduler cache
	amProtocol protocols.ApplicationManagementProtocol
	// shared context provides clients to communicate with api-server
	apiSet *apis.APISet
}

func NewGeneralAppManagementService(amProtocol protocols.ApplicationManagementProtocol,
	apiSet *apis.APISet) *GenericAppManagementService {
	return &GenericAppManagementService{
		amProtocol: amProtocol,
		apiSet:     apiSet,
	}
}

// this implements AppManagementService interface
func (os *GenericAppManagementService) Name() string {
	return "generic-app-management-service"
}

// this implements AppManagementService interface
func (os *GenericAppManagementService) ServiceInit() error {
	os.apiSet.AddEventHandler(
		&apis.ResourceEventHandlers{
			Type:     apis.PodInformerEvents,
			FilterFn: os.filterPods,
			AddFn:    os.addPod,
			UpdateFn: os.updatePod,
			DeleteFn: os.deletePod,
		})
	return nil
}

// this implements AppManagementService interface
func (os *GenericAppManagementService) Start() error {
	// generic app manager leverages the shared context,
	// no other service, go routine is required to be started
	return nil
}

// this implements AppManagementService interface
func (os *GenericAppManagementService) Stop() error {
	// noop
	return nil
}

func (os *GenericAppManagementService) addApplicationInternal(pod *v1.Pod, recovery bool) (*cache.Application, bool) {
	log.Logger.Debug("add pod",
		zap.String("namespace", pod.Namespace),
		zap.String("podName", pod.Name),
		zap.String("podUID", string(pod.UID)),
		zap.String("state", string(pod.Status.Phase)))

	appId, err := utils.GetApplicationIdFromPod(pod)
	if err != nil {
		log.Logger.Error("unable to get application by given pod", zap.Error(err))
		return nil, false
	}

	added := false
	// found appId, now see if this app is already existed
	app, ok := os.amProtocol.GetApplication(appId)
	if !ok {
		// tags will at least have namespace info
		// labels or annotations from the pod can be added when needed
		// user info is retrieved via service account
		tags := map[string]string{}
		if pod.Namespace == "" {
			tags["namespace"] = "default"
		} else {
			tags["namespace"] = pod.Namespace
		}
		// get the application owner (this is all that is available as far as we can find)
		user := pod.Spec.ServiceAccountName
		// add or recovery this app
		app = os.amProtocol.AddApplication(&protocols.AddApplicationRequest{
			ApplicationID: appId,
			QueueName:     utils.GetQueueNameFromPod(pod),
			User:          user,
			Tags:          tags,
			Recovery:      recovery,
		})
		added = true
	}

	// app already exist, add the task if needed
	if _, err := app.GetTask(string(pod.UID)); err != nil {
		os.amProtocol.AddTask(&protocols.AddTaskRequest{
			ApplicationID: app.GetApplicationId(),
			TaskID:        string(pod.UID),
			Pod:           pod,
			Recovery:      recovery,
		})
	}

	return app, added
}

// recover the app state if it is needed, returns the app and recovering=true to the caller
// so the caller knows which app is under recovering
func (os *GenericAppManagementService) RecoverApplication(pod *v1.Pod) (app *cache.Application, recovering bool) {
	return os.addApplicationInternal(pod, true)
}

// filter pods by scheduler name and state
func (os *GenericAppManagementService) filterPods(obj interface{}) bool {
	switch obj.(type) {
	case *v1.Pod:
		pod := obj.(*v1.Pod)
		return utils.IsSchedulablePod(pod)
	default:
		return false
	}
}

func (os *GenericAppManagementService) validatePod(pod *v1.Pod) error {
	if pod.Spec.SchedulerName == "" || pod.Spec.SchedulerName != os.apiSet.GetClientSet().Conf.SchedulerName {
		// only pod with specific scheduler name is valid to us
		return fmt.Errorf("only pod whose spec has explicitly "+
			"specified schedulerName=%s is a valid scheduling-target, but schedulerName for pod %s(%s) is %s",
			os.apiSet.GetClientSet().Conf.SchedulerName, pod.Name, pod.UID, pod.Spec.SchedulerName)
	}

	if _, err := utils.GetApplicationIdFromPod(pod); err != nil {
		return err
	}

	return nil
}

func (os *GenericAppManagementService) addPod(obj interface{}) {
	pod, err := utils.Convert2Pod(obj)
	if err != nil {
		log.Logger.Error("failed to add pod", zap.Error(err))
		return
	}

	if pod.Status.Phase == v1.PodPending {
		os.addApplicationInternal(pod, false)
	}
}

// when pod resource is modified, we need to act accordingly
// e.g vertical scale out the pod, this requires the scheduler to be aware of this
func (os *GenericAppManagementService) updatePod(old, new interface{}) {
	// TODO
}

// this function is called when a pod is deleted from api-server.
// when a pod is completed, the equivalent task's state will also be completed
// optionally, we run a completionHandler per workload, in order to determine
// if a application is completed along with this pod's completion
func (os *GenericAppManagementService) deletePod(obj interface{}) {
	// when a pod is deleted, we need to check its role.
	// for spark, if driver pod is deleted, then we consider the app is completed
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = t
	case k8sCache.DeletedFinalStateUnknown:
		var err error
		pod, err = utils.Convert2Pod(t.Obj)
		if err != nil {
			log.Logger.Error(err.Error())
			return
		}
	default:
		log.Logger.Error("cannot convert to pod")
		return
	}

	appId, err := utils.GetApplicationIdFromPod(pod)
	if err != nil {
		log.Logger.Error("unable to get application by given pod", zap.Error(err))
		return
	}

	if application, ok := os.amProtocol.GetApplication(appId); ok {
		log.Logger.Debug("release allocation")
		dispatcher.Dispatch(cache.NewSimpleTaskEvent(
			application.GetApplicationId(), string(pod.UID), events.CompleteTask))

		log.Logger.Info("delete pod",
			zap.String("namespace", pod.Namespace),
			zap.String("podName", pod.Name),
			zap.String("podUID", string(pod.UID)))
		// starts a completion handler to handle the completion of a app on demand
		os.startCompletionHandler(application, pod)
	}
}

func (os *GenericAppManagementService) startCompletionHandler(app *cache.Application, pod *v1.Pod) {
	for name, value := range pod.Labels {
		if name == common.SparkLabelRole && value == common.SparkLabelRoleDriver {
			app.StartCompletionHandler(cache.CompletionHandler{
				CompleteFn: func() {
					podWatch, err := os.apiSet.GetClientSet().
						KubeClient.GetClientSet().CoreV1().
						Pods(pod.Namespace).Watch(metav1.ListOptions{Watch: true})
					if err != nil {
						log.Logger.Info("unable to create Watch for pod",
							zap.String("pod", pod.Name),
							zap.Error(err))
						return
					}

					for {
						select {
						case targetPod, ok := <-podWatch.ResultChan():
							if !ok {
								return
							}
							resp := targetPod.Object.(*v1.Pod)
							if resp.Status.Phase == v1.PodSucceeded && resp.UID == pod.UID {
								log.Logger.Info("spark driver completed, app completed",
									zap.String("pod", resp.Name),
									zap.String("appId", app.GetApplicationId()))
								dispatcher.Dispatch(cache.NewSimpleApplicationEvent(app.GetApplicationId(), events.CompleteApplication))
								return
							}
						}
					}
				},
			})
			return
		}
	}
}