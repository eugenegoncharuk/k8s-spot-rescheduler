/*
Copyright 2017 Pusher Ltd.

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
	"context"
	goflag "flag"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pusher/k8s-spot-rescheduler/metrics"
	"github.com/pusher/k8s-spot-rescheduler/nodes"
	"github.com/pusher/k8s-spot-rescheduler/scaler"
	apiv1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	simulator "k8s.io/autoscaler/cluster-autoscaler/simulator"
	autoscaler_drain "k8s.io/autoscaler/cluster-autoscaler/utils/drain"
	kube_utils "k8s.io/autoscaler/cluster-autoscaler/utils/kubernetes"
	kube_client "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	kube_restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	kube_record "k8s.io/client-go/tools/record"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
)

var (
	flags = flag.NewFlagSet(
		`rescheduler: rescheduler --running-in-cluster=true`,
		flag.ExitOnError)

	inCluster = flags.Bool("running-in-cluster", true,
		`Optional, if this controller is running in a kubernetes cluster, use the
		 pod secrets for creating a Kubernetes client.`)

	namespace = flags.String("namespace", "kube-system",
		`Namespace in which k8s-spot-rescheduler is run`)

	contentType = flags.String("kube-api-content-type", "application/vnd.kubernetes.protobuf",
		`Content type of requests sent to apiserver.`)

	housekeepingInterval = flags.Duration("housekeeping-interval", 10*time.Second,
		`How often rescheduler takes actions.`)

	nodeDrainDelay = flags.Duration("node-drain-delay", 10*time.Minute,
		`How long the scheduler should wait between draining nodes.`)

	podEvictionTimeout = flags.Duration("pod-eviction-timeout", 2*time.Minute,
		`How long should the rescheduler attempt to retrieve successful pod
		 evictions for.`)

	maxGracefulTermination = flags.Duration("max-graceful-termination", 2*time.Minute,
		`How long should the rescheduler wait for pods to shutdown gracefully before
		 failing the node drain attempt.`)

	listenAddress = flags.String("listen-address", "localhost:9235",
		`Address to listen on for serving prometheus metrics`)

	spotTaintToBeRemoved = flags.String("spot-node-taint-to-be-removed", "",
		`Spot Tainе to be removed`)

	home = homeDir()

	kubeconfig = flags.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")

	deleteNonReplicatedPods = flags.Bool("delete-non-replicated-pods", false, `Delete non-replicated pods running on on-demand instance. Note that some non-replicated pods will not be rescheduled.`)

	showVersion = flags.Bool("version", false, "Show version information and exit.")
)

func main() {
	flags.AddGoFlagSet(goflag.CommandLine)

	// Log to stderr by default and fix usage message accordingly
	logToStdErr := flags.Lookup("logtostderr")
	logToStdErr.DefValue = "true"
	flags.Set("logtostderr", "true")

	// Add nodes labels as flags
	flags.StringVar(&nodes.OnDemandNodeLabel,
		"on-demand-node-label",
		"kubernetes.io/role=worker",
		`Name of label on nodes to be considered for draining.`)
	flags.StringVar(&nodes.SpotNodeLabel,
		"spot-node-label",
		"kubernetes.io/role=spot-worker",
		`Name of label on nodes to be considered as targets for pods.`)

	flags.IntVar(&nodes.PriorityThreshold, "priority-threshold", 0,
		`Lowest priority to consider while evaluating spot nodes`)

	flags.Parse(os.Args)

	if *showVersion {
		fmt.Printf("k8s-spot-rescheduler %s\n", VERSION)
		os.Exit(0)
	}

	err := validateArgs(nodes.OnDemandNodeLabel, nodes.SpotNodeLabel)
	if err != nil {
		fmt.Printf("Error: %s", err)
		os.Exit(1)
	}

	glog.Infof("Running Rescheduler")

	// Register metrics from metrics.go
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(*listenAddress, nil)
		glog.Fatalf("Failed to start metrics: %v", err)
	}()

	kubeClient, err := createKubeClient(flags, *inCluster)
	if err != nil {
		glog.Fatalf("Failed to create kube client: %v", err)
	}

	recorder := createEventRecorder(kubeClient)

	// This is where the leader election used to be

	run(kubeClient, recorder)
}

func run(kubeClient kube_client.Interface, recorder kube_record.EventRecorder) {

	stopChannel := make(chan struct{})

	// Predicate checker from K8s scheduler works out if a Pod could schedule onto a node
	predicateChecker, err := simulator.NewSchedulerBasedPredicateChecker(kubeClient, stopChannel)
	if err != nil {
		glog.Fatalf("Failed to create predicate checker: %v", err)
	}

	nodeLister := kube_utils.NewReadyNodeLister(kubeClient, stopChannel)
	podDisruptionBudgetLister := kube_utils.NewPodDisruptionBudgetLister(kubeClient, stopChannel)
	unschedulablePodLister := kube_utils.NewUnschedulablePodLister(kubeClient, stopChannel)

	// Set nextDrainTime to now to ensure we start processing straight away.
	nextDrainTime := time.Now()

	for {
		select {
		// Run forever, every housekeepingInterval seconds
		case <-time.After(*housekeepingInterval):
			{
				// Don't do anything if we are waiting for the drain delay timer
				if time.Until(nextDrainTime) > 0 {
					glog.V(2).Infof("Waiting %s for drain delay timer.", time.Until(nextDrainTime).Round(time.Second))
					continue
				}

				// Don't run if pods are unschedulable.
				// Attempt to not make things worse.
				unschedulablePods, err := unschedulablePodLister.List()
				if err != nil {
					glog.Errorf("Failed to get unschedulable pods: %v", err)
				}
				if len(unschedulablePods) > 0 {
					glog.V(2).Info("Waiting for unschedulable pods to be scheduled.")
					continue
				}

				glog.V(3).Info("Starting node processing.")

				// Get all nodes in the cluster
				allNodes, err := nodeLister.List()
				if err != nil {
					glog.Errorf("Failed to list nodes: %v", err)
					continue
				}

				// Build a map of nodeInfo structs.
				// NodeInfo is used to map pods onto nodes and see their available
				// resources.
				nodeMap, err := nodes.NewNodeMap(kubeClient, allNodes)
				if err != nil {
					glog.Errorf("Failed to build node map; %v", err)
					continue
				}

				// Update metrics.
				metrics.UpdateNodesMap(nodeMap)

				// Get PodDisruptionBudgets
				allPDBs, err := podDisruptionBudgetLister.List()
				if err != nil {
					glog.Errorf("Failed to list PDBs: %v", err)
					continue
				}

				// Get onDemand and spot nodeInfoArrays
				// These are sorted when the nodeMap is created.
				onDemandNodeInfos := nodeMap[nodes.OnDemand]
				spotNodeInfos := nodeMap[nodes.Spot]
				spotSnapshot := spotNodeInfos.GetClusterSnapshot()

				// Update spot node metrics
				updateSpotNodeMetrics(spotNodeInfos, allPDBs)

				removeTaintFromAllSpotNodes(kubeClient, spotNodeInfos)

				// No on demand nodes so nothing to do.
				if len(onDemandNodeInfos) < 1 {
					glog.V(2).Info("No nodes to process.")
				}

				// Go through each onDemand node in turn
				// Build a plan to move pods onto other nodes
				// In the case that all can be moved, drain the node
				for _, nodeInfo := range onDemandNodeInfos {

					// Get a list of pods that we would need to move onto other nodes
					allPods, blockingPod, err := autoscaler_drain.GetPodsForDeletionOnNodeDrain(nodeInfo.Pods, allPDBs, *deleteNonReplicatedPods, false, false, nil, 0, time.Now())
					if blockingPod != nil {
						glog.Infof("BlockingPod: %v", err)
					}
					if err != nil {
						glog.Errorf("Failed to get pods for consideration: %v", err)
						continue
					}

					podsForDeletion := make([]*apiv1.Pod, 0)
					for _, pod := range allPods {
						controlledByDaemonSet := false
						for _, owner := range pod.GetOwnerReferences() {
							if *owner.Controller && owner.Kind == "DaemonSet" {
								controlledByDaemonSet = true
								break
							}
						}

						if controlledByDaemonSet {
							glog.V(4).Infof("Ignoring pod %s which is controlled by DaemonSet", podID(pod))
							continue
						}

						//glog.V(4).Infof("Checking namespace")
						//if pod.Namespace == "kube-system" {
						//	glog.V(4).Infof("Ignoring pod %s which is namespace kube-system", podID(pod))
						//	continue
						//}

						podsForDeletion = append(podsForDeletion, pod)
					}

					// Update the number of pods on this node's metrics
					metrics.UpdateNodePodsCount(nodes.OnDemandNodeLabel, nodeInfo.Node.Name, len(podsForDeletion))
					if len(podsForDeletion) < 1 {
						// No pods so should just wait for node to be autoscaled away.
						glog.V(2).Infof("No pods on %s, skipping.", nodeInfo.Node.Name)
						continue
					}

					glog.V(2).Infof("Considering %s for removal", nodeInfo.Node.Name)

					// Checks whether or not a node can be drained
					spotSnapshot.Fork()
					err = canDrainNode(predicateChecker, spotSnapshot, spotNodeInfos, podsForDeletion)
					if err != nil {
						glog.V(2).Infof("Cannot drain node: %v", err)
						spotSnapshot.Revert()
						continue
					}

					// If building plan was successful, can drain node.
					glog.V(2).Infof("All pods on %v can be moved. Will drain node.", nodeInfo.Node.Name)
					// Drain the node - places eviction on each pod moving them in turn.
					err = drainNode(kubeClient, recorder, nodeInfo.Node, podsForDeletion, int(maxGracefulTermination.Seconds()), *podEvictionTimeout)
					if err != nil {
						glog.Errorf("Failed to drain node: %v", err)
					}
					// Add the drain delay to allow system to stabilise
					nextDrainTime = time.Now().Add(*nodeDrainDelay)
					break
				}

				glog.V(3).Info("Finished processing nodes.")
			}
		}
	}
}

func removeTaintFromAllSpotNodes(kubeClient kube_client.Interface, spotNodeInfos nodes.NodeInfoArray) {
	if *spotTaintToBeRemoved == "" {
		return
	}

	for _, spotNodeInfo := range spotNodeInfos {
		for i, taint := range spotNodeInfo.Node.Spec.Taints {
			if taint.Key == *spotTaintToBeRemoved {
				// Delete the element from the array without preserving order
				// https://github.com/golang/go/wiki/SliceTricks#delete-without-preserving-order
				spotNodeInfo.Node.Spec.Taints[i] = spotNodeInfo.Node.Spec.Taints[len(spotNodeInfo.Node.Spec.Taints)-1]
				spotNodeInfo.Node.Spec.Taints = spotNodeInfo.Node.Spec.Taints[:len(spotNodeInfo.Node.Spec.Taints)-1]

				updatedNodeWithoutTaint, err := kubeClient.CoreV1().Nodes().Update(context.TODO(), spotNodeInfo.Node, metav1.UpdateOptions{})
				if err != nil || updatedNodeWithoutTaint == nil {
					glog.Error("Аiled to update node %v after deleting taint: %v", spotNodeInfo.Node.Name, err)
					continue
				}

				glog.Infof("Successfully removed taint on node %v", updatedNodeWithoutTaint.Name)
			}
		}
	}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

// Configure the kube client used to access the api, either from kubeconfig or
//from pod environment if running in the cluster
func createKubeClient(flags *flag.FlagSet, inCluster bool) (kube_client.Interface, error) {
	var config *kube_restclient.Config
	var err error
	if inCluster {
		// Load config from Kubernetes well known location.
		config, err = kube_restclient.InClusterConfig()
	} else {

		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(err.Error())
		}

	}
	if err != nil {
		return nil, fmt.Errorf("error connecting to the client: %v", err)
	}
	config.ContentType = *contentType
	return kube_client.NewForConfigOrDie(config), nil
}

// Create an event broadcaster so that we can call events when we modify the system
func createEventRecorder(client kube_client.Interface) kube_record.EventRecorder {
	eventBroadcaster := kube_record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(client.CoreV1().RESTClient()).Events("")})
	return eventBroadcaster.NewRecorder(runtime.NewScheme(), apiv1.EventSource{Component: "rescheduler"})
}

// Determines if any of the nodes meet the predicates that allow the Pod to be
// scheduled on the node, and returns the node if it finds a suitable one.
// Currently sorts nodes by most requested CPU in an attempt to fill fuller
// nodes first (Attempting to bin pack)
func findSpotNodeForPod(predicateChecker simulator.PredicateChecker, spotSnapshot simulator.ClusterSnapshot, nodes nodes.NodeInfoArray, pod *apiv1.Pod) string {
	for _, nodeInfo := range nodes {
		// Pretend pod isn't scheduled
		pod.Spec.NodeName = ""

		// Check with the schedulers predicates to find a node to schedule on
		err := predicateChecker.CheckPredicates(spotSnapshot, pod, nodeInfo.Node.Name)
		if err == nil {
			return nodeInfo.Node.Name
		} else {
			glog.V(4).Infof("Pod %s can't be rescheduled on node %s: %v", podID(pod), nodeInfo.Node.Name, err)
		}
	}

	return ""
}

// Goes through a list of pods and works out new nodes to place them on.
// Returns an error if any of the pods won't fit onto existing spot nodes.
func canDrainNode(predicateChecker simulator.PredicateChecker, spotSnapshot simulator.ClusterSnapshot, nodes nodes.NodeInfoArray, pods []*apiv1.Pod) error {

	for _, pod := range pods {
		// Works out if a spot node is available for rescheduling
		nodeName := findSpotNodeForPod(predicateChecker, spotSnapshot, nodes, pod)

		// We can't find a Spot node to move this pod to
		// So let's try to evict this pod if it has annotation cluster-autoscaler.kubernetes.io/safe-to-evict = true
		// by just draining the node
		if nodeName == "" {
			if pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] == "false" {
				return fmt.Errorf("Pod %s can't be rescheduled on any existing node [cluster-autoscaler.kubernetes.io/safe-to-evict=false]", podID(pod))
			}
			glog.V(4).Infof("Pod %s can be rescheduled on any available node [cluster-autoscaler.kubernetes.io/safe-to-evict=true]", podID(pod))

			continue
		}

		glog.V(4).Infof("Pod %s can be rescheduled on %s, adding to plan.", podID(pod), nodeName)
		spotSnapshot.AddPod(pod, nodeName)
	}

	return nil
}

// Performs a drain on given node and updates the nextDrainTime variable.
// Returns an error if the drain fails.
func drainNode(kubeClient kube_client.Interface, recorder kube_record.EventRecorder, node *apiv1.Node, pods []*apiv1.Pod, maxGracefulTermination int, podEvictionTimeout time.Duration) error {
	err := scaler.DrainNode(node, pods, kubeClient, recorder, maxGracefulTermination, podEvictionTimeout, scaler.EvictionRetryTime)
	if err != nil {
		metrics.UpdateNodeDrainCount("Failure", node.Name)
		return err
	}

	metrics.UpdateNodeDrainCount("Success", node.Name)
	return nil
}

// Goes through a list of NodeInfos and updates the metrics system with the
// number of pods that the rescheduler understands (So not daemonsets for
// instance) that are on each of the nodes, labelling them as spot nodes.
func updateSpotNodeMetrics(spotNodeInfos nodes.NodeInfoArray, pdbs []*policyv1.PodDisruptionBudget) {
	for _, nodeInfo := range spotNodeInfos {
		// Get a list of pods that are on the node (Only the types considered by the rescheduler)
		podsOnNode, _, err := autoscaler_drain.GetPodsForDeletionOnNodeDrain(nodeInfo.Pods, pdbs, *deleteNonReplicatedPods, false, false, nil, 0, time.Now())
		if err != nil {
			glog.Errorf("Failed to update metrics on spot node %s: %v", nodeInfo.Node.Name, err)
			continue
		}
		metrics.UpdateNodePodsCount(nodes.SpotNodeLabel, nodeInfo.Node.Name, len(podsOnNode))

	}
}

// Returns the pods Namespace/Name as a string
func podID(pod *apiv1.Pod) string {
	return fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
}

// Checks that the node lablels provided as arguments are in fact, sane.
func validateArgs(OnDemandNodeLabel string, SpotNodeLabel string) error {
	if len(strings.Split(OnDemandNodeLabel, "=")) > 2 {
		return fmt.Errorf("the on demand node label is not correctly formatted: expected '<label_name>' or '<label_name>=<label_value>', but got %s", OnDemandNodeLabel)
	}

	if len(strings.Split(SpotNodeLabel, "=")) > 2 {
		return fmt.Errorf("the spot node label is not correctly formatted: expected '<label_name>' or '<label_name>=<label_value>', but got %s", SpotNodeLabel)
	}

	return nil
}
