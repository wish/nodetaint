package main

import (
	"fmt"
	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/apps/v1"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"nodetaint/config"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var (
	dsList map[string]v1.DaemonSet
	store cache.Indexer
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

func checkDSStatus(clientSet *kubernetes.Clientset, node *core_v1.Node, opts config.Ops) (bool, error) {
	// get all the pods scheduled on the nodes
	pods, err := clientSet.CoreV1().Pods("").List(meta_v1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%v", node.Name),
	})
	if err != nil {
		return false, err
	}

	if pods == nil || len(pods.Items) == 0 {
		return false, nil
	}

	readyPods := make(map[string]int)
	for _, pod := range pods.Items {
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

		dsName := pod.OwnerReferences[0].Name
		if _, ok := dsList[dsName]; ok {
			readyPods[dsName] = 1
		}
	}

	if len(readyPods) != len(dsList) {
		return false, nil
	}

	return true, nil
}

func removeTaint(clientSet *kubernetes.Clientset, node *core_v1.Node, opts config.Ops) error {
	for idx, taint := range node.Spec.Taints {
		if taint.Key == opts.NodeTaint {
			node.Spec.Taints[idx] = node.Spec.Taints[len(node.Spec.Taints) - 1]
			node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints) - 1]
			break
		}
	}

	_, err := clientSet.CoreV1().Nodes().Update(node)
	if err != nil {
		return fmt.Errorf("error removing taint from node %v: %v", node.Name, err)
	}
	logrus.Infof("Applied deletion taint from node %v", node.Name)

	return nil
}

func checkTaint(node *core_v1.Node, opt config.Ops) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == opt.NodeTaint {
			return true
		}
	}

	return false
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

	setupLogging(opts.LogLevel)

	clientSet, err := getClientset()
	if err != nil {
		logrus.Fatalf("Failed to create k8s clientset: %v", err)
	}

	isHandling := sync.Mutex{}
	// Handle termination
	dsStopCh := make(chan struct{})
	defer close(dsStopCh)
	dsHandler := func() {
		isHandling.Lock()
		defer isHandling.Unlock()

		err := store.Resync()
		if err != nil {
			logrus.Errorf("Failed to sync daemonsets: %v", err)
			return
		}
		for _, obj := range store.List() {
			if ds, ok := obj.(*v1.DaemonSet); ok {
				if _, ok := ds.Annotations[opts.DaemonSetAnnotation]; ok {
					dsList[ds.Name] = *ds
				}
			}
		}
	}
	store, dsInformer, err := NewDSInformer(&dsHandler)
	if err != nil {
		logrus.Fatalf("Error creating daemonsets watcher: %v", err)
	}
	//inti sync daemonsets to local cache
	for _, obj := range store.List() {
		if ds, ok := obj.(*v1.DaemonSet); ok {
			if _, ok := ds.Annotations[opts.DaemonSetAnnotation]; ok {
				dsList[ds.Name] = *ds
			}
		}
	}
	go dsInformer.Run(dsStopCh)

	podHandler := func(pod *core_v1.Pod, node *core_v1.Node) {
		isHandling.Lock()
		defer isHandling.Unlock()

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
		finished, err := checkDSStatus(clientSet, node, *opts)
		if err != nil {
			logrus.Errorf("Failed to sync daemonsets: %v", err)
			return
		}
		if !finished {
			return
		}

		// remove taint to allow other pods to be scheduled on node
		err = removeTaint(clientSet, node, *opts)
		if err != nil {
			logrus.Errorf("Failed to remove taint from node: %v", err)
		}
	}

	// Handle termination
	stopCh := make(chan struct{})
	defer close(stopCh)
	handler := func(node *core_v1.Node) {
		isHandling.Lock()
		defer isHandling.Unlock()

		// check if the node has required taint, if not, ignore this node
		if ok := checkTaint(node, *opts); !ok {
			return
		}

		// launch pod watcher
		podInformer, err := NewPodInformer(&podHandler, node)
		if err != nil {
			logrus.Fatalf("Error creating pod watcher: %v", err)
		}
		podStopCh := make(chan struct{})
		go podInformer.Run(podStopCh)
	}

	c, err := NewController(&handler)
	if err != nil {
		logrus.Fatalf("Error creating node watcher: %v", err)
	}
	c.Run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm

	logrus.Infof("Received SIGTERM or SIGINT. Shutting down.")
}
