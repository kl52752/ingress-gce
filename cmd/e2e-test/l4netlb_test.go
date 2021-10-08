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
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/ingress-gce/pkg/e2e"
)

func TestL4NetLbCreation(t *testing.T) {
	t.Parallel()

	Framework.RunWithSandbox("L4NetLb", t, func(t *testing.T, s *e2e.Sandbox) {
		svcName := fmt.Sprintf("netlb-service-e2e-%x", s.RandInt)
		// saName := fmt.Sprintf("service-netlb-e2e-%x", s.RandInt)
		replicas := int32(1)
		if err := e2e.EnsureEchoDeployment(s, svcName, replicas, e2e.NoopModify); err != nil {
			t.Fatalf("Error ensuring echo deployment: %v", err)
		}
		if err := e2e.WaitForEchoDeploymentStable(s, svcName); err != nil {
			t.Errorf("Echo deployment failed to become stable: %v", err)
		}
		if _, err := e2e.EnsureEchoService(s, svcName, map[string]string{}, v1.ServiceTypeLoadBalancer, replicas); err != nil {
			t.Fatalf("error ensuring echo service %s/%s: %q", s.Namespace, svcName, err)
		}
		t.Logf("ensured echo service %s/%s", s.Namespace, svcName)
		foundEvents, err := e2e.CheckSvcEvents(s, svcName, v1.EventTypeNormal, "ADD")
		if err != nil {
			t.Fatalf("errored quering for service events: %q", err)
		}
		if foundEvents {
			t.Fatalf("found error events when none were expected")
		}
	})
}
