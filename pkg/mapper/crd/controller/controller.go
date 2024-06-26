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

package controller

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/labels"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/sirupsen/logrus"

	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/arn"
	ramauthenticatorv1alpha1 "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/apis/ramauthenticator/v1alpha1"
	clientset "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/clientset/versioned"
	ramscheme "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/clientset/versioned/scheme"
	informers "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/informers/externalversions/ramauthenticator/v1alpha1"
	listers "github.com/AliyunContainerService/ack-ram-authenticator/pkg/mapper/crd/generated/listers/ramauthenticator/v1alpha1"
)

const (
	// controllerAgentName is the name the controller appears as in the Event logger
	controllerAgentName = "ack-ram-authenticator"

	// SuccessSynced is used as part of the Event 'reason' when a Identity is synced
	SuccessSynced = "Synced"

	// IdentitySynced is the `message` when an Identity is synced
	IdentitySynced = "Identity synced successfully"
)

// Controller implements the logic for getting and mutating RAMIdentityMappings
type Controller struct {
	// kubeclientset implements the Kubernetes clientset, used for the event recorder
	kubeclientset kubernetes.Interface

	// ramclientset implements the RAMIdentityMapping clientset, used for getting identities
	ramclientset clientset.Interface
	// ramMappingLister implements the lister interface for RAMIdentityMappings
	ramMappingLister listers.RAMIdentityMappingLister
	// ramMappingsSynced is a function to get if the informers have synced
	ramMappingsSynced cache.InformerSynced
	// ramMappingsIndex is a custom indexer which allows for indexing on canonical arns
	ramMappingsIndex cache.Indexer

	// workqueue implements a FIFO queue used for processing items
	workqueue workqueue.RateLimitingInterface
	// recorder implements the Event recorder interface for logging events.
	recorder record.EventRecorder

	// WildMappingCache is a lru cache for custom user defined mapping with wild character
	WildMappingCache     atomic.Value
	WildMappingCacheLock sync.Mutex
}

type WildMappingMap map[string]*ramauthenticatorv1alpha1.RAMIdentityMapping

// New will initialize a default controller object
func New(
	kubeclientset kubernetes.Interface,
	ramclientset clientset.Interface,
	ramMappingInformer informers.RAMIdentityMappingInformer) *Controller {

	// Initialize the Scheme
	utilruntime.Must(ramscheme.AddToScheme(scheme.Scheme))

	// Setup event broadcaster
	logrus.Info("creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:        kubeclientset,
		ramclientset:         ramclientset,
		ramMappingLister:     ramMappingInformer.Lister(),
		ramMappingsSynced:    ramMappingInformer.Informer().HasSynced,
		workqueue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "RAMIdentityMappings"),
		recorder:             recorder,
		WildMappingCache:     atomic.Value{},
		WildMappingCacheLock: sync.Mutex{},
	}

	logrus.Info("setting up event handlers")
	// adding event handlers to load the informer and convert roles into
	// canonical ARNs, we're ignoring deletes because all checks for roles happen
	// using the in-memory cache which is updated automatically on deletes no further
	// actions are necessary
	ramMappingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueRAMIdentityMapping,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueRAMIdentityMapping(new)
		},
		DeleteFunc: func(obj interface{}) {
			controller.removeWildMapping()
		},
	})

	err := ramMappingInformer.Informer().GetIndexer().AddIndexers(cache.Indexers{
		"canonicalARN": IndexRAMIdentityMappingByCanonicalArn,
	})
	if err != nil {
		logrus.WithError(err).Fatal("error adding index")
	}

	controller.ramMappingsIndex = ramMappingInformer.Informer().GetIndexer()

	return controller
}

// Run will implement the loop for processing items
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	logrus.Info("starting ack ram authenticator controller")

	logrus.Info("waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.ramMappingsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	c.WildMappingCache.Store(WildMappingMap{})
	logrus.Info("starting workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	logrus.Info("started workers")
	<-stopCh
	logrus.Info("shutting down workers")

	return nil
}

// runWorker loops over each item looking for boolean returns
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will process each item off the queue
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.syncHandler(key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing %s : %s, requeuing", key, err.Error())
		}

		c.workqueue.Forget(obj)
		logrus.Infof("successfully synced %s", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) removeWildMapping() {
	allMapping, err := c.ramMappingLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	c.WildMappingCacheLock.Lock()
	defer c.WildMappingCacheLock.Unlock()

	wildCacheMap := c.WildMappingCache.Load().(WildMappingMap)
	// Copy because we cannot write to storageMap without a race
	wildCacheMapp2 := make(WildMappingMap)
	for _, mapping := range allMapping {
		if !hasWildDedinedInMappingArn(mapping.Status.CanonicalARN) {
			continue
		}
		if _, ok := wildCacheMap[mapping.Name]; ok {
			wildCacheMapp2[mapping.Name] = wildCacheMap[mapping.Name]
		}
	}
	c.WildMappingCache.Store(wildCacheMapp2)
	for key := range wildCacheMap {
		if _, ok := wildCacheMapp2[key]; !ok {
			logrus.Infof("mapping instance %v has removed from WildMappingCache", key)
		}
	}
}

func (c *Controller) syncHandler(key string) (err error) {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key %s", key))
		return nil
	}
	logrus.Infof("syncHandler with key %s", name)

	ramIdentityMapping, err := c.ramMappingLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("ram identity mapping %s no longer exists", key))
			return nil
		}
		return err
	}

	// Process items
	if ramIdentityMapping.Spec.ARN != "" {
		ramIdentityMappingCopy := ramIdentityMapping.DeepCopy()

		canonicalizedARN, err := arn.Canonicalize(strings.ToLower(ramIdentityMapping.Spec.ARN))
		if err != nil {
			logrus.Infof("canonicalizedARN err %v", err)
			return err
		}

		ramIdentityMappingCopy.Status.CanonicalARN = canonicalizedARN
		_, err = c.ramclientset.RamauthenticatorV1alpha1().RAMIdentityMappings().UpdateStatus(context.TODO(), ramIdentityMappingCopy, metav1.UpdateOptions{})
		if err != nil {
			logrus.Infof("syncHandler failed to udpate status, err %v, copy %v", err, ramIdentityMappingCopy)
			return err
		}
		//check if need refresh wild mapping cache store
		if hasWildDedinedInMappingArn(canonicalizedARN) {
			c.WildMappingCacheLock.Lock()
			defer c.WildMappingCacheLock.Unlock()
			//refresh cache with created or updated ram identity mapping
			wildCacheMap := c.WildMappingCache.Load().(WildMappingMap)
			storageMap2 := wildCacheMap.clone()
			storageMap2[ramIdentityMappingCopy.Name] = ramIdentityMappingCopy
			c.WildMappingCache.Store(storageMap2)
			logrus.Infof("syncHandler has add a wild mapping with arn %s into cache map", canonicalizedARN)
		}
	}

	c.recorder.Event(ramIdentityMapping, corev1.EventTypeNormal, SuccessSynced, IdentitySynced)
	return nil
}

// enqueueRAMIdentityMapping will pull in a new RAMIdentityMapping and update it
func (c *Controller) enqueueRAMIdentityMapping(obj interface{}) {
	var key string
	var err error

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (in WildMappingMap) clone() WildMappingMap {
	if in == nil {
		return nil
	}
	out := make(WildMappingMap, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

//wild character including '?' and '*'
//? matches exactly one occurrence of any character.
//* matches arbitrary many (including zero) occurrences of any character.
func hasWildDedinedInMappingArn(arn string) bool {
	if strings.ContainsAny(arn, "?*") {
		return true
	}
	return false
}

// IndexRAMIdentityMappingByCanonicalArn collects the information for the additional indexer used for finding identities
func IndexRAMIdentityMappingByCanonicalArn(obj interface{}) ([]string, error) {
	ramIdentity, ok := obj.(*ramauthenticatorv1alpha1.RAMIdentityMapping)
	if !ok {
		return []string{}, nil
	}

	canonicalArnStr := ramIdentity.Status.CanonicalARN
	if canonicalArnStr == "" {
		return []string{}, nil
	}

	return []string{canonicalArnStr}, nil
}
