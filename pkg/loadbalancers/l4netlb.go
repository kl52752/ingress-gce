/*
Copyright 2020 The Kubernetes Authors.

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

package loadbalancers

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/cloud-provider/service/helpers"
	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/firewalls"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/klog"
	"k8s.io/legacy-cloud-providers/gce"
)

// L4NetLb handles the resource creation/deletion/update for a given L4NetLb service.
type L4NetLb struct {
	cloud       *gce.Cloud
	backendPool *backends.Backends
	scope       meta.KeyType
	// TODO(52752) change namer for proper NetLb Namer
	namer namer.L4ResourcesNamer
	// recorder is used to generate k8s Events.
	recorder            record.EventRecorder
	Service             *corev1.Service
	ServicePort         utils.ServicePort
	NamespacedName      types.NamespacedName
	sharedResourcesLock *sync.Mutex
}

// SyncResultNetLb contains information about the outcome of an L4NetLb sync. It stores the list of resource name annotations,
// sync error, the GCE resource that hit the error along with the error type and more fields.
type SyncResultNetLb struct {
	Annotations        map[string]string
	Error              error
	GCEResourceInError string
	Status             *corev1.LoadBalancerStatus
	SyncType           string
	StartTime          time.Time
}

// NewL4NetLb creates a new Handler for the given L4NetLb service.
func NewL4NetLb(service *corev1.Service, cloud *gce.Cloud, scope meta.KeyType, namer namer.L4ResourcesNamer, recorder record.EventRecorder, lock *sync.Mutex) *L4NetLb {
	l4netlb := &L4NetLb{cloud: cloud,
		scope:               scope,
		namer:               namer,
		recorder:            recorder,
		Service:             service,
		sharedResourcesLock: lock,
		NamespacedName:      types.NamespacedName{Name: service.Name, Namespace: service.Namespace},
		backendPool:         backends.NewPool(cloud, namer),
	}
	portId := utils.ServicePortID{Service: l4netlb.NamespacedName}
	l4netlb.ServicePort = utils.ServicePort{ID: portId,
		BackendNamer: l4netlb.namer,
	}
	return l4netlb
}

// createKey generates a meta.Key for a given GCE resource name.
func (l4netlb *L4NetLb) createKey(name string) (*meta.Key, error) {
	return composite.CreateKey(l4netlb.cloud, name, l4netlb.scope)
}

// EnsureNetLoadBalancer ensures that all GCE resources for the given loadbalancer service have
// been created. It returns a LoadBalancerStatus with the updated ForwardingRule IP address.
func (l4netlb *L4NetLb) EnsureNetLoadBalancer(nodeNames []string, svc *corev1.Service) *SyncResultNetLb {
	result := &SyncResultNetLb{
		Annotations: make(map[string]string),
		StartTime:   time.Now(),
		SyncType:    SyncTypeCreate}

	// If service already has an IP assigned, treat it as an update instead of a new Loadbalancer.
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		result.SyncType = SyncTypeUpdate
	}

	l4netlb.Service = svc
	hcLink, hcFwName, hcPort, err := l4netlb.createHealthCheck()
	if err != nil {
		result.GCEResourceInError = annotations.HealthcheckResource
		result.Error = err
		return result
	}

	name := l4netlb.ServicePort.BackendName()
	protocol, res := l4netlb.createFirewalls(name, hcLink, hcFwName, hcPort, nodeNames)
	if res.Error != nil {
		return res
	}
	existingFR, e := l4netlb.handleBackendProtocolChange(name, protocol)
	if e != nil {
		result.Error = err
		return result
	}

	// ensure backend service
	bs, err := l4netlb.backendPool.EnsureL4BackendService(name, hcLink, protocol, string(l4netlb.Service.Spec.SessionAffinity), string(cloud.SchemeExternal), l4netlb.NamespacedName, meta.VersionGA)
	if err != nil {
		result.GCEResourceInError = annotations.BackendServiceResource
		result.Error = err
		return result
	}

	fr, err := l4netlb.ensureForwardingRule(bs.SelfLink, existingFR)
	if err != nil {
		klog.Errorf("EnsureNetLoadBalancer: Failed to create forwarding rule - %v", err)
		result.GCEResourceInError = annotations.ForwardingRuleResource
		result.Error = err
		return result
	}

	result.Status = &corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: fr.IPAddress}}}
	return result
}

// GetFRName returns the name of the forwarding rule for the given NetLB service.
// This appends the protocol to the forwarding rule name, which will help supporting multiple protocols in the same service.
func (l4netlb *L4NetLb) GetFRName() string {
	_, _, _, protocol := utils.GetPortsAndProtocol(l4netlb.Service.Spec.Ports)
	return l4netlb.getFRNameWithProtocol(string(protocol))
}

func (l4netlb *L4NetLb) getFRNameWithProtocol(protocol string) string {
	return l4netlb.namer.L4ForwardingRule(l4netlb.Service.Namespace, l4netlb.Service.Name, strings.ToLower(protocol))
}

func (l4netlb *L4NetLb) createHealthCheck() (string, string, int32, error) {
	// TODO(52752) set shared based on service params
	sharedHC := true
	hcName, hcFwName := l4netlb.namer.L4HealthCheck(l4netlb.Service.Namespace, l4netlb.Service.Name, sharedHC)
	hcPath, hcPort := gce.GetNodesHealthCheckPath(), gce.GetNodesHealthCheckPort()
	if !sharedHC {
		hcPath, hcPort = helpers.GetServiceHealthCheckPathPort(l4netlb.Service)
	} else {
		// Take the lock when creating the shared healthcheck
		l4netlb.sharedResourcesLock.Lock()
		defer l4netlb.sharedResourcesLock.Unlock()
	}

	_, hcLink, err := healthchecks.EnsureL4HealthCheck(l4netlb.cloud, hcName, l4netlb.NamespacedName, sharedHC, hcPath, hcPort, meta.Regional)
	return hcLink, hcFwName, hcPort, err
}

func (l4netlb *L4NetLb) createFirewalls(name, hcLink, hcFwName string, hcPort int32, nodeNames []string) (string, *SyncResultNetLb) {
	_, portRanges, _, protocol := utils.GetPortsAndProtocol(l4netlb.Service.Spec.Ports)
	// ensure firewalls
	result := &SyncResultNetLb{}
	sourceRanges, err := helpers.GetLoadBalancerSourceRanges(l4netlb.Service)
	if err != nil {
		result.Error = err
		return "", result
	}
	hcSourceRanges := gce.L4LoadBalancerSrcRanges()
	ensureFunc := func(name, IP string, sourceRanges, portRanges []string, proto string, shared bool) error {
		if shared {
			l4netlb.sharedResourcesLock.Lock()
			defer l4netlb.sharedResourcesLock.Unlock()
		}
		nsName := utils.ServiceKeyFunc(l4netlb.Service.Namespace, l4netlb.Service.Name)
		err := firewalls.EnsureL4InternalFirewallRule(l4netlb.cloud, name, IP, nsName, sourceRanges, portRanges, nodeNames, proto, shared)
		if err != nil {
			if fwErr, ok := err.(*firewalls.FirewallXPNError); ok {
				l4netlb.recorder.Eventf(l4netlb.Service, corev1.EventTypeNormal, "XPN", fwErr.Message)
				return nil
			}
			return err
		}
		return nil
	}

	//// Add firewall rule for NetLB traffic to nodes
	result.Error = ensureFunc(name, hcLink, sourceRanges.StringSlice(), portRanges, string(protocol), false)
	if result.Error != nil {
		result.GCEResourceInError = annotations.FirewallRuleResource
		return "", result
	}

	// Add firewall rule for healthchecks to nodes
	result.Error = ensureFunc(hcFwName, "", hcSourceRanges, []string{strconv.Itoa(int(hcPort))}, string(corev1.ProtocolTCP), false)
	if result.Error != nil {
		result.GCEResourceInError = annotations.FirewallForHealthcheckResource
	}
	return string(protocol), result
}

func (l4netlb *L4NetLb) handleBackendProtocolChange(name, protocol string) (*composite.ForwardingRule, error) {
	//Check if protocol has changed for this service. In this case, forwarding rule should be deleted before
	//the backend service can be updated.
	existingBS, err := l4netlb.backendPool.Get(name, meta.VersionGA, l4netlb.scope)
	err = utils.IgnoreHTTPNotFound(err)
	if err != nil {
		klog.Errorf("Failed to lookup existing backend service, ignoring err: %v", err)
		return nil, err
	}
	existingFR := l4netlb.GetForwardingRule(l4netlb.GetFRName(), meta.VersionGA)
	if existingBS != nil && existingBS.Protocol != protocol {
		klog.Infof("Protocol changed from %q to %q for service %s", existingBS.Protocol, protocol, l4netlb.NamespacedName)
		// Delete forwarding rule if it exists
		existingFR = l4netlb.GetForwardingRule(l4netlb.getFRNameWithProtocol(existingBS.Protocol), meta.VersionGA)
		l4netlb.deleteForwardingRule(l4netlb.getFRNameWithProtocol(existingBS.Protocol), meta.VersionGA)
	}
	return existingFR, nil
}
