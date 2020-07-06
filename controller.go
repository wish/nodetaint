package main

import (
	"github.com/sirupsen/logrus"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	listers_v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"time"
)

type Controller struct {
	Clientset *kubernetes.Clientset
	informer  cache.Controller
	indexer   cache.Indexer
	lister    listers_v1.NodeLister
}

func (c *Controller) Run(stopCh <-chan struct{}) {
	go c.informer.Run(stopCh)

	// Wait for the caches to be synced before starting workers
	logrus.Info("Waiting for initial cache sync")
	if ok := cache.WaitForCacheSync(stopCh, c.informer.HasSynced); !ok {
		logrus.Error("Failed to sync informer cache")
		return
	}
	logrus.Info("cache synced")
}

func NewController(handler *func(*core_v1.Node)) (*Controller, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	watchList := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"nodes",
		meta_v1.NamespaceAll,
		fields.Everything(),
	)

	indexer, informer := cache.NewIndexerInformer(
		watchList,
		&core_v1.Node{},
		5*time.Minute, // Do a full update every 5 minutes, making extra sure nothing was missed
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if node, ok := obj.(*core_v1.Node); ok {
					(*handler)(node)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if node, ok := newObj.(*core_v1.Node); ok {
					(*handler)(node)
				}
			},
		},
		cache.Indexers{},
	)

	lister := listers_v1.NewNodeLister(indexer)

	controller := Controller{
		clientset,
		informer,
		indexer,
		lister,
	}

	return &controller, nil
}
