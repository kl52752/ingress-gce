/*
Copyright 2021 The Kubernetes Authors.

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

package l4netlb

import (
	"fmt"
	"sync"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	v1 "k8s.io/api/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/cloud-provider/service/helpers"
	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/controller/translator"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/ingress-gce/pkg/utils/patch"
	"k8s.io/klog"
)

type L4NetLbController struct {
	ctx           *context.ControllerContext
	svcQueue      utils.TaskQueue
	numWorkers    int
	serviceLister cache.Indexer
	nodeLister    listers.NodeLister
	stopCh        chan struct{}

	translator  *translator.Translator
	backendPool *backends.Backends
	namer       namer.L4ResourcesNamer
	// enqueueTracker tracks the latest time an update was enqueued
	enqueueTracker utils.TimeTracker
	// syncTracker tracks the latest time an enqueued service was synced
	syncTracker         utils.TimeTracker
	sharedResourcesLock sync.Mutex

	instancePool instances.NodePool
	igLinker     backends.Linker
}

// NewL4NetLbController creates a controller for l4 external loadbalancer.
func NewL4NetLbController(
	ctx *context.ControllerContext,
	stopCh chan struct{}) *L4NetLbController {
	if ctx.NumL4Workers <= 0 {
		klog.Infof("L4 Worker count has not been set, setting to 1")
		ctx.NumL4Workers = 1
	}

	backendPool := backends.NewPool(ctx.Cloud, ctx.L4Namer)
	instancePool := instances.NewNodePool(ctx.Cloud, ctx.ClusterNamer, ctx, utils.GetBasePath(ctx.Cloud))
	l4netLb := &L4NetLbController{
		ctx:           ctx,
		numWorkers:    ctx.NumL4Workers,
		serviceLister: ctx.ServiceInformer.GetIndexer(),
		nodeLister:    listers.NewNodeLister(ctx.NodeInformer.GetIndexer()),
		stopCh:        stopCh,
		translator:    translator.NewTranslator(ctx),
		backendPool:   backendPool,
		namer:         ctx.L4Namer,
		instancePool:  instancePool,
		igLinker:      backends.NewRegionalInstanceGroupLinker(instancePool, backendPool),
	}
	l4netLb.svcQueue = utils.NewPeriodicTaskQueueWithMultipleWorkers("l4netLb", "services", l4netLb.numWorkers, l4netLb.sync)

	ctx.ServiceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addSvc := obj.(*v1.Service)
			svcKey := utils.ServiceKeyFunc(addSvc.Namespace, addSvc.Name)
			needsNetLB, svcType := annotations.WantsNewL4NetLb(addSvc)
			if needsNetLB {
				klog.V(3).Infof("NetLB Service %s added, enqueuing", svcKey)
				l4netLb.ctx.Recorder(addSvc.Namespace).Eventf(addSvc, v1.EventTypeNormal, "ADD", svcKey)
				l4netLb.svcQueue.Enqueue(addSvc)
				l4netLb.enqueueTracker.Track()
			} else {
				klog.V(4).Infof("L4NetLb Ignoring add for non-lb service %s based on %v", svcKey, svcType)
			}
		},
		// Deletes will be handled in the Update when the deletion timestamp is set.
		UpdateFunc: func(old, cur interface{}) {
			//TODO(kl52752) add implementation
		},
	})
	klog.Infof("l4NetLbController started")
	ctx.AddHealthCheck("service-controller health", l4netLb.checkHealth)
	return l4netLb
}

func (lc *L4NetLbController) checkHealth() error {
	//TODO(kl52752) add implementation
	return nil
}

//Init inits instance Pool
func (lc *L4NetLbController) Init() {
	lc.instancePool.Init(lc.translator)
}

// Run starts the loadbalancer controller.
func (lc *L4NetLbController) Run() {
	klog.Infof("Starting l4NetLbController")
	lc.svcQueue.Run()

	<-lc.stopCh
	klog.Infof("Shutting down l4NetLbController")
}

func (lc *L4NetLbController) shutdown() {
	klog.Infof("Shutting down l4NetLbController")
	lc.svcQueue.Shutdown()
}

func (lc *L4NetLbController) sync(key string) error {
	lc.syncTracker.Track()
	svc, exists, err := lc.ctx.Services().GetByKey(key)
	if err != nil {
		return fmt.Errorf("Failed to lookup NetLb service for key %s : %w", key, err)
	}
	if !exists || svc == nil {
		klog.V(3).Infof("Ignoring delete of service %s not managed by L4NetLbController", key)
		return nil
	}
	var result *loadbalancers.SyncResultNetLb
	if wantsNetLb, _ := annotations.WantsNewL4NetLb(svc); wantsNetLb {
		result = lc.processServiceCreateOrUpdate(key, svc)
		if result == nil {
			// result will be nil if the service was ignored(due to presence of service controller finalizer).
			return nil
		}
		return result.Error
	}
	klog.V(3).Infof("Ignoring sync of service %s, neither delete nor ensure needed.", key)
	return nil
}

// processServiceCreateOrUpdate ensures load balancer resources for the given service, as needed.
// Returns an error if processing the service update failed.
func (lc *L4NetLbController) processServiceCreateOrUpdate(key string, service *v1.Service) *loadbalancers.SyncResultNetLb {
	l4netlb := loadbalancers.NewL4NetLb(service, lc.ctx.Cloud, meta.Regional, lc.namer, lc.ctx.Recorder(service.Namespace), &lc.sharedResourcesLock)
	if !lc.shouldProcessService(service, l4netlb) {
		return nil
	}

	// #TODO(kl52752) Add ensure finalizer for NetLB
	nodeNames, err := utils.GetReadyNodeNames(lc.nodeLister)
	if err != nil {
		return &loadbalancers.SyncResultNetLb{Error: err}
	}

	if err := lc.ensureInstanceGroup(service, nodeNames); err != nil {
		lc.ctx.Recorder(service.Namespace).Eventf(service, v1.EventTypeWarning, "SyncInstanceagroupsFailed",
			"Error syncing instance group: %v", err)
		return &loadbalancers.SyncResultNetLb{Error: err}
	}

	// Use the same function for both create and updates. If controller crashes and restarts,
	// all existing services will show up as Service Adds.
	syncResult := l4netlb.EnsureNetLoadBalancer(nodeNames, service)
	if syncResult.Error != nil {
		lc.ctx.Recorder(service.Namespace).Eventf(service, v1.EventTypeWarning, "SyncLoadBalancerFailed",
			"Error syncing l4 net load balancer: %v", syncResult.Error)
		return syncResult
	}

	if err = lc.linkIgToBackendService(l4netlb.ServicePort); err != nil {
		lc.ctx.Recorder(service.Namespace).Eventf(service, v1.EventTypeWarning, "SyncLoadBalancerFailed",
			"Error linking instance groups to backend service status: %v", err)
		syncResult.Error = err
		return syncResult
	}

	err = lc.updateServiceStatus(service, syncResult.Status)
	if err != nil {
		lc.ctx.Recorder(service.Namespace).Eventf(service, v1.EventTypeWarning, "SyncLoadBalancerFailed",
			"Error updating l4 net load balancer status: %v", err)
		syncResult.Error = err
		return syncResult
	}
	lc.ctx.Recorder(service.Namespace).Eventf(service, v1.EventTypeNormal, "SyncLoadBalancerSuccessful",
		"Successfully ensured l4 net load balancer resources")
	return nil
}

func (lc *L4NetLbController) linkIgToBackendService(port utils.ServicePort) error {
	zones, err := lc.translator.ListZones(utils.AllNodesPredicate)
	if err != nil {
		return err
	}
	var groupKeys []backends.GroupKey
	for _, zone := range zones {
		groupKeys = append(groupKeys, backends.GroupKey{Zone: zone})
	}
	return lc.igLinker.Link(port, groupKeys)
}

// shouldProcessService returns if the given LoadBalancer service should be processed by this controller.
func (lc *L4NetLbController) shouldProcessService(service *v1.Service, l4 *loadbalancers.L4NetLb) bool {
	//TODO(kl52752) add implementation
	return true
}

func (lc *L4NetLbController) ensureInstanceGroup(service *v1.Service, nodeNames []string) error {
	_, _, nodePorts, _ := utils.GetPortsAndProtocol(service.Spec.Ports)
	_, err := lc.instancePool.EnsureInstanceGroupsAndPorts(lc.ctx.ClusterNamer.InstanceGroup(), nodePorts)
	if err != nil {
		return err
	}
	return lc.instancePool.Sync(nodeNames)
}

func (lc *L4NetLbController) updateServiceStatus(svc *v1.Service, newStatus *v1.LoadBalancerStatus) error {
	if helpers.LoadBalancerStatusEqual(&svc.Status.LoadBalancer, newStatus) {
		return nil
	}
	return patch.PatchServiceLoadBalancerStatus(lc.ctx.KubeClient.CoreV1(), svc, *newStatus)
}
