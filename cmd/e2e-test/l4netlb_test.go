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

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	v1 "k8s.io/api/core/v1"
	"k8s.io/ingress-gce/pkg/e2e"
	"k8s.io/ingress-gce/pkg/fuzz"
	"k8s.io/ingress-gce/pkg/utils/common"
	"k8s.io/legacy-cloud-providers/gce"
)

const (
	replicas = int32(1)
)

// Test scenario
// 1. Create 2 Services with ExternalTrafficPolicy folowing Cluster and Local.
// 2. Verify that they do not share health check.
// 3. Update Service ExternalTrafficPolicy Local to Cluster.
// 4. Verify that they share health check.
// 5. Delete one service.
// 6. Verify that health check was not deleted.
// 7. Update Service from Cluster to Local.
// 8. Delete second service.
// 9. Verify that non-shared health check was deleted.
func TestL4NetLbCreateAndUpdate(t *testing.T) {
	t.Parallel()

	Framework.RunWithSandbox("L4NetLbCreateUpdate", t, func(t *testing.T, s *e2e.Sandbox) {
		svcName := fmt.Sprintf("netlb1-%x", s.RandInt)
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName, map[string]string{}, replicas, v1.ServiceExternalTrafficPolicyTypeCluster); err != nil {
			t.Fatalf("Error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}
		svcName2 := fmt.Sprintf("netlb2-%x", s.RandInt)
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName2, map[string]string{}, replicas, v1.ServiceExternalTrafficPolicyTypeLocal); err != nil {
			t.Fatalf("Error ensuring echo service %s/%s: %q", s.Namespace, svcName2, err)
		}
		ensureL4LbRunning(s, svcName, common.NetLBFinalizerV2, t)
		ensureL4LbRunning(s, svcName2, common.NetLBFinalizerV2, t)

		_, svc1Params := validateNetLbService(s, svcName, t)
		if svc1Params.ExternalTrafficPolicy != string(v1.ServiceExternalTrafficPolicyTypeCluster) {
			t.Fatalf("Service %s ExternalTrafficPolicy should be Cluster.", svcName)
		}
		_, svc2ParamsLocal := validateNetLbService(s, svcName2, t)
		if svc2ParamsLocal.ExternalTrafficPolicy != string(v1.ServiceExternalTrafficPolicyTypeLocal) {
			t.Fatalf("Service %s ExternalTrafficPolicy should be Local.", svcName2)
		}
		if svc1Params.HcName == svc2ParamsLocal.HcName {
			t.Fatalf("Services should not share health checks")
		}

		// Update Service ExternalTrafficPolicy to Cluster
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName2, nil, replicas, v1.ServiceExternalTrafficPolicyTypeCluster); err != nil {
			t.Fatalf("Error updating echo service %s/%s: %q", s.Namespace, svcName2, err)
		}
		time.Sleep(60 * time.Second)
		eventMsg := "Local -> Cluster"
		ensureMsg := "Successfully ensured L4 External LoadBalancer resources"
		if err := e2e.WaitForSvcUpdateEvents(s, svcName2, eventMsg, ensureMsg); err != nil {
			t.Fatalf("Error waiting for event %s err: %v", eventMsg, err)
		}
		t.Logf("Check service after Update")
		_, svc2ParamsCluster := validateNetLbService(s, svcName2, t)
		if svc2ParamsCluster.ExternalTrafficPolicy != string(v1.ServiceExternalTrafficPolicyTypeCluster) {
			t.Fatalf("Service %s ExternalTrafficPolicy should be Cluster.", svcName2)
		}
		if svc1Params.HcName != svc2ParamsCluster.HcName {
			t.Fatalf("Services should share health checks")
		}

		// Delete one service and check that shared health check was not deleted
		e2e.DeleteEchoService(s, svcName)
		if err := e2e.WaitForNetLBServiceDeletion(s, Framework.Cloud, svc1Params); !isHealthCheckDeletedError(err) {
			t.Fatalf("Expected health check deleted error when checking L4 NetLB deleted err:%v", err)
		}

		// Delete second service and check that non-shared health check was deleted
		e2e.DeleteEchoService(s, svcName2)
		if err := e2e.WaitForNetLBServiceDeletion(s, Framework.Cloud, svc2ParamsLocal); err != nil {
			t.Fatalf("Unexpected error when checking L4 NetLB deleted err:%v", err)
		}
	})
}

// Test scenario
// 1. Create NetLB service.
// 2. Verify service resources and Forwarding rule schema EXTERNAL.
// 3. Update service annotation to Internal.
// 4. Verify service resources and Forwarding rule schema INTERNAL.
// 5. Remove service annotation Internal.
// 6. Verify service resources and Forwarding rule schema EXTERNAL.
func TestL4NetLbMigrateToILB(t *testing.T) {
	t.Parallel()

	Framework.RunWithSandbox("L4NetLbMigrateToILB", t, func(t *testing.T, s *e2e.Sandbox) {
		svcName := fmt.Sprintf("netlb-migration-ilb-%x", s.RandInt)
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName, map[string]string{}, replicas, v1.ServiceExternalTrafficPolicyTypeCluster); err != nil {
			t.Fatalf("Error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}
		ensureL4LbRunning(s, svcName, common.NetLBFinalizerV2, t)
		l4lbData, svcParams := validateNetLbService(s, svcName, t)
		if svcParams.ExternalTrafficPolicy != string(v1.ServiceExternalTrafficPolicyTypeCluster) {
			t.Fatalf("Service %s ExternalTrafficPolicy should be Cluster.", svcName)
		}

		if l4lbData.ForwardingRule == nil || l4lbData.ForwardingRule.GA.LoadBalancingScheme != string(cloud.SchemeExternal) {
			t.Fatalf("Wrong forwarding rule scheme %v", l4lbData.ForwardingRule)
		}
		if l4lbData.BackendService == nil || l4lbData.BackendService.GA.LoadBalancingScheme != string(cloud.SchemeExternal) {
			t.Fatalf("Wrong backend service scheme %v", l4lbData.BackendService)
		}

		// Update Service to Internal
		annotations := make(map[string]string)
		annotations[gce.ServiceAnnotationLoadBalancerType] = "Internal"
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName, annotations, replicas, ""); err != nil {
			t.Fatalf("Error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}

		eventMsg := "Deleted L4 External LoadBalancer"
		ensureMsg := "Successfully ensured load balancer resources"
		if err := e2e.WaitForSvcUpdateEvents(s, svcName, eventMsg, ensureMsg); err != nil {
			t.Fatalf("Error waiting for event %s err: %v", eventMsg, err)
		}
		if err := e2e.WaitForSvcFinalilzerDeleted(s, svcName, common.NetLBFinalizerV2); err != nil {
			t.Fatalf("Error waiting for finalizer deleted ")
		}
		ensureL4LbRunning(s, svcName, common.ILBFinalizerV2, t)
		t.Logf("Check service after Update")
		ilbResources, _ := validateILbService(s, svcName, t)
		if ilbResources.ForwardingRule == nil || ilbResources.ForwardingRule.GA.LoadBalancingScheme != string(cloud.SchemeInternal) {
			t.Fatalf("Wrong forwarding rule scheme %v", ilbResources.ForwardingRule)
		}
		if ilbResources.BackendService == nil || ilbResources.BackendService.GA.LoadBalancingScheme != string(cloud.SchemeInternal) {
			t.Fatalf("Wrong backend service scheme %v", ilbResources.BackendService)
		}
		// Update back to External
		annotations = make(map[string]string)
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName, annotations, replicas, ""); err != nil {
			t.Fatalf("Error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}

		eventMsg = "Deleted load balancer"
		ensureMsg = "Successfully ensured L4 External LoadBalancer resources"
		if err := e2e.WaitForSvcUpdateEvents(s, svcName, eventMsg, ensureMsg); err != nil {
			t.Fatalf("Error waiting for event %s err: %v", eventMsg, err)
		}
		if err := e2e.WaitForSvcFinalilzerDeleted(s, svcName, common.ILBFinalizerV2); err != nil {
			t.Fatalf("Error waiting for finalizer deleted ")
		}
		ensureL4LbRunning(s, svcName, common.NetLBFinalizerV2, t)
		l4lbData, svcParams = validateNetLbService(s, svcName, t)
		if svcParams.ExternalTrafficPolicy != string(v1.ServiceExternalTrafficPolicyTypeCluster) {
			t.Fatalf("Service %s ExternalTrafficPolicy should be Cluster.", svcName)
		}

		if l4lbData.ForwardingRule == nil || l4lbData.ForwardingRule.GA.LoadBalancingScheme != string(cloud.SchemeExternal) {
			t.Fatalf("Wrong forwarding rule scheme %v", l4lbData.ForwardingRule)
		}
		if l4lbData.BackendService == nil || l4lbData.BackendService.GA.LoadBalancingScheme != string(cloud.SchemeExternal) {
			t.Fatalf("Wrong backend service scheme %v", l4lbData.BackendService)
		}
	})
}
func TestL4NetLbServicesNetworkTierAnnotationChanged(t *testing.T) {
	t.SkipNow()
	t.Parallel()

	Framework.RunWithSandbox("L4NetLbNetworkTier", t, func(t *testing.T, s *e2e.Sandbox) {
		replicas := int32(1)
		svcName := fmt.Sprintf("netlb-network-tier-%x", s.RandInt)
		if _, err := e2e.EnsureL4NetLBEchoService(s, svcName, map[string]string{}, replicas, v1.ServiceExternalTrafficPolicyTypeCluster); err != nil {
			t.Fatalf("error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}
		ensureL4LbRunning(s, svcName, common.NetLBFinalizerV2, t)
		l4netlb, _ := validateNetLbService(s, svcName, t)
		if l4netlb.ForwardingRule.GA.NetworkTier != cloud.NetworkTierDefault.ToGCEValue() {
			t.Fatalf("Network tier mismatch %v != %v", l4netlb.ForwardingRule.GA.NetworkTier, cloud.NetworkTierDefault)
		}
	})
}

func isHealthCheckDeletedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error health check should be deleted")
}

func validateNetLbService(s *e2e.Sandbox, svcName string, t *testing.T) (*fuzz.L4NetLB, *fuzz.L4LBForSvcParams) {
	svcParams, err := e2e.GetL4LBForSvcParams(s, svcName)
	if err != nil {
		t.Fatalf("Error getting svc params: Err %q", err)
	}
	svcParams.Region = Framework.Region
	svcParams.Network = Framework.Network
	t.Logf("Service: %s", svcName)
	t.Logf("Health check name: %s", svcParams.HcName)
	t.Logf("Health check firewall rule name: %s", svcParams.HcFwRuleName)
	t.Logf("Backend name: %s", svcParams.BsName)
	t.Logf("Forwarding rule name: %s", svcParams.FwrName)
	t.Logf("Forwarding rule firewall name: %s", svcParams.FwRuleName)
	t.Logf("Region: %s", svcParams.Region)
	t.Logf("VIP: %s", svcParams.VIP)
	l4netlb, err := fuzz.GetRegionalL4NetLBForService(context.Background(), Framework.Cloud, svcParams)
	if err != nil {
		t.Fatalf("Error checking L4 NetLB resources %v:", err)
	}
	return l4netlb, svcParams
}

func validateILbService(s *e2e.Sandbox, svcName string, t *testing.T) (*fuzz.L4ILB, *fuzz.L4LBForSvcParams) {
	svcParams, err := e2e.GetL4LBForSvcParams(s, svcName)
	if err != nil {
		t.Fatalf("Error getting svc params: Err %q", err)
	}
	svcParams.Region = Framework.Region
	svcParams.Network = Framework.Network
	t.Logf("ILB Service: %s", svcName)
	t.Logf("Health check name: %s", svcParams.HcName)
	t.Logf("Health check firewall rule name: %s", svcParams.HcFwRuleName)
	t.Logf("Backend name: %s", svcParams.BsName)
	t.Logf("Forwarding rule name: %s", svcParams.FwrName)
	t.Logf("Forwarding rule firewall name: %s", svcParams.FwRuleName)
	t.Logf("Region: %s", svcParams.Region)
	t.Logf("VIP: %s", svcParams.VIP)
	l4ilb, err := fuzz.GetL4ILBForService(context.Background(), Framework.Cloud, svcParams)
	if err != nil {
		t.Fatalf("Error checking L4 ILB resources %v:", err)
	}
	return l4ilb, svcParams
}

func ensureL4LbRunning(s *e2e.Sandbox, svcName, finalizer string, t *testing.T) {
	if err := e2e.WaitForEchoDeploymentStable(s, svcName); err != nil {
		t.Errorf("Echo deployment failed to become stable: %v", err)
	}
	t.Logf("ensured echo service %s/%s", s.Namespace, svcName)
	if err := e2e.WaitForSvcFinalilzer(s, svcName, finalizer); err != nil {
		t.Fatalf("Errore waiting for svc finalizer: %q", err)
	}
	t.Logf("finalizer is present %s/%s", s.Namespace, svcName)
	if err := e2e.WaitForNetLbAnnotations(s, svcName); err != nil {
		t.Fatalf("Expected service annotations")
	}
}
