/*
Copyright 2018 The Kubernetes Authors.
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

package backends

import (
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"google.golang.org/api/compute/v1"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cloud-provider-gcp/providers/gce"
	"k8s.io/ingress-gce/pkg/backends/features"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/network"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/klog/v2"
)

const (
	DefaultConnectionDrainingTimeoutSeconds = 30
	defaultTrackingMode                     = "PER_CONNECTION"
	PerSessionTrackingMode                  = "PER_SESSION" // the only one supported with strong session affinity

)

// Backends handles CRUD operations for backends.
type Backends struct {
	cloud                       *gce.Cloud
	namer                       namer.BackendNamer
	useConnectionTrackingPolicy bool
}

// Backends is a Pool.
var _ Pool = (*Backends)(nil)

// NewPool returns a new backend pool.
// - cloud: implements BackendServices
// - namer: produces names for backends.
func NewPool(cloud *gce.Cloud, namer namer.BackendNamer) *Backends {
	return &Backends{
		cloud: cloud,
		namer: namer,
	}
}

// NewPoolWithConnectionTrackingPolicy returns a new backend pool.
// It is similar to NewPool() but has a field for ConnectionTrackingPolicy flag
// - cloud: implements BackendServices
// - namer: produces names for backends.
// - useConnectionTrackingPolicy: specifies the need in Connection Tracking Policy configuration
func NewPoolWithConnectionTrackingPolicy(cloud *gce.Cloud, namer namer.BackendNamer, useConnectionTrackingPolicy bool) *Backends {
	return &Backends{
		cloud:                       cloud,
		namer:                       namer,
		useConnectionTrackingPolicy: useConnectionTrackingPolicy,
	}
}

// ensureDescription updates the BackendService Description with the expected value
func ensureDescription(be *composite.BackendService, sp *utils.ServicePort) (needsUpdate bool) {
	desc := sp.GetDescription()
	features.SetDescription(&desc, sp)
	descString := desc.String()
	if be.Description == descString {
		return false
	}
	be.Description = descString
	return true
}

// Create implements Pool.
func (b *Backends) Create(sp utils.ServicePort, hcLink string) (*composite.BackendService, error) {
	name := sp.BackendName()
	namedPort := &compute.NamedPort{
		Name: b.namer.NamedPort(sp.NodePort),
		Port: sp.NodePort,
	}

	version := features.VersionFromServicePort(&sp)
	be := &composite.BackendService{
		Version:      version,
		Name:         name,
		Protocol:     string(sp.Protocol),
		Port:         namedPort.Port,
		PortName:     namedPort.Name,
		HealthChecks: []string{hcLink},
		// LogConfig is using GA API so this is not considered for computing API version.
		LogConfig: &composite.BackendServiceLogConfig{
			Enable: true,
			// Sampling rate needs to be specified explicitly.
			SampleRate: 1.0,
		},
	}

	if sp.L7ILBEnabled {
		// This enables l7-ILB and advanced traffic management features
		be.LoadBalancingScheme = "INTERNAL_MANAGED"
	} else if sp.L7XLBRegionalEnabled {
		be.LoadBalancingScheme = "EXTERNAL_MANAGED"
	}

	ensureDescription(be, &sp)
	scope := features.ScopeFromServicePort(&sp)
	key, err := composite.CreateKey(b.cloud, name, scope)
	if err != nil {
		return nil, err
	}

	if err := composite.CreateBackendService(b.cloud, key, be, klog.TODO()); err != nil {
		return nil, err
	}
	// Note: We need to perform a GCE call to re-fetch the object we just created
	// so that the "Fingerprint" field is filled in. This is needed to update the
	// object without error.
	return b.Get(name, version, scope)
}

// Update implements Pool.
func (b *Backends) Update(be *composite.BackendService) error {
	// Ensure the backend service has the proper version before updating.
	be.Version = features.VersionFromDescription(be.Description)
	scope, err := composite.ScopeFromSelfLink(be.SelfLink)
	if err != nil {
		return err
	}

	key, err := composite.CreateKey(b.cloud, be.Name, scope)
	if err != nil {
		return err
	}
	if err := composite.UpdateBackendService(b.cloud, key, be, klog.TODO()); err != nil {
		return err
	}
	return nil
}

// Get implements Pool.
func (b *Backends) Get(name string, version meta.Version, scope meta.KeyType) (*composite.BackendService, error) {
	key, err := composite.CreateKey(b.cloud, name, scope)
	if err != nil {
		return nil, err
	}
	be, err := composite.GetBackendService(b.cloud, key, version, klog.TODO())
	if err != nil {
		return nil, err
	}
	// Evaluate the existing features from description to see if a lower
	// API version is required so that we don't lose information from
	// the existing backend service.
	versionRequired := features.VersionFromDescription(be.Description)

	if features.IsLowerVersion(versionRequired, version) {
		be, err = composite.GetBackendService(b.cloud, key, versionRequired, klog.TODO())
		if err != nil {
			return nil, err
		}
	}
	return be, nil
}

// Delete implements Pool.
func (b *Backends) Delete(name string, version meta.Version, scope meta.KeyType) error {
	klog.V(2).Infof("Deleting backend service %v", name)

	key, err := composite.CreateKey(b.cloud, name, scope)
	if err != nil {
		return err
	}
	err = composite.DeleteBackendService(b.cloud, key, version, klog.TODO())
	if err != nil {
		if utils.IsHTTPErrorCode(err, http.StatusNotFound) || utils.IsInUsedByError(err) {
			klog.Infof("DeleteBackendService(_, %v, %v) = %v; ignorable error", key, version, err)
			return nil
		}
		klog.Errorf("DeleteBackendService(_, %v, %v) = %v", key, version, err)
		return err
	}
	klog.V(2).Infof("DeleteBackendService(_, %v, %v) ok", key, version)
	return nil
}

// Health implements Pool.
func (b *Backends) Health(name string, version meta.Version, scope meta.KeyType) (string, error) {
	be, err := b.Get(name, version, scope)
	if err != nil {
		return "Unknown", fmt.Errorf("error getting backend service %s: %w", name, err)
	}
	if len(be.Backends) == 0 {
		return "Unknown", fmt.Errorf("no backends found for backend service %q", name)
	}

	// TODO: Include port, ip in the status, since it's in the health info.
	// TODO (shance) convert to composite types
	ret := "Unknown"
	for _, backend := range be.Backends {
		var hs *compute.BackendServiceGroupHealth
		switch scope {
		case meta.Global:
			hs, err = b.cloud.GetGlobalBackendServiceHealth(name, backend.Group)
		case meta.Regional:
			hs, err = b.cloud.GetRegionalBackendServiceHealth(name, b.cloud.Region(), backend.Group)
		default:
			return "Unknown", fmt.Errorf("invalid scope for Health(): %s", scope)
		}

		if err != nil {
			return "Unknown", fmt.Errorf("error getting health for backend %q: %w", name, err)
		}
		if len(hs.HealthStatus) == 0 || hs.HealthStatus[0] == nil {
			klog.V(3).Infof("backend service %q does not have health status: %v", name, hs.HealthStatus)
			continue
		}

		for _, instanceStatus := range hs.HealthStatus {
			ret = instanceStatus.HealthState
			// return immediately with the value if we found at least one healthy instance
			if ret == "HEALTHY" {
				return ret, nil
			}
		}
	}
	return ret, nil
}

// List lists all backends managed by this controller.
func (b *Backends) List(key *meta.Key, version meta.Version) ([]*composite.BackendService, error) {
	// TODO: for consistency with the rest of this sub-package this method
	// should return a list of backend ports.
	var backends []*composite.BackendService
	var err error

	backends, err = composite.ListBackendServices(b.cloud, key, version, klog.TODO())
	if err != nil {
		return nil, err
	}

	var clusterBackends []*composite.BackendService

	for _, bs := range backends {
		if b.namer.NameBelongsToCluster(bs.Name) {
			scope, err := composite.ScopeFromSelfLink(bs.SelfLink)
			if err != nil {
				return nil, err
			}
			bs.Scope = scope

			clusterBackends = append(clusterBackends, bs)
		}
	}
	return clusterBackends, nil
}

// AddSignedUrlKey adds a SignedUrlKey to a BackendService
func (b *Backends) AddSignedUrlKey(be *composite.BackendService, signedurlkey *composite.SignedUrlKey) error {
	klog.V(2).Infof("Adding SignedUrlKey %q to backend service %q", signedurlkey.KeyName, be.Name)

	scope, err := composite.ScopeFromSelfLink(be.SelfLink)
	if err != nil {
		return err
	}

	key, err := composite.CreateKey(b.cloud, be.Name, scope)
	if err != nil {
		return err
	}
	if err := composite.AddSignedUrlKey(b.cloud, key, be, signedurlkey); err != nil {
		return err
	}
	return nil
}

// DeleteSignedUrlKey deletes a SignedUrlKey from BackendService
func (b *Backends) DeleteSignedUrlKey(be *composite.BackendService, keyName string) error {
	klog.V(2).Infof("Deleting SignedUrlKey %q from backend service %q", keyName, be.Name)

	scope, err := composite.ScopeFromSelfLink(be.SelfLink)
	if err != nil {
		return err
	}

	key, err := composite.CreateKey(b.cloud, be.Name, scope)
	if err != nil {
		return err
	}
	if err := composite.DeleteSignedUrlKey(b.cloud, key, be, keyName); err != nil {
		return err
	}
	return nil
}

// EnsureL4BackendService creates or updates the backend service with the given name.
// TODO(code-elinka): refactor the list of arguments (there are too many now)
func (b *Backends) EnsureL4BackendService(name, hcLink, protocol, sessionAffinity, scheme string, namespacedName types.NamespacedName, network network.NetworkInfo, connectionTrackingPolicy *composite.BackendServiceConnectionTrackingPolicy) (*composite.BackendService, error) {
	start := time.Now()
	klog.V(2).Infof("EnsureL4BackendService started, backend service name: %v, service: %v, scheme: %v, protocol: %v, sessionAffinity: %v, network: %v, subnetwork: %v ", name, namespacedName, scheme, protocol, sessionAffinity, network.NetworkURL, network.SubnetworkURL)
	defer func() {
		klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): finished, time taken: %v", name, protocol, scheme, namespacedName, time.Since(start))
	}()

	klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v...):checking existing backend service", name, protocol, scheme, namespacedName)
	key, err := composite.CreateKey(b.cloud, name, meta.Regional)
	if err != nil {
		return nil, err
	}
	bs, err := composite.GetBackendService(b.cloud, key, meta.VersionGA, klog.TODO())
	if err != nil && !utils.IsNotFoundError(err) {
		return nil, err
	}
	desc, err := utils.MakeL4LBServiceDescription(namespacedName.String(), "", meta.VersionGA, false, utils.ILB)
	if err != nil {
		klog.Warningf("EnsureL4BackendService: Failed to generate description for BackendService %s, err %v",
			name, err)
	}
	expectedBS := &composite.BackendService{
		Name:                name,
		Protocol:            protocol,
		Description:         desc,
		HealthChecks:        []string{hcLink},
		SessionAffinity:     utils.TranslateAffinityType(sessionAffinity),
		LoadBalancingScheme: scheme,
	}
	// We need this configuration only for Strong Session Affinity feature
	if b.useConnectionTrackingPolicy {
		klog.V(2).Infof("EnsureL4BackendService %v,  service: %v, using connection tracking policy: %+v", name, namespacedName, connectionTrackingPolicy)
		expectedBS.ConnectionTrackingPolicy = connectionTrackingPolicy
	}
	if !network.IsDefault {
		klog.V(2).Infof("EnsureL4BackendService %v,  service: %v, using non-default network: %+v", name, namespacedName, network)
		expectedBS.Network = network.NetworkURL
	}
	if protocol == string(api_v1.ProtocolTCP) {
		expectedBS.ConnectionDraining = &composite.ConnectionDraining{DrainingTimeoutSec: DefaultConnectionDrainingTimeoutSeconds}
	} else {
		// This config is not supported in UDP mode, explicitly set to 0 to reset, if proto was TCP previously.
		expectedBS.ConnectionDraining = &composite.ConnectionDraining{DrainingTimeoutSec: 0}
	}

	// Create backend service if none was found
	if bs == nil {
		klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): creating backend service %v", name, protocol, scheme, namespacedName, name)
		err := composite.CreateBackendService(b.cloud, key, expectedBS, klog.TODO())
		if err != nil {
			return nil, err
		}
		klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): created backend service %v successfully", name, protocol, scheme, namespacedName, name)
		// We need to perform a GCE call to re-fetch the object we just created
		// so that the "Fingerprint" field is filled in. This is needed to update the
		// object without error. The lookup is also needed to populate the selfLink.
		return composite.GetBackendService(b.cloud, key, meta.VersionGA, klog.TODO())
	}

	if backendSvcEqual(expectedBS, bs, b.useConnectionTrackingPolicy) {
		klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): backend service %s did not change, skipping update", name, protocol, scheme, namespacedName, name)
		return bs, nil
	}
	if bs.ConnectionDraining != nil && bs.ConnectionDraining.DrainingTimeoutSec > 0 && protocol == string(api_v1.ProtocolTCP) {
		// only preserves user overridden timeout value when the protocol is TCP
		expectedBS.ConnectionDraining.DrainingTimeoutSec = bs.ConnectionDraining.DrainingTimeoutSec
	}
	klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): updating backend service %v", name, protocol, scheme, namespacedName, name)
	// Set fingerprint for optimistic locking
	expectedBS.Fingerprint = bs.Fingerprint
	// Copy backends to avoid detaching them during update. This could be replaced with a patch call in the future.
	expectedBS.Backends = bs.Backends
	if err := composite.UpdateBackendService(b.cloud, key, expectedBS, klog.TODO()); err != nil {
		return nil, err
	}
	klog.V(2).Infof("EnsureL4BackendService(%v, _, %v, _, %v, %v ...): updated backend service %v successfully", name, protocol, scheme, namespacedName, name)

	return composite.GetBackendService(b.cloud, key, meta.VersionGA, klog.TODO())
}

// backendSvcEqual returns true if the 2 BackendService objects are equal.
// ConnectionDraining timeout is not checked for equality, if user changes
// this timeout and no other backendService parameters change, the backend
// service will not be updated. The list of backends is not checked either,
// since that is handled by the neg-linker.
// The list of backends is not checked, since that is handled by the neg-linker.
func backendSvcEqual(a, b *composite.BackendService, compareConnectionTracking bool) bool {
	svcsEqual := a.Protocol == b.Protocol &&
		a.Description == b.Description &&
		a.SessionAffinity == b.SessionAffinity &&
		a.LoadBalancingScheme == b.LoadBalancingScheme &&
		utils.EqualStringSets(a.HealthChecks, b.HealthChecks) &&
		a.Network == b.Network

	// Compare only for backendSvc that uses Strong Session Affinity feature
	if compareConnectionTracking {
		return svcsEqual && connectionTrackingPolicyEqual(a.ConnectionTrackingPolicy, b.ConnectionTrackingPolicy)
	}

	return svcsEqual
}

// connectionTrackingPolicyEqual returns true if both elements are equal
// and return false if at least one parameter is different
func connectionTrackingPolicyEqual(a, b *composite.BackendServiceConnectionTrackingPolicy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.TrackingMode == b.TrackingMode &&
		a.EnableStrongAffinity == b.EnableStrongAffinity &&
		a.IdleTimeoutSec == b.IdleTimeoutSec
}
