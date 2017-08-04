/*
Copyright 2017 The Kubernetes Authors.

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

package kubemark

import (
	"fmt"

	"k8s.io/kubernetes/pkg/kubemark"
	apiv1 "k8s.io/api/core/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/informers"

	"github.com/golang/glog"
)

const (
	namespaceKubemark = "kubemark"
	hollowNodeName    = "hollow-node"
	kindLabel         = "node-kind"
	nodeGroupLabel    = "autoscaling.k8s.io/nodegroup"
)

// KubemarkManager
type KubemarkManager struct {
	controller *kubemark.KubemarkController
}

func CreateKubemarkManager(externalClient kubeclient.Interface, kubemarkClient kubeclient.Interface, stop <-chan struct{}) (*KubemarkManager, error) {
	externalInformerFactory := informers.NewSharedInformerFactory(externalClient, 0)
	kubemarkInformerFactory := informers.NewSharedInformerFactory(kubemarkClient, 0)
	nodes := kubemarkInformerFactory.Core().V1().Nodes()
	nodes.Informer().Run(stop)
	controller, err := kubemark.NewKubemarkController(externalClient, externalInformerFactory, kubemarkClient, nodes)
	if err != nil {
		return nil, err
	}
	manager:= &KubemarkManager{
		controller: controller,
	}
	externalInformerFactory.Start(stop)
	manager.controller.Init()
	return manager, err
}

func (kubemarkManager *KubemarkManager) RegisterNodeGroup(nodeGroup *NodeGroup) error {
	glog.V(2).Infof("Registering nodegroup %v", nodeGroup.Name)
	ng := kubemarkManager.getNodeGroupByName(nodeGroup.Name)
	if ng == nil {
		for i := 0; i < nodeGroup.minSize; i++ {
			if err := kubemarkManager.addNodeToNodeGroup(nodeGroup); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetNodeGroupForNode returns the name of the nodeGroup to which node with nodeName
// belongs. Returns an error if nodeGroup for node was not found.
func (kubemarkManager *KubemarkManager) GetNodeGroupForNode(nodeName string) (string, error) {
	for _, podObj := range kubemarkManager.controller.externalCluster.podLister.GetStore().List() {
		pod := podObj.(*apiv1.Pod)
		if pod.ObjectMeta.Name == nodeName {
			//glog.Infof("returning pod Label %q", pod.ObjectMeta.Labels[nodeGroupLabel])
			//glog.Infof("Pod: %+v", pod)
			nodeGroup, ok := pod.ObjectMeta.Labels[nodeGroupLabel]
			if ok {
				return nodeGroup, nil
			}
			return "", fmt.Errorf("Can't find nodegroup for node %s. Node exists but does not have the nodeGroupName label.", nodeName)
		}
	}
	return "", fmt.Errorf("Can't find nodegroup for node %s", nodeName)
}

func (kubemarkManager *KubemarkManager) DeleteNode(nodeGroup *NodeGroup, node string) error {
	return kubemarkManager.controller.removeNodeFromNodeGroup(nodeGroup.Name, node) 
}

func (kubemarkManager *KubemarkManager) GetNodesForNodegroup(nodeGroup *NodeGroup) ([]string, error) {
	return kubemarkManager.controller.GetNodesForNodeGroup(nodeGroup.Name)
}

func (kubemarkManager *KubemarkManager) GetNodeGroupSize(nodeGroup *NodeGroup) (int, error) {
	return kubemarkManager.controller.GetNodeGroupSize(nodeGroup.Name)
}

func (kubemarkManager *KubemarkManager) SetNodeGroupSize(nodeGroup *NodeGroup, size int) error {
	return kubemarkManager.controller.SetNodeGroupSize(nodeGroup.Name, size)
}


func (kubemarkManager *KubemarkManager) getNodeGroupByName(nodeGroupName string) *apiv1.ReplicationController {
	for _, obj := range kubemarkManager.externalCluster.rcLister.GetStore().List() {
		rc := obj.(*apiv1.ReplicationController)
		if rc.ObjectMeta.Labels[nodeGroupLabel] == nodeGroupName {
			return rc
		}
	}
	return nil
}
