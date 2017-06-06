/*
Copyright 2016 Skippbox, Ltd.

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
	"errors"
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/kubeless/kubeless/pkg/spec"
	"github.com/kubeless/kubeless/pkg/utils"

	"k8s.io/apimachinery/pkg/types"
)

const (
	tprName    = "function.k8s.io"
	maxRetries = 5
)

var (
	errVersionOutdated = errors.New("Requested version is outdated in apiserver")
	initRetryWaitTime  = 30 * time.Second
)

// Controller object
type Controller struct {
	logger    *logrus.Entry
	clientset kubernetes.Interface
	Functions map[string]*spec.Function
	queue     workqueue.RateLimitingInterface
	informer  cache.SharedIndexInformer
}

// Config contains k8s client of a controller
type Config struct {
	KubeCli   kubernetes.Interface
	TprClient rest.Interface
}

// New initializes a controller object
func New(cfg Config) *Controller {
	lw := cache.NewListWatchFromClient(cfg.TprClient, "functions", api.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	informer := cache.NewSharedIndexInformer(
		lw,
		&spec.Function{},
		0,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	})

	return &Controller{
		logger:    logrus.WithField("pkg", "controller"),
		clientset: cfg.KubeCli,
		informer:  informer,
		queue:     queue,
	}
}

// Init creates tpr functions.k8s.io
func (c *Controller) Init() {
	c.logger.Infof("Initializing Kubeless controller...")
	for {
		//create TPR if it's not exists
		err := initResource(c.clientset)
		if err == nil {
			break
		}
		c.logger.Errorf("Initialization failed: %v", err)
		c.logger.Infof("Retry in %v...", initRetryWaitTime)
		time.Sleep(initRetryWaitTime)
	}
}

// InstallKubeless deploys kubeless-controller
func (c *Controller) InstallKubeless(ctlNamespace string) {
	c.logger.Infof("Installing Kubeless controller into Kubernetes deployment...")
	err := utils.DeployKubeless(c.clientset, ctlNamespace)
	if err != nil {
		c.logger.Errorf("Kubeless controller installation failed: %v", err)
	} else {
		c.logger.Infof("Kubeless controller installation successful!")
	}
}

// InstallMsgBroker deploys kafka-controller
func (c *Controller) InstallMsgBroker(ctlNamespace string) {
	c.logger.Infof("Installing Message Broker into Kubernetes deployment...")
	err := utils.DeployMsgBroker(c.clientset, ctlNamespace)
	if err != nil {
		c.logger.Errorf("Message Broker installation failed: %v", err)
	} else {
		c.logger.Infof("Message Broker installation successful!")
	}
}

// Run starts the kubeless controller
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.logger.Info("Starting kubeless controller")

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	c.logger.Info("Kubeless controller synced and ready")

	wait.Until(c.runWorker, time.Second, stopCh)
}

// HasSynced is required for the cache.Controller interface.
func (c *Controller) HasSynced() bool {
	return c.informer.HasSynced()
}

// LastSyncResourceVersion is required for the cache.Controller interface.
func (c *Controller) LastSyncResourceVersion() string {
	return c.informer.LastSyncResourceVersion()
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
		// continue looping
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.processItem(key.(string))
	if err == nil {
		// No error, reset the ratelimit counters
		c.queue.Forget(key)
	} else if c.queue.NumRequeues(key) < maxRetries {
		c.logger.Errorf("Error processing %s (will retry): %v", key, err)
		c.queue.AddRateLimited(key)
	} else {
		// err != nil and too many retries
		c.logger.Errorf("Error processing %s (giving up): %v", key, err)
		c.queue.Forget(key)
		utilruntime.HandleError(err)
	}

	return true
}

func (c *Controller) processItem(key string) error {
	c.logger.Infof("Processing change to Function %s", key)

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	obj, exists, err := c.informer.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("Error fetching object with key %s from store: %v", key, err)
	}

	if !exists {
		err := utils.DeleteK8sResources(ns, name, c.clientset)
		if err != nil {
			c.logger.Errorf("Can't delete function: %v", err)
			return err
		}
		c.logger.Infof("Deleted Function %s", key)
		return nil
	}

	funcObj := obj.(*spec.Function)
	fUID := funcObj.Metadata.UID

	err = utils.EnsureK8sResources(ns, name, fUID, &funcObj.Spec, c.clientset)
	if err != nil {
		c.logger.Errorf("Function can not be created/updated: %v", err)
		return err
	}

	c.logger.Infof("Updated Function %s", key)
	return nil
}

func initResource(clientset kubernetes.Interface) error {
	tpr := &v1beta1.ThirdPartyResource{
		ObjectMeta: metav1.ObjectMeta{
			Name: tprName,
		},
		Versions: []v1beta1.APIVersion{
			{Name: "v1"},
		},
		Description: "Kubeless: Serverless framework for Kubernetes",
	}

	_, err := clientset.Extensions().ThirdPartyResources().Create(tpr)
	if err != nil && k8sErrors.IsAlreadyExists(err) {
		_, err = clientset.Extensions().ThirdPartyResources().Update(tpr)
	}
	if err != nil {
		return err
	}

	return nil
}

//RunGC starts garbage collector
func (c *Controller) RunGC(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		<-timer.C
		err := c.garbageCollect()
		if err != nil {
			c.logger.Warningf("Failed to cleanup resources: %v", err)
		}
	}
}

func (c *Controller) garbageCollect() error {
	functionList := spec.FunctionList{}
	tprClient, err := utils.GetTPRClient()
	if err != nil {
		return err
	}
	err = tprClient.Get().Resource("functions").Do().Into(&functionList)
	if err != nil {
		return err
	}

	functionUIDSet := make(map[types.UID]bool)
	for _, f := range functionList.Items {
		functionUIDSet[f.Metadata.UID] = true
	}

	if err = collectServices(c.clientset, functionUIDSet); err != nil {
		return err
	}
	if err = collectDeployment(c.clientset, functionUIDSet); err != nil {
		return err
	}
	if err = collectConfigMap(c.clientset, functionUIDSet); err != nil {
		return err
	}

	return nil
}

func collectServices(c kubernetes.Interface, functionUIDSet map[types.UID]bool) error {
	srvs, err := c.CoreV1().Services(api.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, srv := range srvs.Items {
		if len(srv.OwnerReferences) == 0 {
			continue
		}
		if !functionUIDSet[srv.OwnerReferences[0].UID] {
			err = c.CoreV1().Services(srv.GetNamespace()).Delete(srv.GetName(), nil)
			if err != nil && !utils.IsKubernetesResourceNotFoundError(err) {
				return err
			}
		}
	}

	return nil
}

func collectDeployment(c kubernetes.Interface, functionUIDSet map[types.UID]bool) error {
	ds, err := c.AppsV1beta1().Deployments(api.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, d := range ds.Items {
		if len(d.OwnerReferences) == 0 {
			continue
		}
		if !functionUIDSet[d.OwnerReferences[0].UID] {
			err = c.AppsV1beta1().Deployments(d.GetNamespace()).Delete(d.GetName(), utils.CascadeDeleteOptions(0))
			if err != nil {
				if !utils.IsKubernetesResourceNotFoundError(err) {
					return err
				}
			}
		}
	}

	return nil
}

func collectConfigMap(c kubernetes.Interface, functionUIDSet map[types.UID]bool) error {
	cm, err := c.CoreV1().ConfigMaps(api.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, m := range cm.Items {
		if len(m.OwnerReferences) == 0 {
			continue
		}
		if !functionUIDSet[m.OwnerReferences[0].UID] {
			err = c.CoreV1().ConfigMaps(m.GetNamespace()).Delete(m.GetName(), nil)
			if err != nil {
				if !utils.IsKubernetesResourceNotFoundError(err) {
					return err
				}
			}
		}
	}

	return nil
}
