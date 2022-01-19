package main

import (
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/apps/v1"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	taintutils "k8s.io/kubernetes/pkg/util/taints"
)

type Controller struct {
	clientSet     *kubernetes.Clientset
	nodesInformer cache.Controller
	dsInformer    cache.Controller
	dsIndexer     cache.Indexer
}

func (c *Controller) Run(stopCh <-chan struct{}) bool {
	go c.nodesInformer.Run(stopCh)
	go c.dsInformer.Run(stopCh)

	// Wait for the caches to be synced before starting workers
	logrus.Info("Waiting for initial cache sync")
	if ok := cache.WaitForCacheSync(stopCh, c.nodesInformer.HasSynced, c.dsInformer.HasSynced); !ok {
		logrus.Error("Failed to sync informer cache")
		return false
	}
	logrus.Info("cache synced")
	return true
}

func NewController(handler *func(*core_v1.Node), dsHandler *func(ops string, ds *v1.DaemonSet)) (*Controller, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	nodesWatchList := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"nodes",
		meta_v1.NamespaceAll,
		fields.Everything(),
	)

	_, nodesInformer := cache.NewIndexerInformer(
		nodesWatchList,
		&core_v1.Node{},
		5*time.Minute, // Do a full update every 5 minutes, making extra sure nothing was missed
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if node, ok := obj.(*core_v1.Node); ok && taintutils.TaintExists(node.Spec.Taints, notReadyTaint) {
					(*handler)(node)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if node, ok := newObj.(*core_v1.Node); ok && taintutils.TaintExists(node.Spec.Taints, notReadyTaint) {
					(*handler)(node)
				}
			},
		},
		cache.Indexers{},
	)

	dsWatchList := cache.NewListWatchFromClient(
		clientset.AppsV1().RESTClient(),
		"daemonsets",
		meta_v1.NamespaceAll,
		fields.Everything(),
	)

	dsIndexer, dsInformer := cache.NewIndexerInformer(
		dsWatchList,
		&v1.DaemonSet{},
		5*time.Minute, // Do a full update every 5 minutes, making extra sure nothing was missed
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if ds, ok := obj.(*v1.DaemonSet); ok {
					(*dsHandler)("add", ds)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if ds, ok := newObj.(*v1.DaemonSet); ok {
					(*dsHandler)("update", ds)
				}
			},
			DeleteFunc: func(obj interface{}) {
				if ds, ok := obj.(*v1.DaemonSet); ok {
					(*dsHandler)("delete", ds)
				}
			},
		},
		cache.Indexers{},
	)

	controller := Controller{
		clientset,
		nodesInformer,
		dsInformer,
		dsIndexer,
	}

	return &controller, nil
}

func NewPodInformer(handler *func(pod *core_v1.Pod, node *core_v1.Node, podStopChan chan struct{}), node *core_v1.Node, podStopChan chan struct{}) (cache.Indexer, cache.Controller, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	watchList := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		meta_v1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", node.Name),
	)

	indexer, informer := cache.NewIndexerInformer(
		watchList,
		&core_v1.Pod{},
		5*time.Minute,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if pod, ok := obj.(*core_v1.Pod); ok {
					(*handler)(pod, node, podStopChan)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if pod, ok := newObj.(*core_v1.Pod); ok {
					(*handler)(pod, node, podStopChan)
				}
			},
		},
		cache.Indexers{},
	)

	return indexer, informer, nil
}
