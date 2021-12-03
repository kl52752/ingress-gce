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

package l4lb

import (
	"reflect"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cloud-provider/service/helpers"
	"k8s.io/ingress-gce/pkg/context"
	l4metrics "k8s.io/ingress-gce/pkg/l4lb/metrics"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/patch"
	"k8s.io/klog"
)

// ComputeNewAnnotationsIfNeeded checks if new annotations should be added to service.
// If needed creates new service meta object.
// This function is used by External and Internal L4 LB controllers.
func ComputeNewAnnotationsIfNeeded(svc *v1.Service, newAnnotations map[string]string) *metav1.ObjectMeta {
	newObjectMeta := svc.ObjectMeta.DeepCopy()
	newObjectMeta.Annotations = mergeAnnotations(newObjectMeta.Annotations, newAnnotations)
	if reflect.DeepEqual(svc.Annotations, newObjectMeta.Annotations) {
		return nil
	}
	return newObjectMeta
}

// mergeAnnotations merges the new set of l4lb resource annotations with the pre-existing service annotations.
// Existing L4 resource annotation values will be replaced with the values in the new map.
func mergeAnnotations(existing, lbAnnotations map[string]string) map[string]string {
	if existing == nil {
		existing = make(map[string]string)
	} else {
		// Delete existing L4 annotations.
		for _, key := range loadbalancers.L4LBResourceAnnotationKeys {
			delete(existing, key)
		}
	}
	// merge existing annotations with the newly added annotations
	for key, val := range lbAnnotations {
		existing[key] = val
	}
	return existing
}

func updateAnnotations(ctx *context.ControllerContext, svc *v1.Service, newL4LBAnnotations map[string]string) error {
	newObjectMeta := ComputeNewAnnotationsIfNeeded(svc, newL4LBAnnotations)
	if newObjectMeta == nil {
		return nil
	}
	klog.V(3).Infof("Patching annotations of service %v/%v", svc.Namespace, svc.Name)
	return patch.PatchServiceObjectMetadata(ctx.KubeClient.CoreV1(), svc, *newObjectMeta)
}

func updateServiceStatus(ctx *context.ControllerContext, svc *v1.Service, newStatus *v1.LoadBalancerStatus) error {
	if helpers.LoadBalancerStatusEqual(&svc.Status.LoadBalancer, newStatus) {
		return nil
	}
	return patch.PatchServiceLoadBalancerStatus(ctx.KubeClient.CoreV1(), svc, *newStatus)
}

func publishMetrics(ctx *context.ControllerContext, result *loadbalancers.L4LbSyncResult, namespacedName string) {
	if result == nil {
		return
	}
	switch result.SyncType {
	case loadbalancers.SyncTypeCreate, loadbalancers.SyncTypeUpdate:
		klog.V(6).Infof("Internal L4 Loadbalancer for Service %s ensured, updating its state %v in metrics cache", namespacedName, result.MetricsState)
		ctx.ControllerMetrics.SetL4ILBService(namespacedName, result.MetricsState)
		l4metrics.PublishILBSyncMetrics(result.Error == nil, result.SyncType, result.GCEResourceInError, utils.GetErrorType(result.Error), result.StartTime)

	case loadbalancers.SyncTypeDelete:
		// if service is successfully deleted, remove it from cache
		if result.Error == nil {
			klog.V(6).Infof("Internal L4 Loadbalancer for Service %s deleted, removing its state from metrics cache", namespacedName)
			ctx.ControllerMetrics.DeleteL4ILBService(namespacedName)
		}
		l4metrics.PublishILBSyncMetrics(result.Error == nil, result.SyncType, result.GCEResourceInError, utils.GetErrorType(result.Error), result.StartTime)
	default:
		klog.Warningf("Unknown sync type %q, skipping metrics", result.SyncType)
	}
}
