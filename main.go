package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"nodetaint/config"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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

	annotationsMap  = make(map[string]string)
	labelsMap       = make(map[string]string)
	annotationsKeys = make([]string, 0)
	labelsKeys      = make([]string, 0)
)

func filter(ss []string, testFnc func(string) bool) string {
	for _, str := range ss {
		if testFnc(str) {
			return str
		}
	}
	return ""
}

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

func checkDSStatus(node *core_v1.Node) (bool, error) {

	isHandling.Lock()
	defer isHandling.Unlock()

	if _, ok := podStore[node.Name]; !ok {
		return false, nil
	}
	if podStore[node.Name] == nil || len((*podStore[node.Name]).List()) == 0 {
		return false, nil
	}

	// get all the pods scheduled on the nodes
	readyPods := make(map[string]int)
	for _, obj := range (*podStore[node.Name]).List() {
		if pod, ok := obj.(*core_v1.Pod); ok {
			// only check the pods with specified annotation or label
			if filter(annotationsKeys, func(s string) bool {
				_, ok := pod.Annotations[s]
				return ok
			}) == "" {
				if filter(labelsKeys, func(s string) bool {
					_, ok := pod.Labels[s]
					return ok
				}) == "" {
					continue
				}
			}

			if annot := filter(annotationsKeys, func(s string) bool {
				_, ok := pod.Annotations[s]
				return ok
			}); annot != "" {
				if annotationsMap[annot] != "" && annotationsMap[annot] != pod.Annotations[annot] {
					continue
				}
				logrus.Debugf("Checking annotated pod name: %s annotation: %s on node: %s", pod.Name, annot, node.Name)
			}

			if lbl := filter(labelsKeys, func(s string) bool {
				_, ok := pod.Labels[s]
				return ok
			}); lbl != "" {
				if labelsMap[lbl] != "" && labelsMap[lbl] != pod.Labels[lbl] {
					continue
				}
				logrus.Debugf("Checking labeled pod name: %s label: %s on node: %s", pod.Name, lbl, node.Name)
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
				logrus.Debugf("Ready Pod name: %s", pod.Name)
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
		return PatchNodeTaints(ctx, c, node.Name, oldNode, newNode)
	})
}

// PatchNodeTaints patches node's taints.
func PatchNodeTaints(ctx context.Context, c *kubernetes.Clientset, nodeName string, oldNode *core_v1.Node, newNode *core_v1.Node) error {
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

	annotationsList := strings.Split(opts.DaemonSetAnnotation, ",")
	labelsList := strings.Split(opts.DaemonSetLabel, ",")
	for _, annot := range annotationsList {
		kv := strings.Split(annot, ":")
		annotationsMap[strings.TrimSpace(kv[0])] = ""
		if len(kv) == 2 {
			annotationsMap[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	for _, lbl := range labelsList {
		kv := strings.Split(lbl, ":")
		labelsMap[strings.TrimSpace(kv[0])] = ""
		if len(kv) == 2 {
			labelsMap[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}

	for key := range annotationsMap {
		annotationsKeys = append(annotationsKeys, key)
	}

	for key := range labelsMap {
		labelsKeys = append(labelsKeys, key)
	}

	setupLogging(opts.LogLevel)

	clientSet, err := getClientset()
	if err != nil {
		logrus.Fatalf("Failed to create k8s clientset: %v", err)
	}

	// Handle termination
	stopCh := make(chan struct{})
	srv := &http.Server{
		Addr: opts.BindAddr,
	}

	defer srv.Shutdown(context.Background())
	defer close(stopCh)

	dsHandler := func(ops string, ds *v1.DaemonSet) {
		isHandling.Lock()
		defer isHandling.Unlock()

		if filter(annotationsKeys, func(s string) bool {
			_, ok := ds.Spec.Template.Annotations[s]
			return ok
		}) == "" {
			if filter(labelsKeys, func(s string) bool {
				_, ok := ds.Spec.Template.Labels[s]
				return ok
			}) == "" {
				delete(dsList, ds.Name)
				return
			}
		}

		if annot := filter(annotationsKeys, func(s string) bool {
			_, ok := ds.Spec.Template.Annotations[s]
			return ok
		}); annot != "" {
			if annotationsMap[annot] != "" && annotationsMap[annot] != ds.Spec.Template.Annotations[annot] {
				delete(dsList, ds.Name)
				return
			}
		}

		if lbl := filter(labelsKeys, func(s string) bool {
			_, ok := ds.Spec.Template.Labels[s]
			return ok
		}); lbl != "" {
			if labelsMap[lbl] != "" && labelsMap[lbl] != ds.Spec.Template.Labels[lbl] {
				delete(dsList, ds.Name)
				return
			}
		}

		switch ops {
		case "add":
			dsList[ds.Name] = ds
		case "update":
			dsList[ds.Name] = ds
		case "delete":
			delete(dsList, ds.Name)
		}
		logrus.Debugf("Number of required daemonsets is %v", len(dsList))
		for dsName := range dsList {
			logrus.Debugf("Required daemonset name: %s", dsName)
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
		finished, err := checkDSStatus(node)
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
			if annot := filter(annotationsKeys, func(s string) bool {
				_, ok := ds.Spec.Template.Annotations[s]
				return ok
			}); annot != "" {
				if annotationsMap[annot] == "" {
					dsList[ds.Name] = ds
				}
				if annotationsMap[annot] != "" && annotationsMap[annot] == ds.Spec.Template.Annotations[annot] {
					dsList[ds.Name] = ds
				}
			}
			if lbl := filter(labelsKeys, func(s string) bool {
				_, ok := ds.Spec.Template.Labels[s]
				return ok
			}); lbl != "" {
				if labelsMap[lbl] == "" {
					dsList[ds.Name] = ds
				}
				if labelsMap[lbl] != "" && labelsMap[lbl] == ds.Spec.Template.Labels[lbl] {
					dsList[ds.Name] = ds
				}
			}
		}
	}
	logrus.Infof("Number of required daemonsets is %v", len(dsList))
	for dsName := range dsList {
		logrus.Debugf("Required daemonset name: %s", dsName)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK\n")
	})
	http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK\n")
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logrus.Errorf("Error serving HTTP at %v: %v", opts.BindAddr, err)
		}
	}()

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)

	select {
	case <-sigterm:
	case <-stopCh:
	}

	logrus.Infof("Received SIGTERM or SIGINT. Shutting down.")
}
