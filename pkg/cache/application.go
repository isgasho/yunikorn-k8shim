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

	"github.com/looplab/fsm"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cloudera/yunikorn-core/pkg/api"
	"github.com/cloudera/yunikorn-k8shim/pkg/client"
	"github.com/cloudera/yunikorn-k8shim/pkg/common"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/events"
	"github.com/cloudera/yunikorn-k8shim/pkg/conf"
	"github.com/cloudera/yunikorn-k8shim/pkg/dispatcher"
	"github.com/cloudera/yunikorn-k8shim/pkg/log"
	"github.com/cloudera/yunikorn-scheduler-interface/lib/go/si"
)

type Application struct {
	applicationID string
	queue         string
	partition     string
	user          string
	taskMap       map[string]*Task
	tags          map[string]string
	sm            *fsm.FSM
	lock          *sync.RWMutex
	ch            CompletionHandler
	schedulerAPI  api.SchedulerAPI
}

func (app *Application) String() string {
	return fmt.Sprintf("applicationID: %s, queue: %s, partition: %s,"+
		" totalNumOfTasks: %d, currentState: %s",
		app.applicationID, app.queue, app.partition, len(app.taskMap), app.GetApplicationState())
}

func NewApplication(appID, queueName, user string, tags map[string]string, scheduler api.SchedulerAPI) *Application {
	taskMap := make(map[string]*Task)
	app := &Application{
		applicationID: appID,
		queue:         queueName,
		partition:     common.DefaultPartition,
		user:          user,
		taskMap:       taskMap,
		tags:          tags,
		lock:          &sync.RWMutex{},
		ch:            CompletionHandler{running: false},
		schedulerAPI:  scheduler,
	}

	var states = events.States().Application
	app.sm = fsm.NewFSM(
		states.New,
		fsm.Events{
			{Name: string(events.SubmitApplication),
				Src: []string{states.New},
				Dst: states.Submitted},
			{Name: string(events.RecoverApplication),
				Src: []string{states.New},
				Dst: states.Recovering},
			{Name: string(events.AcceptApplication),
				Src: []string{states.Submitted, states.Recovering},
				Dst: states.Accepted},
			{Name: string(events.RunApplication),
				Src: []string{states.Accepted, states.Running},
				Dst: states.Running},
			{Name: string(events.CompleteApplication),
				Src: []string{states.Running},
				Dst: states.Completed},
			{Name: string(events.RejectApplication),
				Src: []string{states.Submitted},
				Dst: states.Rejected},
			{Name: string(events.FailApplication),
				Src: []string{states.Submitted, states.Rejected, states.Accepted, states.Running},
				Dst: states.Failed},
			{Name: string(events.KillApplication),
				Src: []string{states.Accepted, states.Running},
				Dst: states.Killing},
			{Name: string(events.KilledApplication),
				Src: []string{states.Killing},
				Dst: states.Killed},
		},
		fsm.Callbacks{
			string(events.SubmitApplication):   app.handleSubmitApplicationEvent,
			string(events.RecoverApplication):  app.handleRecoverApplicationEvent,
			string(events.RejectApplication):   app.handleRejectApplicationEvent,
			string(events.CompleteApplication): app.handleCompleteApplicationEvent,
		},
	)

	return app
}

func (app *Application) handle(ev events.ApplicationEvent) error {
	// Locking mechanism:
	// 1) when handle event transitions, we first obtain the object's lock,
	//    this helps us to place a pre-check before entering here, in case
	//    we receive some invalidate events. If this introduces performance
	//    regression, a possible way to optimize is to use a separate lock
	//    to protect the transition phase.
	// 2) Note, state machine calls those callbacks here, we must ensure
	//    they are lock-free calls. Otherwise the callback will be blocked
	//    because the lock is already held here.
	app.lock.Lock()
	defer app.lock.Unlock()
	log.Logger.Debug("application state transition",
		zap.String("appID", app.applicationID),
		zap.String("preState", app.sm.Current()),
		zap.String("pendingEvent", string(ev.GetEvent())))
	err := app.sm.Event(string(ev.GetEvent()), ev.GetArgs()...)
	// handle the same state transition not nil error (limit of fsm).
	if err != nil && err.Error() != "no transition" {
		return err
	}
	log.Logger.Debug("application state transition",
		zap.String("appID", app.applicationID),
		zap.String("postState", app.sm.Current()))
	return nil
}

func (app *Application) canHandle(ev events.ApplicationEvent) bool {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.sm.Can(string(ev.GetEvent()))
}

func (app *Application) GetTask(taskID string) (*Task, error) {
	app.lock.RLock()
	defer app.lock.RUnlock()
	if task, ok := app.taskMap[taskID]; ok {
		return task, nil
	}
	return nil, fmt.Errorf("task %s doesn't exist in application %s",
		taskID, app.applicationID)
}

func (app *Application) GetApplicationID() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.applicationID
}

func (app *Application) GetQueue() string {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.queue
}

func (app *Application) AddTask(task *Task) {
	app.lock.Lock()
	defer app.lock.Unlock()
	if _, ok := app.taskMap[task.taskID]; ok {
		// skip adding duplicate task
		return
	}
	app.taskMap[task.taskID] = task
}

func (app *Application) GetApplicationState() string {
	return app.sm.Current()
}

func (app *Application) GetPendingTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Pending)
}

func (app *Application) GetNewTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.New)
}

func (app *Application) GetAllocatedTasks() []*Task {
	app.lock.RLock()
	defer app.lock.RUnlock()
	return app.getTasks(events.States().Task.Allocated)
}

func (app *Application) getTasks(state string) []*Task {
	taskList := make([]*Task, 0)
	if len(app.taskMap) > 0 {
		for _, task := range app.taskMap {
			if task.GetTaskState() == state {
				taskList = append(taskList, task)
			}
		}
	}
	return taskList
}

// This is called in every scheduling interval,
// we are not using dispatcher here because we want to
// make state transition in sync mode in order to prevent
// generating too many duplicate events. However, it must
// ensure non of these calls is expensive, usually, they
// do nothing more than just triggering the state transition.
func (app *Application) Schedule() {
	var states = events.States().Application
	switch app.GetApplicationState() {
	case states.New:
		ev := NewSubmitApplicationEvent(app.GetApplicationID())
		if err := app.handle(ev); err != nil {
			log.Logger.Warn("failed to handle SUBMIT app event",
				zap.Error(err))
		}
	case states.Accepted:
		ev := NewRunApplicationEvent(app.GetApplicationID())
		if err := app.handle(ev); err != nil {
			log.Logger.Warn("failed to handle RUN app event",
				zap.Error(err))
		}
	case states.Running:
		if len(app.GetNewTasks()) > 0 {
			for _, task := range app.GetNewTasks() {
				// for each new task, we do a sanity check before moving the state to Pending_Schedule
				if err := task.sanityCheckBeforeScheduling(); err == nil {
					// note, if we directly trigger submit task event, it may spawn too many duplicate
					// events, because a task might be submitted multiple times before its state transits to PENDING.
					if handleErr := task.handle(
						NewSimpleTaskEvent(task.applicationID, task.taskID, events.InitTask)); handleErr != nil {
						// something goes wrong when transit task to PENDING state,
						// this should not happen because we already checked the state
						// before calling the transition. Nowhere to go, just log the error.
						log.Logger.Warn("init task failed", zap.Error(err))
					}
				} else {
					log.Logger.Debug("task is not ready for scheduling",
						zap.String("appID", task.applicationID),
						zap.String("taskID", task.taskID),
						zap.Error(err))
				}
			}
		}
	default:
		log.Logger.Debug("skipping scheduling application",
			zap.String("appID", app.GetApplicationID()),
			zap.String("appState", app.GetApplicationState()))
	}
}

func (app *Application) handleSubmitApplicationEvent(event *fsm.Event) {
	log.Logger.Info("handle app submission",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags: app.tags,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger.Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
	}
}

func (app *Application) handleRecoverApplicationEvent(event *fsm.Event) {
	log.Logger.Info("handle app recovering",
		zap.String("app", app.String()),
		zap.String("clusterID", conf.GetSchedulerConf().ClusterID))
	err := app.schedulerAPI.Update(
		&si.UpdateRequest{
			NewApplications: []*si.AddApplicationRequest{
				{
					ApplicationID: app.applicationID,
					QueueName:     app.queue,
					PartitionName: app.partition,
					Ugi: &si.UserGroupInformation{
						User: app.user,
					},
					Tags: app.tags,
				},
			},
			RmID: conf.GetSchedulerConf().ClusterID,
		})

	if err != nil {
		// submission failed
		log.Logger.Warn("failed to submit app", zap.Error(err))
		dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
	}
}

func (app *Application) handleRejectApplicationEvent(event *fsm.Event) {
	log.Logger.Info("app is rejected by scheduler", zap.String("appID", app.applicationID))
	// for rejected apps, we directly move them to failed state
	dispatcher.Dispatch(NewFailApplicationEvent(app.applicationID))
}

func (app *Application) handleCompleteApplicationEvent(event *fsm.Event) {
	//// shutdown the working channel
	//close(app.stopChan)
}

// a application can have one and at most one completion handler,
// the completion handler determines when a application is considered as stopped,
// such as for Spark, once driver is succeed, we think this application is completed.
// this interface can be customized for different type of apps.
type CompletionHandler struct {
	running    bool
	completeFn func()
}

func (app *Application) startCompletionHandler(client client.KubeClient, pod *v1.Pod) {
	for name, value := range pod.Labels {
		if name == common.SparkLabelRole && value == common.SparkLabelRoleDriver {
			app.startSparkCompletionHandler(client, pod)
			return
		}
	}
}

func (app *Application) startSparkCompletionHandler(client client.KubeClient, pod *v1.Pod) {
	// spark driver pod
	log.Logger.Info("start app completion handler",
		zap.String("pod", pod.Name),
		zap.String("appID", app.applicationID))
	if app.ch.running {
		return
	}

	app.ch = CompletionHandler{
		completeFn: func() {
			podWatch, err := client.GetClientSet().CoreV1().Pods(pod.Namespace).Watch(metav1.ListOptions{Watch: true})
			if err != nil {
				log.Logger.Info("unable to create Watch for pod",
					zap.String("pod", pod.Name),
					zap.Error(err))
				return
			}

			for targetPod := range podWatch.ResultChan() {
				resp, ok := targetPod.Object.(*v1.Pod)
				if !ok {
					log.Logger.Debug("cast failed unexpected object",
						zap.Any("PodObject", targetPod.Object))
				}
				if resp.Status.Phase == v1.PodSucceeded && resp.UID == pod.UID {
					log.Logger.Info("spark driver completed, app completed",
						zap.String("pod", resp.Name),
						zap.String("appID", app.applicationID))
					dispatcher.Dispatch(NewSimpleApplicationEvent(app.applicationID, events.CompleteApplication))
					return
				}
			}
		},
	}
	app.ch.start()
}

func (ch CompletionHandler) start() {
	if !ch.running {
		go ch.completeFn()
	}
}
