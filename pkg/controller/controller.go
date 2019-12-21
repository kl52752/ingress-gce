/*
Copyright 2015 The Kubernetes Authors.

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
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	unversionedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/ingress-gce/pkg/annotations"
	backendconfigv1beta1 "k8s.io/ingress-gce/pkg/apis/backendconfig/v1beta1"
	frontendconfigv1beta1 "k8s.io/ingress-gce/pkg/apis/frontendconfig/v1beta1"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/common/operator"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/controller/translator"
	"k8s.io/ingress-gce/pkg/flags"
	"k8s.io/ingress-gce/pkg/frontendconfig"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/metrics"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	ingsync "k8s.io/ingress-gce/pkg/sync"
	"k8s.io/ingress-gce/pkg/tls"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/common"
	"k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/klog"
)

// LoadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type LoadBalancerController struct {
	ctx *context.ControllerContext

	nodeLister cache.Indexer
	nodes      *NodeController

	// TODO: Watch secrets
	ingQueue   utils.TaskQueue
	Translator *translator.Translator
	stopCh     chan struct{}
	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	// tlsLoader loads secrets from the Kubernetes apiserver for Ingresses.
	tlsLoader tls.TlsLoader
	// hasSynced returns true if all associated sub-controllers have synced.
	// Abstracted into a func for testing.
	hasSynced func() bool

	// Resource pools.
	instancePool instances.NodePool
	l7Pool       loadbalancers.LoadBalancerPool

	// syncer implementation for backends
	backendSyncer backends.Syncer
	// linker implementations for backends
	negLinker backends.Linker
	igLinker  backends.Linker

	// Ingress sync + GC implementation
	ingSyncer ingsync.Syncer

	// Ingress usage metrics.
	metrics metrics.IngressMetricsCollector
}

// NewLoadBalancerController creates a controller for gce loadbalancers.
func NewLoadBalancerController(
	ctx *context.ControllerContext,
	stopCh chan struct{}) *LoadBalancerController {

	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(klog.Infof)
	broadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{
		Interface: ctx.KubeClient.CoreV1().Events(""),
	})

	healthChecker := healthchecks.NewHealthChecker(ctx.Cloud, ctx.HealthCheckPath, ctx.DefaultBackendHealthCheckPath, ctx.DefaultBackendSvcPort.ID.Service)
	instancePool := instances.NewNodePool(ctx.Cloud, ctx.ClusterNamer)
	backendPool := backends.NewPool(ctx.Cloud, ctx.ClusterNamer)

	lbc := LoadBalancerController{
		ctx:           ctx,
		nodeLister:    ctx.NodeInformer.GetIndexer(),
		Translator:    translator.NewTranslator(ctx),
		tlsLoader:     &tls.TLSCertsFromSecretsLoader{Client: ctx.KubeClient},
		stopCh:        stopCh,
		hasSynced:     ctx.HasSynced,
		nodes:         NewNodeController(ctx, instancePool),
		instancePool:  instancePool,
		l7Pool:        loadbalancers.NewLoadBalancerPool(ctx.Cloud, ctx.ClusterNamer, ctx, namer.NewFrontendNamerFactory(ctx.ClusterNamer, ctx.KubeSystemUID)),
		backendSyncer: backends.NewBackendSyncer(backendPool, healthChecker, ctx.Cloud),
		negLinker:     backends.NewNEGLinker(backendPool, negtypes.NewAdapter(ctx.Cloud), ctx.Cloud),
		igLinker:      backends.NewInstanceGroupLinker(instancePool, backendPool),
		metrics:       ctx.ControllerMetrics,
	}
	lbc.ingSyncer = ingsync.NewIngressSyncer(&lbc)

	lbc.ingQueue = utils.NewPeriodicTaskQueue("ingress", "ingresses", lbc.sync)

	// Ingress event handlers.
	ctx.IngressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*v1beta1.Ingress)
			if !utils.IsGLBCIngress(addIng) {
				klog.V(4).Infof("Ignoring add for ingress %v based on annotation %v", common.NamespacedName(addIng), annotations.IngressClassKey)
				return
			}

			klog.V(3).Infof("Ingress %v added, enqueuing", common.NamespacedName(addIng))
			lbc.ctx.Recorder(addIng.Namespace).Eventf(addIng, apiv1.EventTypeNormal, "ADD", common.NamespacedName(addIng))
			lbc.ingQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			delIng := obj.(*v1beta1.Ingress)
			if delIng == nil {
				klog.Errorf("Invalid object type: %T", obj)
				return
			}
			if delIng.ObjectMeta.DeletionTimestamp != nil {
				klog.V(2).Infof("Ignoring delete event for Ingress %v, deletion will be handled via the finalizer", common.NamespacedName(delIng))
				return
			}

			if !utils.IsGLBCIngress(delIng) {
				klog.V(4).Infof("Ignoring delete for ingress %v based on annotation %v", common.NamespacedName(delIng), annotations.IngressClassKey)
				return
			}

			klog.V(3).Infof("Ingress %v deleted, enqueueing", common.NamespacedName(delIng))
			lbc.ingQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			curIng := cur.(*v1beta1.Ingress)
			if !utils.IsGLBCIngress(curIng) {
				oldIng := old.(*v1beta1.Ingress)
				// If ingress was GLBC Ingress, we need to track ingress class change
				// and run GC to delete LB resources.
				if utils.IsGLBCIngress(oldIng) {
					klog.V(4).Infof("Ingress %v class was changed, enqueuing", common.NamespacedName(curIng))
					lbc.ingQueue.Enqueue(cur)
					return
				}
				return
			}
			if reflect.DeepEqual(old, cur) {
				klog.V(3).Infof("Periodic enqueueing of %v", common.NamespacedName(curIng))
			} else {
				klog.V(3).Infof("Ingress %v changed, enqueuing", common.NamespacedName(curIng))
			}

			lbc.ingQueue.Enqueue(cur)
		},
	})

	// Service event handlers.
	ctx.ServiceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*apiv1.Service)
			ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesService(svc).AsList()
			lbc.ingQueue.Enqueue(convert(ings)...)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				svc := cur.(*apiv1.Service)
				ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesService(svc).AsList()
				lbc.ingQueue.Enqueue(convert(ings)...)
			}
		},
		// Ingress deletes matter, service deletes don't.
	})

	// BackendConfig event handlers.
	ctx.BackendConfigInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			beConfig := obj.(*backendconfigv1beta1.BackendConfig)
			ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesBackendConfig(beConfig, operator.Services(ctx.Services().List())).AsList()
			lbc.ingQueue.Enqueue(convert(ings)...)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				beConfig := cur.(*backendconfigv1beta1.BackendConfig)
				ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesBackendConfig(beConfig, operator.Services(ctx.Services().List())).AsList()
				lbc.ingQueue.Enqueue(convert(ings)...)
			}
		},
		DeleteFunc: func(obj interface{}) {
			var beConfig *backendconfigv1beta1.BackendConfig
			var ok, beOk bool
			beConfig, ok = obj.(*backendconfigv1beta1.BackendConfig)
			if !ok {
				// This can happen if the watch is closed and misses the delete event
				state, stateOk := obj.(cache.DeletedFinalStateUnknown)
				if !stateOk {
					klog.Errorf("Wanted cache.DeleteFinalStateUnknown of backendconfig obj, got: %+v", obj)
					return
				}

				beConfig, beOk = state.Obj.(*backendconfigv1beta1.BackendConfig)
				if !beOk {
					klog.Errorf("Wanted backendconfig obj, got %+v", state.Obj)
					return
				}
			}

			ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesBackendConfig(beConfig, operator.Services(ctx.Services().List())).AsList()
			lbc.ingQueue.Enqueue(convert(ings)...)
		},
	})

	// FrontendConfig event handlers.
	if ctx.FrontendConfigEnabled {
		ctx.FrontendConfigInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				feConfig := obj.(*frontendconfigv1beta1.FrontendConfig)
				ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesFrontendConfig(feConfig).AsList()
				lbc.ingQueue.Enqueue(convert(ings)...)

			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					feConfig := cur.(*frontendconfigv1beta1.FrontendConfig)
					ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesFrontendConfig(feConfig).AsList()
					lbc.ingQueue.Enqueue(convert(ings)...)
				}
			},
			DeleteFunc: func(obj interface{}) {
				var feConfig *frontendconfigv1beta1.FrontendConfig
				var ok, feOk bool
				feConfig, ok = obj.(*frontendconfigv1beta1.FrontendConfig)
				if !ok {
					// This can happen if the watch is closed and misses the delete event
					state, stateOk := obj.(cache.DeletedFinalStateUnknown)
					if !stateOk {
						klog.Errorf("Wanted cache.DeleteFinalStateUnknown of frontendconfig obj, got: %+v type: %T", obj, obj)
						return
					}

					feConfig, feOk = state.Obj.(*frontendconfigv1beta1.FrontendConfig)
					if !feOk {
						klog.Errorf("Wanted frontendconfig obj, got %+v, type %T", state.Obj, state.Obj)
						return
					}
				}

				ings := operator.Ingresses(ctx.Ingresses().List()).ReferencesFrontendConfig(feConfig).AsList()
				lbc.ingQueue.Enqueue(convert(ings)...)
			},
		})
	}

	// Register health check on controller context.
	ctx.AddHealthCheck("ingress", func() error {
		_, err := backendPool.Get("foo", meta.VersionGA, meta.Global)

		// If this container is scheduled on a node without compute/rw it is
		// effectively useless, but it is healthy. Reporting it as unhealthy
		// will lead to container crashlooping.
		if utils.IsHTTPErrorCode(err, http.StatusForbidden) {
			klog.Infof("Reporting cluster as healthy, but unable to list backends: %v", err)
			return nil
		}
		return utils.IgnoreHTTPNotFound(err)
	})

	klog.V(3).Infof("Created new loadbalancer controller")

	return &lbc
}

func (lbc *LoadBalancerController) Init() {
	// TODO(rramkumar): Try to get rid of this "Init".
	lbc.instancePool.Init(lbc.Translator)
	lbc.backendSyncer.Init(lbc.Translator)
}

// Run starts the loadbalancer controller.
func (lbc *LoadBalancerController) Run() {
	klog.Infof("Starting loadbalancer controller")
	go lbc.ingQueue.Run()
	go lbc.nodes.Run()

	<-lbc.stopCh
	klog.Infof("Shutting down Loadbalancer Controller")
}

// Stop stops the loadbalancer controller. It also deletes cluster resources
// if deleteAll is true.
func (lbc *LoadBalancerController) Stop(deleteAll bool) error {
	// Stop is invoked from the http endpoint.
	lbc.stopLock.Lock()
	defer lbc.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !lbc.shutdown {
		close(lbc.stopCh)
		klog.Infof("Shutting down controller queues.")
		lbc.ingQueue.Shutdown()
		lbc.nodes.Shutdown()
		lbc.shutdown = true
	}

	// Deleting shared cluster resources is idempotent.
	// TODO(rramkumar): Do we need deleteAll? Can we get rid of its' flag?
	if deleteAll {
		klog.Infof("Shutting down cluster manager.")
		if err := lbc.l7Pool.Shutdown(lbc.ctx.Ingresses().List()); err != nil {
			return err
		}

		// The backend pool will also delete instance groups.
		return lbc.backendSyncer.Shutdown()
	}
	return nil
}

// SyncBackends implements Controller.
func (lbc *LoadBalancerController) SyncBackends(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}
	ingSvcPorts := syncState.urlMap.AllServicePorts()

	// Create instance groups and set named ports.
	igs, err := lbc.instancePool.EnsureInstanceGroupsAndPorts(lbc.ctx.ClusterNamer.InstanceGroup(), nodePorts(ingSvcPorts))
	if err != nil {
		return err
	}

	nodeNames, err := utils.GetReadyNodeNames(listers.NewNodeLister(lbc.nodeLister))
	if err != nil {
		return err
	}
	// Add/remove instances to the instance groups.
	if err = lbc.instancePool.Sync(nodeNames); err != nil {
		return err
	}

	// TODO: Remove this after deprecation
	ing := syncState.ing
	if utils.IsGCEMultiClusterIngress(syncState.ing) {
		// Add instance group names as annotation on the ingress and return.
		if ing.Annotations == nil {
			ing.Annotations = map[string]string{}
		}
		if err = setInstanceGroupsAnnotation(ing.Annotations, igs); err != nil {
			return err
		}
		if err = updateAnnotations(lbc.ctx.KubeClient, ing.Name, ing.Namespace, ing.Annotations); err != nil {
			return err
		}
		// This short-circuit will stop the syncer from moving to next step.
		return ingsync.ErrSkipBackendsSync
	}

	// Sync the backends
	if err := lbc.backendSyncer.Sync(ingSvcPorts); err != nil {
		return err
	}

	// Get the zones our groups live in.
	zones, err := lbc.Translator.ListZones()
	var groupKeys []backends.GroupKey
	for _, zone := range zones {
		groupKeys = append(groupKeys, backends.GroupKey{Zone: zone})
	}

	// Link backends to groups.
	for _, sp := range ingSvcPorts {
		var linkErr error
		if sp.NEGEnabled {
			// Link backend to NEG's if the backend has NEG enabled.
			linkErr = lbc.negLinker.Link(sp, groupKeys)
		} else {
			// Otherwise, link backend to IG's.
			linkErr = lbc.igLinker.Link(sp, groupKeys)
		}
		if linkErr != nil {
			return linkErr
		}
	}

	return nil
}

// GCBackends implements Controller.
func (lbc *LoadBalancerController) GCBackends(toKeep []*v1beta1.Ingress) error {
	// Only GCE ingress associated resources are managed by this controller.
	GCEIngresses := operator.Ingresses(toKeep).Filter(utils.IsGCEIngress).AsList()
	svcPortsToKeep := lbc.ToSvcPorts(GCEIngresses)
	if err := lbc.backendSyncer.GC(svcPortsToKeep); err != nil {
		return err
	}
	// TODO(ingress#120): Move this to the backend pool so it mirrors creation
	// Do not delete instance group if there exists a GLBC ingress.
	if len(toKeep) == 0 {
		igName := lbc.ctx.ClusterNamer.InstanceGroup()
		klog.Infof("Deleting instance group %v", igName)
		if err := lbc.instancePool.DeleteInstanceGroup(igName); err != err {
			return err
		}
	}
	return nil
}

// SyncLoadBalancer implements Controller.
func (lbc *LoadBalancerController) SyncLoadBalancer(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}

	lb, err := lbc.toRuntimeInfo(syncState.ing, syncState.urlMap)
	if err != nil {
		return err
	}

	// Create higher-level LB resources.
	l7, err := lbc.l7Pool.Ensure(lb)
	if err != nil {
		return err
	}

	syncState.l7 = l7
	return nil
}

// GCv1LoadBalancers implements Controller.
func (lbc *LoadBalancerController) GCv1LoadBalancers(toKeep []*v1beta1.Ingress) error {
	return lbc.l7Pool.GCv1(common.ToIngressKeys(toKeep))
}

// GCv2LoadBalancer implements Controller.
func (lbc *LoadBalancerController) GCv2LoadBalancer(ing *v1beta1.Ingress) error {
	return lbc.l7Pool.GCv2(ing)
}

// EnsureDeleteV1Finalizers implements Controller.
func (lbc *LoadBalancerController) EnsureDeleteV1Finalizers(toCleanup []*v1beta1.Ingress) error {
	if !flags.F.FinalizerRemove {
		klog.V(4).Infof("Removing finalizers not enabled")
		return nil
	}
	for _, ing := range toCleanup {
		ingClient := lbc.ctx.KubeClient.NetworkingV1beta1().Ingresses(ing.Namespace)
		if err := common.EnsureDeleteFinalizer(ing, ingClient, common.FinalizerKey); err != nil {
			klog.Errorf("Failed to ensure delete finalizer %s for ingress %s: %v", common.FinalizerKey, common.NamespacedName(ing), err)
			return err
		}
	}
	return nil
}

// EnsureDeleteV2Finalizer implements Controller.
func (lbc *LoadBalancerController) EnsureDeleteV2Finalizer(ing *v1beta1.Ingress) error {
	if !flags.F.FinalizerRemove {
		klog.V(4).Infof("Removing finalizers not enabled")
		return nil
	}
	ingClient := lbc.ctx.KubeClient.NetworkingV1beta1().Ingresses(ing.Namespace)
	if err := common.EnsureDeleteFinalizer(ing, ingClient, common.FinalizerKeyV2); err != nil {
		klog.Errorf("Failed to ensure delete finalizer %s for ingress %s: %v", common.FinalizerKeyV2, common.NamespacedName(ing), err)
		return err
	}
	return nil
}

// PostProcess implements Controller.
func (lbc *LoadBalancerController) PostProcess(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}

	// Update the ingress status.
	return lbc.updateIngressStatus(syncState.l7, syncState.ing)
}

// sync manages Ingress create/updates/deletes events from queue.
func (lbc *LoadBalancerController) sync(key string) error {
	if !lbc.hasSynced() {
		time.Sleep(context.StoreSyncPollPeriod)
		return fmt.Errorf("waiting for stores to sync")
	}
	klog.V(3).Infof("Syncing %v", key)

	ing, ingExists, err := lbc.ctx.Ingresses().GetByKey(key)
	if err != nil {
		return fmt.Errorf("error getting Ingress for key %s: %v", key, err)
	}

	// Capture GC state for ingress.
	allIngresses := lbc.ctx.Ingresses().List()

	// Determine if the ingress needs to be GCed.
	if !ingExists || utils.NeedsCleanup(ing) {
		frontendGCAlgorithm := frontendGCAlgorithm(ingExists, ing)
		// GC will find GCE resources that were used for this ingress and delete them.
		err := lbc.ingSyncer.GC(allIngresses, ing, frontendGCAlgorithm)
		// Skip emitting an event if ingress does not exist as we cannot retrieve ingress namespace.
		if err != nil && ingExists {
			lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "GC", fmt.Sprintf("Error during GC: %v", err))
		}
		// Delete the ingress state for metrics after GC is successful.
		if err == nil && ingExists {
			lbc.metrics.DeleteIngress(key)
		}
		return err
	}

	// Ensure that a finalizer is attached.
	if flags.F.FinalizerAdd {
		if ing, err = lbc.ensureFinalizer(ing); err != nil {
			return err
		}
	}

	// Bootstrap state for GCP sync.
	urlMap, errs := lbc.Translator.TranslateIngress(ing, lbc.ctx.DefaultBackendSvcPort.ID, lbc.ctx.ClusterNamer)

	if errs != nil {
		msg := fmt.Errorf("error while evaluating the ingress spec: %v", utils.JoinErrs(errs))
		lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Translate", msg.Error())
		return msg
	}

	// Sync GCP resources.
	syncState := &syncState{urlMap, ing, nil}
	syncErr := lbc.ingSyncer.Sync(syncState)
	if syncErr != nil {
		lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Sync", fmt.Sprintf("Error during sync: %v", syncErr.Error()))
	} else {
		// Insert/update the ingress state for metrics after successful sync.
		lbc.metrics.SetIngress(key, metrics.NewIngressState(ing, urlMap.AllServicePorts()))
	}

	// Garbage collection will occur regardless of an error occurring. If an error occurred,
	// it could have been caused by quota issues; therefore, garbage collecting now may
	// free up enough quota for the next sync to pass.
	frontendGCAlgorithm := frontendGCAlgorithm(ingExists, ing)
	if gcErr := lbc.ingSyncer.GC(allIngresses, ing, frontendGCAlgorithm); gcErr != nil {
		lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "GC", fmt.Sprintf("Error during GC: %v", gcErr))
		return fmt.Errorf("error during sync %v, error during GC %v", syncErr, gcErr)
	}

	return syncErr
}

// updateIngressStatus updates the IP and annotations of a loadbalancer.
// The annotations are parsed by kubectl describe.
func (lbc *LoadBalancerController) updateIngressStatus(l7 *loadbalancers.L7, ing *v1beta1.Ingress) error {
	ingClient := lbc.ctx.KubeClient.NetworkingV1beta1().Ingresses(ing.Namespace)

	// Update IP through update/status endpoint
	ip := l7.GetIP()
	currIng, err := ingClient.Get(ing.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	currIng.Status = v1beta1.IngressStatus{
		LoadBalancer: apiv1.LoadBalancerStatus{
			Ingress: []apiv1.LoadBalancerIngress{
				{IP: ip},
			},
		},
	}
	if ip != "" {
		lbIPs := ing.Status.LoadBalancer.Ingress
		if len(lbIPs) == 0 || lbIPs[0].IP != ip {
			// TODO: If this update fails it's probably resource version related,
			// which means it's advantageous to retry right away vs requeuing.
			klog.Infof("Updating loadbalancer %v/%v with IP %v", ing.Namespace, ing.Name, ip)
			if _, err := ingClient.UpdateStatus(currIng); err != nil {
				return err
			}
			lbc.ctx.Recorder(ing.Namespace).Eventf(currIng, apiv1.EventTypeNormal, "CREATE", "ip: %v", ip)
		}
	}
	annotations, err := loadbalancers.GetLBAnnotations(l7, currIng.Annotations, lbc.backendSyncer)
	if err != nil {
		return err
	}

	if err := updateAnnotations(lbc.ctx.KubeClient, ing.Name, ing.Namespace, annotations); err != nil {
		return err
	}
	return nil
}

// toRuntimeInfo returns L7RuntimeInfo for the given ingress.
func (lbc *LoadBalancerController) toRuntimeInfo(ing *v1beta1.Ingress, urlMap *utils.GCEURLMap) (*loadbalancers.L7RuntimeInfo, error) {
	annotations := annotations.FromIngress(ing)
	tls, err := lbc.tlsLoader.Load(ing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// TODO: this path should be removed when external certificate managers migrate to a better solution.
			const msg = "Could not find TLS certificates. Continuing setup for the load balancer to serve HTTP. Note: this behavior is deprecated and will be removed in a future version of ingress-gce"
			lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Sync", msg)
		} else {
			klog.Errorf("Could not get certificates for ingress %s/%s: %v", ing.Namespace, ing.Name, err)
			return nil, err
		}
	}

	var feConfig *frontendconfigv1beta1.FrontendConfig
	if lbc.ctx.FrontendConfigEnabled {
		feConfig, err = frontendconfig.FrontendConfigForIngress(lbc.ctx.FrontendConfigs().List(), ing)
		if err != nil {
			lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Sync", fmt.Sprintf("%v", err))
		}
		// Object in cache could be changed in-flight. Deepcopy to
		// reduce race conditions.
		feConfig = feConfig.DeepCopy()
	}

	return &loadbalancers.L7RuntimeInfo{
		TLS:            tls,
		TLSName:        annotations.UseNamedTLS(),
		Ingress:        ing,
		AllowHTTP:      annotations.AllowHTTP(),
		StaticIPName:   annotations.StaticIPName(),
		UrlMap:         urlMap,
		FrontendConfig: feConfig,
	}, nil
}

func updateAnnotations(client kubernetes.Interface, name, namespace string, annotations map[string]string) error {
	ingClient := client.NetworkingV1beta1().Ingresses(namespace)
	currIng, err := ingClient.Get(name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currIng.Annotations, annotations) {
		klog.V(3).Infof("Updating annotations of %v/%v", namespace, name)
		currIng.Annotations = annotations
		if _, err := ingClient.Update(currIng); err != nil {
			return err
		}
	}
	return nil
}

// ToSvcPorts returns a list of SVC ports given a list of ingresses.
// Note: This method is used for GC.
func (lbc *LoadBalancerController) ToSvcPorts(ings []*v1beta1.Ingress) []utils.ServicePort {
	var knownPorts []utils.ServicePort
	for _, ing := range ings {
		urlMap, _ := lbc.Translator.TranslateIngress(ing, lbc.ctx.DefaultBackendSvcPort.ID, lbc.ctx.ClusterNamer)
		knownPorts = append(knownPorts, urlMap.AllServicePorts()...)
	}
	return knownPorts
}

// defaultFrontendNamingScheme returns frontend naming scheme for an ingress without finalizer.
// This is used for adding an appropriate finalizer on the ingress.
func (lbc *LoadBalancerController) defaultFrontendNamingScheme(ing *v1beta1.Ingress) (namer.Scheme, error) {
	// Ingress frontend naming scheme is determined based on the following logic,
	// V2 frontend namer is disabled         : v1 frontend naming scheme
	// V2 frontend namer is enabled
	//     - VIP does not exists             : v2 frontend naming scheme
	//     - VIP exists
	//         - GCE URL Map exists          : v1 frontend naming scheme
	//         - GCE URL Map does not exists : v2 frontend naming scheme
	if !flags.F.EnableV2FrontendNamer {
		return namer.V1NamingScheme, nil
	}
	if !utils.HasVIP(ing) {
		return namer.V2NamingScheme, nil
	}
	urlMapExists, err := lbc.l7Pool.HasUrlMap(ing)
	if err != nil {
		return "", err
	}
	if urlMapExists {
		return namer.V1NamingScheme, nil
	}
	return namer.V2NamingScheme, nil
}

// ensureFinalizer ensures that a finalizer is attached.
func (lbc *LoadBalancerController) ensureFinalizer(ing *v1beta1.Ingress) (*v1beta1.Ingress, error) {
	ingKey := common.NamespacedName(ing)
	if common.HasFinalizer(ing.ObjectMeta) {
		klog.V(4).Infof("Finalizer exists for ingress %s", ingKey)
		return ing, nil
	}
	namingScheme, err := lbc.defaultFrontendNamingScheme(ing)
	if err != nil {
		return nil, err
	}
	finalizerKey, err := namer.FinalizerForNamingScheme(namingScheme)
	if err != nil {
		return nil, err
	}
	ingClient := lbc.ctx.KubeClient.NetworkingV1beta1().Ingresses(ing.Namespace)
	// Update ingress with finalizer so that load-balancer pool uses correct naming scheme
	// while ensuring frontend resources. Note that this updates only the finalizer annotation
	// which may be inconsistent with ingress store for a short period.
	updatedIng, err := common.EnsureFinalizer(ing, ingClient, finalizerKey)
	if err != nil {
		klog.Errorf("Failed to ensure finalizer %s for ingress %s: %v", finalizerKey, ingKey, err)
		return nil, err
	}
	return updatedIng, nil
}

// frontendGCAlgorithm returns the naming scheme using which frontend resources needs to be cleanedup.
// This also returns a boolean to specify if we need to delete frontend resources.
// GC path is
// If ingress does not exist :   v1 frontends and all backends
// If ingress exists
//    - Needs cleanup
//      - If v1 naming scheme  :    v1 frontends and all backends
//      - If v2 naming scheme  :    v2 frontends and all backends
//    - Does not need cleanup
//      - Finalizer enabled    :    all backends
//      - Finalizer disabled   :    v1 frontends and all backends
func frontendGCAlgorithm(ingExists bool, ing *v1beta1.Ingress) utils.FrontendGCAlgorithm {
	// If ingress does not exist, that means its pre-finalizer era.
	// Run GC via v1 naming scheme.
	if !ingExists {
		return utils.CleanupV1FrontendResources
	}
	// Determine if we do not need to delete current ingress.
	if !utils.NeedsCleanup(ing) {
		// GC backends only if current ingress does not need cleanup and finalizers is enabled.
		if flags.F.FinalizerAdd {
			return utils.NoCleanUpNeeded
		}
		return utils.CleanupV1FrontendResources
	}
	namingScheme := namer.FrontendNamingScheme(ing)
	switch namingScheme {
	case namer.V2NamingScheme:
		return utils.CleanupV2FrontendResources
	case namer.V1NamingScheme:
		return utils.CleanupV1FrontendResources
	default:
		klog.Errorf("Unexpected naming scheme %v", namingScheme)
		return utils.NoCleanUpNeeded
	}
}
