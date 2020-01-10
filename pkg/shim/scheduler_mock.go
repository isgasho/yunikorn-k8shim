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

package main

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"gotest.tools/assert"
	v1 "k8s.io/api/core/v1"

	"github.com/cloudera/yunikorn-core/pkg/api"
	coreconfigs "github.com/cloudera/yunikorn-core/pkg/common/configs"
	"github.com/cloudera/yunikorn-core/pkg/entrypoint"
	"github.com/cloudera/yunikorn-k8shim/pkg/cache"
	"github.com/cloudera/yunikorn-k8shim/pkg/callback"
	"github.com/cloudera/yunikorn-k8shim/pkg/client"
	"github.com/cloudera/yunikorn-k8shim/pkg/common"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/test"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/utils"
	"github.com/cloudera/yunikorn-k8shim/pkg/conf"
	"github.com/cloudera/yunikorn-k8shim/pkg/log"
	"github.com/cloudera/yunikorn-scheduler-interface/lib/go/si"
)

const fakeClusterID = "test-cluster"
const fakeClusterVersion = "0.1.0"
const fakeClusterSchedulerName = "yunikorn-test"
const fakeClusterSchedulingInterval = time.Second

// fake cluster is used for testing
// it uses fake kube client to simulate API calls with k8s, all other code paths are real
type MockScheduler struct {
	context     *cache.Context
	scheduler   *KubernetesShim
	proxy       api.SchedulerAPI
	client      client.KubeClient
	coreContext *entrypoint.ServiceContext
	conf        string
	bindFn      func(pod *v1.Pod, hostID string) error
	deleteFn    func(pod *v1.Pod) error
	stopChan    chan struct{}
}

func (fc *MockScheduler) init(queues string) {
	configs := conf.SchedulerConf{
		ClusterID:      fakeClusterID,
		ClusterVersion: fakeClusterVersion,
		SchedulerName:  fakeClusterSchedulerName,
		Interval:       fakeClusterSchedulingInterval,
		KubeConfig:     "",
		TestMode:       true,
	}

	conf.Set(&configs)
	fc.conf = queues
	fc.stopChan = make(chan struct{})
	// default functions for bind and delete, this can be override if necessary
	if fc.deleteFn == nil {
		fc.deleteFn = func(pod *v1.Pod) error {
			fmt.Printf("pod deleted")
			return nil
		}
	}

	if fc.bindFn == nil {
		fc.bindFn = func(pod *v1.Pod, hostID string) error {
			fmt.Printf("pod bound")
			return nil
		}
	}

	serviceContext := entrypoint.StartAllServices()
	rmProxy := serviceContext.RMProxy
	coreconfigs.MockSchedulerConfigByData([]byte(fc.conf))

	fakeClient := test.NewKubeClientMock()
	fakeClient.MockBindFn(fc.bindFn)
	fakeClient.MockDeleteFn(fc.deleteFn)

	schedulerAPI, ok := rmProxy.(api.SchedulerAPI)
	if !ok {
		log.Logger.Debug("cast failed unexpected object",
			zap.Any("schedulerAPI", rmProxy))
	}
	context := cache.NewContextInternal(client.NewAPIFactory(schedulerAPI, fakeClient, &configs), true)
	rmCallback := callback.NewAsyncRMCallback(context)
	ss := newShimSchedulerInternal(schedulerAPI, context, nil, rmCallback)

	fc.context = context
	fc.scheduler = ss
	fc.proxy = schedulerAPI
	fc.client = fakeClient
	fc.coreContext = serviceContext
}

func (fc *MockScheduler) start() {
	fc.scheduler.run()
}

func (fc *MockScheduler) addNode(nodeName string, memory, cpu int64) error {
	nodeResource := common.NewResourceBuilder().
		AddResource(common.Memory, memory).
		AddResource(common.CPU, cpu).
		Build()
	node := common.CreateFromNodeSpec(nodeName, nodeName, nodeResource)
	request := common.CreateUpdateRequestForNewNode(node)
	fmt.Printf("report new nodes to scheduler, request: %s", request.String())
	return fc.proxy.Update(&request)
}

func (fc *MockScheduler) addTask(tid string, ask *si.Resource, app *cache.Application) cache.Task {
	task := cache.CreateTaskForTest(tid, app, ask, fc.context)
	app.AddTask(&task)
	return task
}

func (fc *MockScheduler) waitForSchedulerState(t *testing.T, expectedState string) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if fc.scheduler.GetSchedulerState() == expectedState {
			break
		}
		log.Logger.Info("waiting for scheduler state",
			zap.String("expected", expectedState),
			zap.String("actual", fc.scheduler.GetSchedulerState()))
		time.Sleep(time.Second)
		if time.Now().After(deadline) {
			t.Errorf("wait for scheduler to reach state %s failed, current state %s",
				expectedState, fc.scheduler.GetSchedulerState())
		}
	}
}

func (fc *MockScheduler) waitAndAssertApplicationState(t *testing.T, appID, expectedState string) {
	appList := fc.context.SelectApplications(func(app *cache.Application) bool {
		return app.GetApplicationID() == appID
	})
	assert.Equal(t, len(appList), 1)
	assert.Equal(t, appList[0].GetApplicationID(), appID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if appList[0].GetApplicationState() == expectedState {
			break
		}
		log.Logger.Info("waiting for app state",
			zap.String("expected", expectedState),
			zap.String("actual", appList[0].GetApplicationState()))
		time.Sleep(time.Second)
		if time.Now().After(deadline) {
			t.Errorf("application %s doesn't reach expected state in given time, expecting: %s, actual: %s",
				appID, expectedState, appList[0].GetApplicationState())
		}
	}
}

func (fc *MockScheduler) addApplication(app *cache.Application) {
	fc.context.AddApplication(&cache.AddApplicationRequest{
		Metadata: cache.ApplicationMetadata{
			ApplicationID: app.GetApplicationID(),
			QueueName:     app.GetQueue(),
			User:          "",
			Tags:          nil,
		},
		Recovery:      false,
	})
}

func (fc *MockScheduler) newApplication(appID, queueName string) *cache.Application {
	app := cache.NewApplication(appID, queueName, "testuser", map[string]string{}, fc.proxy)
	return app
}

func (fc *MockScheduler) waitAndAssertTaskState(t *testing.T, appID, taskID, expectedState string) {
	appList := fc.context.SelectApplications(func(app *cache.Application) bool {
		return app.GetApplicationID() == appID
	})
	assert.Equal(t, len(appList), 1)
	assert.Equal(t, appList[0].GetApplicationID(), appID)

	task, err := appList[0].GetTask(taskID)
	assert.Assert(t, err == nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if task.GetTaskState() == expectedState {
			break
		}
		log.Logger.Info("waiting for task state",
			zap.String("expected", expectedState),
			zap.String("actual", task.GetTaskState()))
		time.Sleep(time.Second)
		if time.Now().After(deadline) {
			t.Errorf("task %s doesn't reach expected state in given time, expecting: %s, actual: %s",
				taskID, expectedState, task.GetTaskState())
		}
	}
}

func (fc *MockScheduler) waitAndVerifySchedulerAllocations(
	queueName, partitionName, applicationID string, expectedNumOfAllocations int) error {
	partition := fc.coreContext.Cache.GetPartition(partitionName)
	if partition == nil {
		return fmt.Errorf("partition %s is not found in the scheduler context", partitionName)
	}

	return utils.WaitForCondition(func() bool {
		for _, app := range partition.GetApplications() {
			if app.ApplicationID == applicationID {
				if len(app.GetAllAllocations()) == expectedNumOfAllocations {
					return true
				}
			}
		}
		return false
	}, time.Second, 5*time.Second)
}

func (fc *MockScheduler) stop() {
	close(fc.stopChan)
	fc.scheduler.stop()
}
