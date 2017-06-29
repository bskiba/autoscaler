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

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"

	"github.com/golang/glog"
)

const (
	ProviderName = "kubemark"
)

type KubemarkCloudProvider struct {
	kubemarkManager *KubemarkManager
	nodeGroups      []*NodeGroup
}

func BuildKubemarkCloudProvider(kubemarkManager *KubemarkManager, specs []string) (*KubemarkCloudProvider, error) {
	kubemark := &KubemarkCloudProvider{
		kubemarkManager: kubemarkManager,
		nodeGroups:      make([]*NodeGroup, 0),
	}
	for _, spec := range specs {
		if err := kubemark.addNodeGroup(spec); err != nil {
			return nil, err
		}
	}
	return kubemark, nil
}

func (kubemark *KubemarkCloudProvider) addNodeGroup(spec string) error {
	nodeGroup, err := buildNodeGroup(spec, kubemark.kubemarkManager)
	if err != nil {
		return err
	}
	glog.Infof("Nodegroup: %+v", nodeGroup)
	err = kubemark.kubemarkManager.RegisterNodeGroup(nodeGroup)
	if err != nil {
		return err
	}
	kubemark.nodeGroups = append(kubemark.nodeGroups, nodeGroup)
	glog.Infof("Nodegroups: +%v", kubemark.nodeGroups)
	return nil
}

// Name returns name of the cloud provider.
func (kubemark *KubemarkCloudProvider) Name() string {
	return ProviderName
}

// NodeGroups returns all node groups configured for this cloud provider.
func (kubemark *KubemarkCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	result := make([]cloudprovider.NodeGroup, 0, len(kubemark.nodeGroups))
	for _, nodegroup := range kubemark.nodeGroups {
		result = append(result, nodegroup)
	}
	glog.Infof("Nodegroups: %+v", result)
	return result
}

// Pricing returns pricing model for this cloud provider or error if not available.
func (kubemark *KubemarkCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

// NodeGroup implements NodeGroup interfrace.
type NodeGroup struct {
	Name string

	kubemarkManager *KubemarkManager

	minSize int
	maxSize int
}

// NodeGroupForNode returns the node group for the given node.
func (kubemark *KubemarkCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	glog.Infof("Getting node group for node %+v", node)
	nodeGroupName, err := kubemark.kubemarkManager.GetNodeGroupForNode(node.ObjectMeta.Name)
	if err != nil {
		return nil, err
	}
	for i, nodeGroup := range kubemark.nodeGroups {
		if nodeGroup.Name == nodeGroupName {
			return kubemark.nodeGroups[i], nil
		}
	}
	return nil, nil
}

// Id returns nodegroup name.
func (nodeGroup *NodeGroup) Id() string {
	return nodeGroup.Name
}

// MinSize returns minimum size of the node group.
func (nodeGroup *NodeGroup) MinSize() int {
	return nodeGroup.minSize
}

// MaxSize returns maximum size of the node group.
func (nodeGroup *NodeGroup) MaxSize() int {
	return nodeGroup.maxSize
}

// Debug returns a debug string for the nodegroup.
func (nodeGroup *NodeGroup) Debug() string {
	return fmt.Sprintf("%s (%d:%d)", nodeGroup.Id(), nodeGroup.MinSize(), nodeGroup.MaxSize())
}

// Nodes returns a list of all nodes that belong to this node group.
func (nodeGroup *NodeGroup) Nodes() ([]string, error) {
	ids := make([]string, 0)
	nodes, err := nodeGroup.kubemarkManager.GetNodesForNodegroup(nodeGroup)
	if err != nil {
		return ids, err
	}
	for _, node := range nodes {
		ids = append(ids, ":////"+node)
	}
	return ids, nil
}

func (nodeGroup *NodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	size, err := nodeGroup.kubemarkManager.GetNodeGroupSize(nodeGroup)
	if err != nil {
		return err
	}
	if size <= nodeGroup.MinSize() {
		return fmt.Errorf("min size reached, nodes will not be deleted")
	}
	for _, node := range nodes {
		if err := nodeGroup.kubemarkManager.DeleteNode(nodeGroup, node.ObjectMeta.Name); err != nil {
			return err
		}
	}
	return nil
}

// IncreaseSize increases NodeGroup size
func (nodeGroup *NodeGroup) IncreaseSize(delta int) error {
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}
	size, err := nodeGroup.kubemarkManager.GetNodeGroupSize(nodeGroup)
	if err != nil {
		return err
	}
	if int(size)+delta > nodeGroup.MaxSize() {
		return fmt.Errorf("size increase too large - desired:%d max:%d", int(size)+delta, nodeGroup.MaxSize())
	}
	return nodeGroup.kubemarkManager.AddNodesToNodeGroup(nodeGroup, delta)
}

// TargetSize returns the current TARGET size of the node group. It is possible that the
// number is different from the number of nodes registered in Kuberentes.
func (nodeGroup *NodeGroup) TargetSize() (int, error) {
	size, err := nodeGroup.kubemarkManager.GetNodeGroupSize(nodeGroup)
	return int(size), err
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
func (nodeGroup *NodeGroup) DecreaseTargetSize(delta int) error {
	if delta >= 0 {
		return fmt.Errorf("size decrease must be negative")
	}
	size, err := nodeGroup.kubemarkManager.GetNodeGroupSize(nodeGroup)
	if err != nil {
		return err
	}
	nodes, err := nodeGroup.kubemarkManager.GetNodesForNodegroup(nodeGroup)
	if err != nil {
		return err
	}
	if int(size)+delta < len(nodes) {
		return fmt.Errorf("attempt to delete existing nodes targetSize:%d delta:%d existingNodes: %d",
			size, delta, len(nodes))
	}
	return nodeGroup.kubemarkManager.RemoveNodesFromNodeGroup(nodeGroup, -delta)
}

// TemplateNodeInfo returns a node template for this node group.
func (nodeGroup *NodeGroup) TemplateNodeInfo() (*schedulercache.NodeInfo, error) {
	return nil, cloudprovider.ErrNotImplemented
}

func buildNodeGroup(value string, kubemarkManager *KubemarkManager) (*NodeGroup, error) {
	spec, err := dynamic.SpecFromString(value, true)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node group spec: %v", err)
	}

	nodeGroup := &NodeGroup{
		Name:            spec.Name,
		kubemarkManager: kubemarkManager,
		minSize:         spec.MinSize,
		maxSize:         spec.MaxSize,
	}

	glog.Infof("Returning nodegroup: %+v", nodeGroup)

	return nodeGroup, nil
}
