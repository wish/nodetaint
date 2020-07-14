package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/apps/v1"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	clientretry "k8s.io/client-go/util/retry"
	taintutils "k8s.io/kubernetes/pkg/util/taints"
	"nodetaint/config"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	dsList   = make(map[string]*v1.DaemonSet)
	podStore = make(map[string]*cache.Indexer)

	isHandling = sync.Mutex{}

	UpdateTaintBackoff = wait.Backoff{
		Steps:    5,
		Duration: 100 * time.Millisecond,
		Jitter:   1.0,
	}

	// notReadyTaint is the taint for when a node is not ready
	notReadyTaint = &core_v1.Taint{
		Effect: core_v1.TaintEffectNoSchedule,
	}
)

func setupLogging(logLevel string) {
	// Use log level
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Fatalf("Unknown log level %s: %v", logLevel, err)
	}
	logrus.SetLevel(level)

	// Set the log format to have a reasonable timestamp
	formatter := &logrus.TextFormatter{
		FullTimestamp: true,
	}
	logrus.SetFormatter(formatter)
}

func getClientset() (*kubernetes.Clientset, error) {
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

func checkDSStatus(node *core_v1.Node, opts config.Ops) (bool, error) {
	isHandling.Lock()
	defer isHandling.Unlock()

	// get all the pods scheduled on the nodes
	if len((*podStore[node.Name]).List()) == 0 {
		return false, nil
	}

	readyPods := make(map[string]int)
	for _, obj := range (*podStore[node.Name]).List() {
		if pod, ok := obj.(*core_v1.Pod); ok {
			// only check the pods with specified annotation
			if _, ok := pod.Annotations[opts.DaemonSetAnnotation]; !ok {
				continue
			}

			// only check the ready pods
			podReady := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == core_v1.PodReady && condition.Status == "True" {
					podReady = true
					break
				}
			}
			if !podReady {
				continue
			}

			if pod.OwnerReferences == nil || len(pod.OwnerReferences) == 0 {
				continue
			}
			dsName := pod.OwnerReferences[0].Name
			if _, ok := dsList[dsName]; ok {
				readyPods[dsName] = 1
			}
		}
	}

	if len(readyPods) != len(dsList) {
		return false, nil
	}

	return true, nil
}

// RemoveTaintOffNode is for cleaning up taints temporarily added to node,
// won't fail if target taint doesn't exist or has been removed.
// If passed a node it'll check if there's anything to be done, if taint is not present it won't issue
// any API calls.
func RemoveTaintOffNode(ctx context.Context, c *kubernetes.Clientset, node *core_v1.Node, taints ...*core_v1.Taint) error {
	firstTry := true
	return clientretry.RetryOnConflict(UpdateTaintBackoff, func() error {
		var err error
		var oldNode *core_v1.Node
		// First we try getting node from the API server cache, as it's cheaper. If it fails
		// we get it from etcd to be sure to have fresh data.c
		if firstTry {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, node.Name, meta_v1.GetOptions{ResourceVersion: "0"})
			firstTry = false
		} else {
			oldNode, err = c.CoreV1().Nodes().Get(ctx, node.Name, meta_v1.GetOptions{})
		}
		if err != nil {
			return err
		}

		var newNode *core_v1.Node
		oldNodeCopy := oldNode
		updated := false

		for _, taint := range taints {
			curNewNode, ok, err := taintutils.RemoveTaint(oldNodeCopy, taint)
			if err != nil {
				return fmt.Errorf("failed to remove taint of node")
			}
			updated = updated || ok
			newNode = curNewNode
			oldNodeCopy = curNewNode
		}
		if !updated {
			return nil
		}
		return PatchNodeTaints(c, node.Name, oldNode, newNode, ctx)
	})
}

// PatchNodeTaints patches node's taints.
func PatchNodeTaints(c *kubernetes.Clientset, nodeName string, oldNode *core_v1.Node, newNode *core_v1.Node, ctx context.Context) error {
	oldData, err := json.Marshal(oldNode)
	if err != nil {
		return fmt.Errorf("failed to marshal old node %#v for node %q: %v", oldNode, nodeName, err)
	}

	newTaints := newNode.Spec.Taints
	newNodeClone := oldNode.DeepCopy()
	newNodeClone.Spec.Taints = newTaints
	newData, err := json.Marshal(newNodeClone)
	if err != nil {
		return fmt.Errorf("failed to marshal new node %#v for node %q: %v", newNodeClone, nodeName, err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, core_v1.Node{})
	if err != nil {
		return fmt.Errorf("failed to create patch for node %q: %v", nodeName, err)
	}

	_, err = c.CoreV1().Nodes().Patch(ctx, nodeName, types.StrategicMergePatchType, patchBytes, meta_v1.PatchOptions{})
	logrus.Infof("Applied deletion taint from node %v", nodeName)
	return err
}

// contextForChannel derives a child context from a parent channel.
//
// The derived context's Done channel is closed when the returned cancel function
// is called or when the parent channel is closed, whichever happens first.
//
// Note the caller must *always* call the CancelFunc, otherwise resources may be leaked.
func contextForChannel(parentCh <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		select {
		case <-parentCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func main() {
	opts := &config.Ops{}
	parser := flags.NewParser(opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		// If the error was from the parser, then we can simply return
		// as Parse() prints the error already
		if _, ok := err.(*flags.Error); ok {
			os.Exit(1)
		}
		logrus.Fatalf("Error parsing flags: %v", err)
	}

	notReadyTaint.Key = opts.NodeTaint

	setupLogging(opts.LogLevel)

	clientSet, err := getClientset()
	if err != nil {
		logrus.Fatalf("Failed to create k8s clientset: %v", err)
	}

	// Handle termination
	stopCh := make(chan struct{})
	defer close(stopCh)

	dsHandler := func(ops string, ds *v1.DaemonSet) {
		isHandling.Lock()
		defer isHandling.Unlock()

		if _, ok := ds.Spec.Template.Annotations[opts.DaemonSetAnnotation]; !ok {
			delete(dsList, ds.Name)
			return
		}

		switch ops {
		case "add":
			dsList[ds.Name] = ds
		case "update":
			dsList[ds.Name] = ds
		case "delete":
			delete(dsList, ds.Name)
		}
	}

	podHandler := func(pod *core_v1.Pod, node *core_v1.Node, podStopChan chan struct{}) {
		// check if the node has required taint, if not, ignore this node
		if !taintutils.TaintExists(node.Spec.Taints, notReadyTaint) {
			delete(podStore, node.Name)
			podStopChan <- struct{}{}
			return
		}

		// proceed to valid node only if current pod's condition is ready
		podReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == core_v1.PodReady && condition.Status == "True" {
				podReady = true
				break
			}
		}
		if !podReady {
			return
		}

		// check if all daemonset pods are running
		finished, err := checkDSStatus(node, *opts)
		if err != nil {
			logrus.Errorf("Failed to sync daemonsets: %v", err)
			return
		}
		if !finished {
			return
		}

		ctx, cancel := contextForChannel(podStopChan)
		defer cancel()

		// remove taint to allow other pods to be scheduled on node
		err = RemoveTaintOffNode(ctx, clientSet, node, notReadyTaint)
		if err != nil {
			logrus.Errorf("Failed to remove taint from node: %v", err)
		}
		delete(podStore, node.Name)
		podStopChan <- struct{}{}
	}

	hasSynced := false
	handler := func(node *core_v1.Node) {
		// check if the node has required taint, if not, ignore this node
		if !taintutils.TaintExists(node.Spec.Taints, notReadyTaint) || !hasSynced {
			return
		}
		// there is already a goroutine handling this node
		isHandling.Lock()
		defer isHandling.Unlock()
		if _, ok := podStore[node.Name]; ok {
			return
		}
		logrus.Infof("node:%v has required taint", node.Name)

		go func() {
			// Handle termination
			podStopCh := make(chan struct{})
			defer close(podStopCh)
			// launch pod watcher
			podIndexer, podInformer, err := NewPodInformer(&podHandler, node, podStopCh)
			if err != nil {
				logrus.Fatalf("Error creating pod watcher: %v", err)
			}
			go podInformer.Run(podStopCh)
			isHandling.Lock()
			podStore[node.Name] = &podIndexer
			isHandling.Unlock()
			<-podStopCh
		}()
	}

	c, err := NewController(&handler, &dsHandler)
	if err != nil {
		logrus.Fatalf("Error creating node watcher: %v", err)
	}
	hasSynced = c.Run(stopCh)

	for _, obj := range c.dsIndexer.List() {
		if ds, ok := obj.(*v1.DaemonSet); ok {
			if _, ok := ds.Spec.Template.Annotations[opts.DaemonSetAnnotation]; ok {
				dsList[ds.Name] = ds
			}
		}
	}
	logrus.Infof("Number of required daemonsets is %v", len(dsList))

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)

	select {
	case <-sigterm:
	case <-stopCh:
	}

	logrus.Infof("Received SIGTERM or SIGINT. Shutting down.")
}
