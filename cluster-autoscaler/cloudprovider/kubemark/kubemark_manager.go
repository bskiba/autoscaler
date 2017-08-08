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
	"math/rand"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api"
	kube_client "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	informers "k8s.io/kubernetes/pkg/client/informers/informers_generated/externalversions"

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
	nodeTemplate      *apiv1.ReplicationController
	externalCluster   ExternalCluster
	kubemarkCluster   KubemarkCluster
	createNodeQueue   chan *apiv1.ReplicationController
	nodeGroupSizeLock sync.Mutex
	nodeGroupSize     map[string]int
}

type ExternalCluster struct {
	rcLister  cache.SharedInformer
	podLister cache.SharedInformer
	client    kube_client.Interface
}

type KubemarkCluster struct {
	client            kube_client.Interface
	nodeLister        cache.SharedInformer
	nodesToDelete     map[string]bool
	nodesToDeleteLock sync.Mutex
}

func CreateKubemarkManager(externalClient kube_client.Interface, kubemarkClient kube_client.Interface, stop <-chan struct{}) (*KubemarkManager, error) {
	externalInformerFactory := informers.NewSharedInformerFactory(externalClient, 0)
	kubemarkInformerFactory := informers.NewSharedInformerFactory(kubemarkClient, 0)
	manager := &KubemarkManager{
		externalCluster: ExternalCluster{
			rcLister:  externalInformerFactory.InformerFor(&apiv1.ReplicationController{}, newReplicationControllerInformer),
			podLister: externalInformerFactory.InformerFor(&apiv1.Pod{}, newPodInformer),
			client:    externalClient,
		},
		kubemarkCluster: KubemarkCluster{
			nodeLister:        kubemarkInformerFactory.InformerFor(&apiv1.Node{}, newNodeInformer),
			client:            kubemarkClient,
			nodesToDelete:     make(map[string]bool),
			nodesToDeleteLock: sync.Mutex{},
		},
		createNodeQueue:   make(chan *apiv1.ReplicationController),
		nodeGroupSizeLock: sync.Mutex{},
		nodeGroupSize:     make(map[string]int),
	}

	externalInformerFactory.Start(stop)
	for _, synced := range externalInformerFactory.WaitForCacheSync(stop) {
		if !synced {
			return nil, fmt.Errorf("Failed to sync data  of external cluster.")
		}
	}
	kubemarkInformerFactory.Start(stop)
	for _, synced := range kubemarkInformerFactory.WaitForCacheSync(stop) {
		if !synced {
			return nil, fmt.Errorf("Failed to sync data  of external cluster.")
		}
	}
	manager.kubemarkCluster.nodeLister.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: manager.kubemarkCluster.removeUnneededNodes,
	})

	manager.kubemarkCluster.nodesToDelete = make(map[string]bool)

	go manager.runReplicationControllerCreation(stop)

	rand.Seed(time.Now().UTC().UnixNano())

	nodeTemplate, err := manager.getNodeTemplate()
	if err != nil {
		return nil, err
	}
	manager.nodeTemplate = nodeTemplate
	return manager, nil
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
	for _, podObj := range kubemarkManager.externalCluster.podLister.GetStore().List() {
		pod := podObj.(*apiv1.Pod)
		if pod.ObjectMeta.Name == nodeName {
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
	for _, podObj := range kubemarkManager.externalCluster.podLister.GetStore().List() {
		pod := podObj.(*apiv1.Pod)
		if pod.ObjectMeta.Name == node {
			if pod.ObjectMeta.Labels[nodeGroupLabel] != nodeGroup.Name {
				return fmt.Errorf("Can't delete node %s from nodegroup %s. Node is not in nodegroup.", node, nodeGroup.Name)
			}
			policy := metav1.DeletePropagationForeground
			kubemarkManager.nodeGroupSizeLock.Lock()
			defer kubemarkManager.nodeGroupSizeLock.Unlock()
			err := kubemarkManager.externalCluster.client.CoreV1().ReplicationControllers(namespaceKubemark).Delete(
				pod.ObjectMeta.Labels["name"],
				&metav1.DeleteOptions{PropagationPolicy: &policy})
			kubemarkManager.nodeGroupSize[nodeGroup.Name]--
			if err != nil {
				return err
			}
			glog.Infof("Marking node %s for deletion.", node)
			kubemarkManager.kubemarkCluster.markNodeForDeletion(node)
			return nil
		}
	}

	return fmt.Errorf("Can't delete node %s from nodegroup %s. Node does not exist.", node, nodeGroup.Name)
}

func (kubemarkManager *KubemarkManager) GetNodesForNodegroup(nodeGroup *NodeGroup) ([]string, error) {
	pods := kubemarkManager.externalCluster.podLister.GetStore().List()
	result := make([]string, 0, len(pods))
	for _, podObj := range pods {
		pod := podObj.(*apiv1.Pod)
		if pod.ObjectMeta.Labels[nodeGroupLabel] == nodeGroup.Name {
			result = append(result, pod.ObjectMeta.Name)
		}
	}
	return result, nil
}

func (kubemarkManager *KubemarkManager) GetNodeGroupSize(nodeGroup *NodeGroup) (int, error) {
	size := 0
	for _, rcObj := range kubemarkManager.externalCluster.rcLister.GetStore().List() {
		rc := rcObj.(*apiv1.ReplicationController)
		if rc.Labels[nodeGroupLabel] == nodeGroup.Name {
			size += 1
		}
	}
	return size, nil
}

func (kubemarkManager *KubemarkManager) GetNodeGroupTargetSize(nodeGroup *NodeGroup) (int, error) {
	kubemarkManager.nodeGroupSizeLock.Lock()
	defer kubemarkManager.nodeGroupSizeLock.Unlock()
	return kubemarkManager.nodeGroupSize[nodeGroup.Name], nil
}

func (kubemarkManager *KubemarkManager) AddNodesToNodeGroup(nodeGroup *NodeGroup, delta int) error {
	if delta < 0 {
		return fmt.Errorf("Delta has to be positive. Got %d", delta)
	}
	for i := 0; i < delta; i++ {
		if err := kubemarkManager.addNodeToNodeGroup(nodeGroup); err != nil {
			return err
		}
	}
	return nil
}

func (kubemarkManager *KubemarkManager) RemoveNodesFromNodeGroup(nodeGroup *NodeGroup, delta int) error {
	if delta < 0 {
		return fmt.Errorf("Delta has to be positive. Got %d", delta)
	}
	nodes, err := kubemarkManager.GetNodesForNodegroup(nodeGroup)
	if err != nil {
		return err
	}
	for i, node := range nodes {
		if i == delta {
			return nil
		}
		if err := kubemarkManager.DeleteNode(nodeGroup, node); err != nil {
			return err
		}
	}
	return fmt.Errorf("Can't remove %p nodes from %s nodegroup, not enough nodes.")
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

func (kubemarkManager *KubemarkManager) getReplicationControllerByName(name string) *apiv1.ReplicationController {
	for _, obj := range kubemarkManager.externalCluster.rcLister.GetStore().List() {
		rc, ok := obj.(*apiv1.ReplicationController)
		if !ok {
			continue
		}
		if rc.ObjectMeta.Name == name {
			return rc
		}
	}
	return nil
}

func (kubemarkManager *KubemarkManager) getNodeNameForPod(podName string) (string, error) {
	for _, obj := range kubemarkManager.externalCluster.podLister.GetStore().List() {
		pod := obj.(*apiv1.Pod)
		if pod.ObjectMeta.Name == podName {
			return pod.Labels["name"], nil
		}
	}
	return "", fmt.Errorf("pod %s not found", podName)
}

func (kubemarkManager *KubemarkManager) addNodeToNodeGroup(nodeGroup *NodeGroup) error {
	templateCopy, err := api.Scheme.Copy(kubemarkManager.nodeTemplate)
	if err != nil {
		return err
	}
	node := templateCopy.(*apiv1.ReplicationController)
	node.Name = nodeGroup.Name + "-" + fmt.Sprint(rand.Int63())
	node.Labels = map[string]string{nodeGroupLabel: nodeGroup.Name, "name": node.Name, kindLabel: hollowNodeName}
	node.Spec.Template.Labels = node.Labels
	kubemarkManager.nodeGroupSizeLock.Lock()
	defer kubemarkManager.nodeGroupSizeLock.Unlock()
	kubemarkManager.nodeGroupSize[nodeGroup.Name]++
	kubemarkManager.createNodeQueue <- node
	return nil
}

func (kubemarkManager *KubemarkManager) runReplicationControllerCreation(stop <-chan struct{}) {

	for {
		select {
		case node := <-kubemarkManager.createNodeQueue:
			_, err := kubemarkManager.externalCluster.client.CoreV1().ReplicationControllers(node.Namespace).Create(node)
			if err != nil {
				glog.Infof("Failed to create node %s: %v", node.Name, err)
				kubemarkManager.createNodeQueue <- node
			}
		case <-stop:
			return
		}
	}
}

// getNodeTemplate returns the template for hollow node replication controllers
// by looking for an existing hollow node specification. This requires at least
// one kubemark node to be present on startup.
func (kubemarkManager *KubemarkManager) getNodeTemplate() (*apiv1.ReplicationController, error) {
	podName, err := kubemarkManager.kubemarkCluster.getHollowNodeName()
	if err != nil {
		return nil, err
	}
	hollowNodeName, err := kubemarkManager.getNodeNameForPod(podName)
	if err != nil {
		return nil, err
	}
	if hollowNode := kubemarkManager.getReplicationControllerByName(hollowNodeName); hollowNode != nil {
		nodeTemplate := &apiv1.ReplicationController{
			Spec: apiv1.ReplicationControllerSpec{
				Template: hollowNode.Spec.Template,
			},
		}

		nodeTemplate.Spec.Selector = nil
		nodeTemplate.Namespace = namespaceKubemark
		one := int32(1)
		nodeTemplate.Spec.Replicas = &one

		return nodeTemplate, nil
	}
	return nil, fmt.Errorf("can't get hollow node template")
}

func (kubemarkCluster *KubemarkCluster) getHollowNodeName() (string, error) {
	for _, nodeObj := range kubemarkCluster.nodeLister.GetStore().List() {
		node, ok := nodeObj.(*apiv1.Node)
		if !ok {
			continue
		}
		return node.Name, nil
	}
	return "", fmt.Errorf("did not find any hollow nodes in the cluster")
}

func (kubemarkCluster *KubemarkCluster) removeUnneededNodes(oldObj interface{}, newObj interface{}) {
	newNode, ok := newObj.(*apiv1.Node)
	if !ok {
		return
	}
	for _, condition := range newNode.Status.Conditions {
		if condition.Type == apiv1.NodeReady && condition.Status != apiv1.ConditionTrue {
			kubemarkCluster.nodesToDeleteLock.Lock()
			defer kubemarkCluster.nodesToDeleteLock.Unlock()
			if kubemarkCluster.nodesToDelete[newNode.Name] {
				kubemarkCluster.nodesToDelete[newNode.Name] = false
				if err := kubemarkCluster.client.CoreV1().Nodes().Delete(newNode.Name, &metav1.DeleteOptions{}); err != nil {
					glog.Error("Failed to delete node %s from kubemark cluster", newNode.Name)
				}
			}
			return
		}
	}
}

func (kubemarkCluster *KubemarkCluster) markNodeForDeletion(name string) {
	kubemarkCluster.nodesToDeleteLock.Lock()
	defer kubemarkCluster.nodesToDeleteLock.Unlock()
	kubemarkCluster.nodesToDelete[name] = true

}

func newReplicationControllerInformer(kubeClient kube_client.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	rcListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "replicationcontrollers", namespaceKubemark, fields.Everything())
	return cache.NewSharedIndexInformer(rcListWatch, &apiv1.ReplicationController{}, resyncPeriod, nil)
}

func newPodInformer(kubeClient kube_client.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	podListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "pods", namespaceKubemark, fields.Everything())
	return cache.NewSharedIndexInformer(podListWatch, &apiv1.Pod{}, resyncPeriod, nil)
}

func newNodeInformer(kubeClient kube_client.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	nodeListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "nodes", "", fields.Everything())
	informer := cache.NewSharedIndexInformer(nodeListWatch, &apiv1.Node{}, resyncPeriod, nil)
	return informer
}
