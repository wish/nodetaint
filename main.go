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
	"nodetaint/config"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
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
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

// get a list of daemonsets that needs to be running on the node before any other pods can be scheduled
func getDaemonSet(clientSet *kubernetes.Clientset, opts config.Ops) (map[string]v1.DaemonSet, error) {
	ds := make(map[string]v1.DaemonSet)
	dsList, err := clientSet.AppsV1().DaemonSets("").List(meta_v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, item := range dsList.Items {
		if _, ok := item.Annotations[opts.DaemonSetAnnotation]; ok {
			ds[item.Name] = item
		}
	}

	return ds, nil
}

func checkDSStatus(clientSet *kubernetes.Clientset, node *core_v1.Node, opts config.Ops) (bool, error) {
	required, err := getDaemonSet(clientSet, opts)
	if err != nil {
		logrus.Errorf("Failed to get a list of daemonsets: %v", err)
		return false, err
	}

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

	for _, pod := range pods.Items {
		// only check the pods with specified annotation
		if _, ok := pod.Annotations[opts.DaemonSetAnnotation]; !ok {
			continue
		}

		// only check the running pods
		if pod.Status.Phase != core_v1.PodRunning {
			continue
		}

		dsName := getDaemonsetName(pod.Name)
		if _, ok := required[dsName]; ok {
			delete(required, dsName)
		}
	}

	if len(required) != 0 {
		return false, nil
	}

	return true, nil
}

func getDaemonsetName(podName string) string {
	segs := strings.Split(podName, "-")
	segs = segs[:len(segs) - 1]
	return strings.Join(segs, "-")
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

	dur, err := time.ParseDuration(opts.PollPeriod)
	if err != nil {
		logrus.Fatalf("Error parsing poll period: %v", err)
	}

	setupLogging(opts.LogLevel)

	clientSet, err := getClientset()
	if err != nil {
		logrus.Fatalf("Failed to create k8s clientset: %v", err)
	}

	// Handle termination
	stopCh := make(chan struct{})
	defer close(stopCh)

	isHandling := sync.Mutex{}
	handler := func(node *core_v1.Node) {
		isHandling.Lock()
		defer isHandling.Unlock()
		// check if the node has required taint, if not, ignore this node
		if ok := checkTaint(node, *opts); !ok {
			return
		}

		// check if all daemonsets are running
		finished := false
		for !finished {
			time.Sleep(dur)

			finished, err = checkDSStatus(clientSet, node, *opts)
			if err != nil {
				logrus.Errorf("Failed to sync daemonsets: %v", err)
				return
			}
		}

		// remove taint to allow other pods to be scheduled on node
		err := removeTaint(clientSet, node, *opts)
		if err != nil {
			logrus.Errorf("Failed to remove taint from node: %v", err)
			return
		}
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
