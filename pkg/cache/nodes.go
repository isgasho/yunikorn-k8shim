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

	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"

	"github.com/cloudera/yunikorn-core/pkg/api"
	"github.com/cloudera/yunikorn-k8shim/pkg/cache/external"
	"github.com/cloudera/yunikorn-k8shim/pkg/common"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/events"
	"github.com/cloudera/yunikorn-k8shim/pkg/common/utils"
	"github.com/cloudera/yunikorn-k8shim/pkg/dispatcher"
	"github.com/cloudera/yunikorn-k8shim/pkg/log"
	"github.com/cloudera/yunikorn-scheduler-interface/lib/go/si"
)

// scheduler nodes maintain cluster nodes and their status for the scheduler
type schedulerNodes struct {
	proxy    api.SchedulerAPI
	nodesMap map[string]*SchedulerNode
	cache    *external.SchedulerCache
	lock     *sync.RWMutex
}

func newSchedulerNodes(schedulerAPI api.SchedulerAPI, cache *external.SchedulerCache) *schedulerNodes {
	return &schedulerNodes{
		proxy:    schedulerAPI,
		nodesMap: make(map[string]*SchedulerNode),
		cache:    cache,
		lock:     &sync.RWMutex{},
	}
}

func (nc *schedulerNodes) getNode(name string) *SchedulerNode {
	nc.lock.RLock()
	defer nc.lock.RUnlock()
	if node, ok := nc.nodesMap[name]; ok {
		return node
	}
	return nil
}

func convertToNode(obj interface{}) (*v1.Node, error) {
	if node, ok := obj.(*v1.Node); ok {
		return node, nil
	}
	return nil, fmt.Errorf("cannot convert to *v1.Node: %v", obj)
}

func equals(n1 *v1.Node, n2 *v1.Node) bool {
	n1Resource := common.GetNodeResource(&n1.Status)
	n2Resource := common.GetNodeResource(&n2.Status)
	return common.Equals(n1Resource, n2Resource)
}

func (nc *schedulerNodes) addExistingAllocation(pod *v1.Pod) error {
	nc.lock.Lock()
	defer nc.lock.Unlock()
	if utils.IsAssignedPod(pod) {
		if appID, err := utils.GetApplicationIDFromPod(pod); err == nil {
			if schedulerNode, ok := nc.nodesMap[pod.Spec.NodeName]; ok {
				schedulerNode.addExistingAllocation(&si.Allocation{
					AllocationKey:    pod.Name,
					AllocationTags:   nil,
					UUID:             string(pod.UID),
					ResourcePerAlloc: common.GetPodResource(pod),
					QueueName:        utils.GetQueueNameFromPod(pod),
					NodeID:           pod.Spec.NodeName,
					ApplicationID:    appID,
					PartitionName:    common.DefaultPartition,
				})
			}
		} else {
			return err
		}
	}
	return nil
}

func (nc *schedulerNodes) addNode(node *v1.Node) {
	nc.addAndReportNode(node, true)
}

func (nc *schedulerNodes) addAndReportNode(node *v1.Node, reportNode bool) {
	nc.lock.Lock()
	defer nc.lock.Unlock()

	// add node to nodes map
	if _, ok := nc.nodesMap[node.Name]; !ok {
		log.Logger.Info("adding node to context",
			zap.String("nodeName", node.Name),
			zap.String("UID", string(node.UID)),
			zap.Bool("schedulable", !node.Spec.Unschedulable))
		newNode := newSchedulerNode(node.Name, string(node.UID),
			common.GetNodeResource(&node.Status), nc.proxy, !node.Spec.Unschedulable)
		nc.nodesMap[node.Name] = newNode
	}

	// once node is added to scheduler, first thing is to recover its state
	// node might already be in healthy state, previously recovered during recovery process,
	// do not trigger recover again in this case.
	if reportNode {
		if node, ok := nc.nodesMap[node.Name]; ok {
			if node.getNodeState() == events.States().Node.New {
				dispatcher.Dispatch(CachedSchedulerNodeEvent{
					NodeID: node.name,
					Event:  events.RecoverNode,
				})
			}
		}
	}
}

func (nc *schedulerNodes) drainNode(node *v1.Node) {
	log.Logger.Info("draining node", zap.String("name", node.Name))
	if node, ok := nc.nodesMap[node.Name]; ok {
		if node.getNodeState() == events.States().Node.Healthy {
			dispatcher.Dispatch(CachedSchedulerNodeEvent{
				NodeID: node.name,
				Event:  events.DrainNode,
			})
		}
	}
}

func (nc *schedulerNodes) restoreNode(node *v1.Node) {
	log.Logger.Info("restoring node", zap.String("name", node.Name))
	if node, ok := nc.nodesMap[node.Name]; ok {
		if node.getNodeState() == events.States().Node.Draining {
			dispatcher.Dispatch(CachedSchedulerNodeEvent{
				NodeID: node.name,
				Event:  events.RestoreNode,
			})
		}
	}
}

func (nc *schedulerNodes) updateNode(oldNode, newNode *v1.Node) {
	nc.lock.Lock()
	defer nc.lock.Unlock()

	// cordon or restore node
	if (!oldNode.Spec.Unschedulable) && newNode.Spec.Unschedulable {
		nc.drainNode(newNode)
	} else if oldNode.Spec.Unschedulable && !newNode.Spec.Unschedulable {
		nc.restoreNode(newNode)
	}

	// node resource changes
	if equals(oldNode, newNode) {
		log.Logger.Info("Node status not changed, skip this UpdateNode event")
		return
	}

	node := common.CreateFrom(newNode)
	request := common.CreateUpdateRequestForUpdatedNode(node)
	log.Logger.Info("report updated nodes to scheduler", zap.Any("request", request))
	if err := nc.proxy.Update(&request); err != nil {
		log.Logger.Info("hitting error while handling UpdateNode", zap.Error(err))
	}
}

func (nc *schedulerNodes) deleteNode(node *v1.Node) {
	nc.lock.Lock()
	defer nc.lock.Unlock()

	n := common.CreateFrom(node)
	request := common.CreateUpdateRequestForDeleteNode(n)
	log.Logger.Info("report updated nodes to scheduler", zap.Any("request", request.String()))
	if err := nc.proxy.Update(&request); err != nil {
		log.Logger.Info("hitting error while handling UpdateNode", zap.Error(err))
	}
}

func (nc *schedulerNodes) schedulerNodeEventHandler() func(obj interface{}) {
	return func(obj interface{}) {
		if event, ok := obj.(events.SchedulerNodeEvent); ok {
			if node := nc.getNode(event.GetNodeID()); node != nil {
				if node.canHandle(event) {
					if err := node.handle(event); err != nil {
						log.Logger.Error("failed to handle scheduler node event",
							zap.String("event", string(event.GetEvent())),
							zap.Error(err))
					}
				}
			}
		}
	}
}
