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
	"testing"
	"flag"
	"fmt"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/client/clientset_generated/clientset/fake"
	informers "k8s.io/kubernetes/pkg/client/informers/informers_generated/externalversions"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/stretchr/testify/assert"
)

type testCase struct {
	pods  []*apiv1.Pod
	rcs   []*apiv1.ReplicationController
	nodes []*apiv1.Node
}

func init() {
        flag.Set("alsologtostderr", fmt.Sprintf("%t", true))
        var logLevel string
        flag.StringVar(&logLevel, "logLevel", "4", "test")
        flag.Lookup("v").Value.Set(logLevel)
}

func createTestKubemarkManager(t *testing.T, podList *apiv1.PodList, rcList *apiv1.ReplicationControllerList, nodeList *apiv1.NodeList) *KubemarkManager {
	externalClient := fake.NewSimpleClientset(podList, rcList)
	kubemarkClient := fake.NewSimpleClientset(nodeList)
	externalInformerFactory := informers.NewSharedInformerFactory(externalClient, controller.NoResyncPeriodFunc())
	kubemarkInformerFactory := informers.NewSharedInformerFactory(kubemarkClient, controller.NoResyncPeriodFunc())
	kubemarkManager := &KubemarkManager{
		externalCluster: ExternalCluster{
			client:    externalClient,
			podLister: externalInformerFactory.Core().V1().Pods().Informer(), //externalInformerFactory.InformerFor(&apiv1.Pod{}, newPodInformer),
			rcLister:  externalInformerFactory.InformerFor(&apiv1.ReplicationController{}, newReplicationControllerInformer),
		},
		kubemarkCluster: KubemarkCluster{
			client:     kubemarkClient,
			nodeLister: kubemarkInformerFactory.Core().V1().Nodes(),
		},
	}
	for i, _ := range podList.Items {
		err := kubemarkManager.externalCluster.podLister.GetStore().Add(&podList.Items[i])
		if err != nil {
			t.Fatalf("%#v", err.Error())
		}
	}
	for i, _ := range rcList.Items {
                err := kubemarkManager.externalCluster.rcLister.GetStore().Add(&rcList.Items[i])
                if err != nil {
                        t.Fatalf("%#v", err.Error())
                }
        }
	t.Logf("%#v", len(kubemarkManager.externalCluster.podLister.GetStore().List()))
	return kubemarkManager
}

func TestGetNodesForNodeGroup(t *testing.T) {
	testCases := []struct {
		pods          []apiv1.Pod
		nodeGroup     *NodeGroup
		expectedErr   error
		expectedNodes []string
	}{
		{
			[]apiv1.Pod{},
			&NodeGroup{Name: "ng1"},
			nil,
			[]string{},
		},
		{
			[]apiv1.Pod{
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name:   "pod1",
					Namespace: "kubemark",
					Labels: map[string]string{nodeGroupLabel: "ng1"},
				}},
			},
			&NodeGroup{Name: "ng1"},
			nil,
			[]string{"pod1"},
		},
		{
			[]apiv1.Pod{
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name:"pod1",
					Namespace: "kubemark",
					Labels: map[string]string{nodeGroupLabel: "ng2"},
				}},
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
                                        Name:"pod2",
                                        Namespace: "kubemark",
                                        Labels: map[string]string{nodeGroupLabel: "ng1"},
                                }},
			},
			&NodeGroup{Name: "ng1"},
			nil,
			[]string{"pod2"},
		},
	}
	for _, tc := range testCases {
		t.Logf("%#v", tc.pods)
		km := createTestKubemarkManager(
			t,
			&apiv1.PodList{Items: tc.pods},
			&apiv1.ReplicationControllerList{Items: []apiv1.ReplicationController{}},
			&apiv1.NodeList{Items: []apiv1.Node{}})
		t.Logf("%#v", km.externalCluster.podLister.GetStore().List())
		nodes, err := km.GetNodesForNodegroup(tc.nodeGroup)
		assert.Equal(t, tc.expectedErr, err)
		assert.Equal(t, tc.expectedNodes, nodes)
	}
}

func TestGetNodeGroupForNode(t *testing.T) {
	testCases := []struct {
		pods          []apiv1.Pod
		node string
		expectedErr   error
		expectedNodeGroup string
	}{
		{
			[]apiv1.Pod{},
			"pod1",
			fmt.Errorf("Can't find nodegroup for node pod1"),
			"",
		},
		{
			[]apiv1.Pod{
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name:   "pod1",
					Namespace: "kubemark",
				}},
			},
			"pod1",
			fmt.Errorf("Can't find nodegroup for node pod1. Node exists but does not have the nodeGroupName label."),
			"",
		},
		{
			[]apiv1.Pod{
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name:"pod1",
					Namespace: "kubemark",
					Labels: map[string]string{nodeGroupLabel: "ng2"},
				}},
				apiv1.Pod{ObjectMeta: metav1.ObjectMeta{
                                        Name:"pod2",
                                        Namespace: "kubemark",
                                        Labels: map[string]string{nodeGroupLabel: "ng1"},
                                }},
			},
			"pod2",
			nil,
			"ng1",
		},
	}
	for _, tc := range testCases {
		km := createTestKubemarkManager(
			t,
			&apiv1.PodList{Items: tc.pods},
			&apiv1.ReplicationControllerList{Items: []apiv1.ReplicationController{}},
			&apiv1.NodeList{Items: []apiv1.Node{}})
		t.Logf("%#v", km.externalCluster.podLister.GetStore().List())
		ng, err := km.GetNodeGroupForNode(tc.node)
		assert.Equal(t, tc.expectedErr, err)
		assert.Equal(t, tc.expectedNodeGroup, ng)
	}
}

func TestGetNodeGroupSize(t *testing.T) {
        testCases := []struct {
                rcs          []apiv1.ReplicationController
                nodeGroup     *NodeGroup
                expectedErr   error
                expectedSize  int
        }{
                {
                        []apiv1.ReplicationController{},
                        &NodeGroup{Name: "ng1"},
                        nil,
                        0,
                },
                {
                        []apiv1.ReplicationController{
                                apiv1.ReplicationController{ObjectMeta: metav1.ObjectMeta{
                                        Name:   "rc1",
                                        Namespace: "kubemark",
                                        Labels: map[string]string{nodeGroupLabel: "ng1"},
                                }},
                        },
                        &NodeGroup{Name: "ng1"},
                        nil,
                        1,
                },
                {
                        []apiv1.ReplicationController{
                                apiv1.ReplicationController{ObjectMeta: metav1.ObjectMeta{
                                        Name:"rc1",
                                        Namespace: "kubemark",
                                        Labels: map[string]string{nodeGroupLabel: "ng2"},
                                }},
                                apiv1.ReplicationController{ObjectMeta: metav1.ObjectMeta{
                                        Name:"rc2",
                                        Namespace: "kubemark",
                                        Labels: map[string]string{nodeGroupLabel: "ng1"},
                                }},
                        },
                        &NodeGroup{Name: "ng1"},
                        nil,
                        1,
                },
        }
        for _, tc := range testCases {
                km := createTestKubemarkManager(
                        t,
                        &apiv1.PodList{Items: []apiv1.Pod{}},
			&apiv1.ReplicationControllerList{Items: tc.rcs},
                        &apiv1.NodeList{Items: []apiv1.Node{}})
                t.Logf("%#v", km.externalCluster.podLister.GetStore().List())
                size, err := km.GetNodeGroupSize(tc.nodeGroup)
		assert.Equal(t, tc.expectedSize, size)
		assert.Equal(t, tc.expectedErr, err)
	}
}
