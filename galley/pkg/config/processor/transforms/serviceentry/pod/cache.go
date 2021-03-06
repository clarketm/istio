// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pod

import (
	"fmt"
	"reflect"

	"istio.io/istio/galley/pkg/config/event"
	"istio.io/istio/galley/pkg/config/meta/metadata"
	"istio.io/istio/galley/pkg/config/resource"
	"istio.io/istio/pkg/spiffe"

	coreV1 "k8s.io/api/core/v1"
)

var _ Cache = &cacheImpl{}
var _ event.Handler = &cacheImpl{}

// k8s well known labels
const (
	LabelZoneRegion        = "failure-domain.beta.kubernetes.io/region"
	LabelZoneFailureDomain = "failure-domain.beta.kubernetes.io/zone"
)

// Info for a Pod.
type Info struct {
	IP       string
	FullName resource.Name
	Labels   map[string]string
	Locality string

	// ServiceAccountName the Spiffe name for the Pod service account.
	ServiceAccountName string

	NodeName string
}

// Listener is an observer of updates to the pod cache.
type Listener struct {
	PodAdded   func(info Info)
	PodUpdated func(info Info)
	PodDeleted func(info Info)
}

// Cache for pod Info.
type Cache interface {
	GetPodByIP(ip string) (Info, bool)
}

// NewCache creates a cache and its update handler
func NewCache(listener Listener) (Cache, event.Handler) {
	c := &cacheImpl{
		pods:               make(map[string]Info),
		nodeNameToLocality: make(map[string]string),
		listener:           listener,
	}
	return c, c
}

type cacheImpl struct {
	listener           Listener
	pods               map[string]Info
	nodeNameToLocality map[string]string
}

// GetPodByIP looks up and returns pod info based on ip.
func (pc *cacheImpl) GetPodByIP(ip string) (Info, bool) {
	pod, ok := pc.pods[ip]
	return pod, ok
}

// Handle implmenets event.Handler
func (pc *cacheImpl) Handle(e event.Event) {
	switch e.Source {
	case metadata.K8SCoreV1Nodes:
		pc.handleNode(e)
	case metadata.K8SCoreV1Pods:
		pc.handlePod(e)
	default:
		return
	}
}

func (pc *cacheImpl) handleNode(e event.Event) {
	// Nodes don't have namespaces.
	_, nodeName := e.Entry.Metadata.Name.InterpretAsNamespaceAndName()

	switch e.Kind {
	case event.Added, event.Updated:
		// Just update the node information directly
		labels := e.Entry.Metadata.Labels

		region := labels[LabelZoneRegion]
		zone := labels[LabelZoneFailureDomain]

		newLocality := getLocality(region, zone)
		oldLocality := pc.nodeNameToLocality[nodeName]
		if newLocality != oldLocality {
			pc.nodeNameToLocality[nodeName] = getLocality(region, zone)

			// Update the pods.
			pc.updatePodLocality(nodeName, newLocality)
		}
	case event.Deleted:
		if _, ok := pc.nodeNameToLocality[nodeName]; ok {
			delete(pc.nodeNameToLocality, nodeName)

			// Update the pods.
			pc.updatePodLocality(nodeName, "")
		}
	}
}

func (pc *cacheImpl) handlePod(e event.Event) {
	switch e.Kind {
	case event.Added, event.Updated:
		pod := e.Entry.Item.(*coreV1.Pod)

		ip := pod.Status.PodIP
		if ip == "" {
			// PodIP will be empty when pod is just created, but before the IP is assigned
			// via UpdateStatus.
			return
		}

		switch pod.Status.Phase {
		case coreV1.PodPending, coreV1.PodRunning:
			// add to cache if the pod is running or pending
			nodeName := pod.Spec.NodeName
			locality := pc.nodeNameToLocality[nodeName]
			serviceAccountName := kubeToIstioServiceAccount(pod.Spec.ServiceAccountName, pod.Namespace)
			pod := Info{
				IP:                 ip,
				FullName:           e.Entry.Metadata.Name,
				NodeName:           nodeName,
				Locality:           locality,
				Labels:             pod.Labels,
				ServiceAccountName: serviceAccountName,
			}

			pc.updatePod(pod)
		default:
			// delete if the pod switched to other states and is in the cache
			pc.deletePod(ip)
		}
	case event.Deleted:
		var ip string
		if pod, ok := e.Entry.Item.(*coreV1.Pod); ok {
			ip = pod.Status.PodIP
		} else {
			// The resource was either not available or failed parsing. Look it up by brute force.
			for podIP, info := range pc.pods {
				if info.FullName == e.Entry.Metadata.Name {
					ip = podIP
					break
				}
			}
		}

		// delete only if this pod was in the cache
		pc.deletePod(ip)
	}
}

func (pc *cacheImpl) updatePod(pod Info) {
	// Store the pod.
	prevPod, exists := pc.pods[pod.IP]
	if exists && reflect.DeepEqual(prevPod, pod) {
		// Nothing changed - just return.
		return
	}

	// Store the updated pod.
	pc.pods[pod.IP] = pod

	// Notify the listeners.
	if exists {
		pc.listener.PodUpdated(pod)
	} else {
		pc.listener.PodAdded(pod)
	}

}

func (pc *cacheImpl) deletePod(ip string) {
	if pod, exists := pc.pods[ip]; exists {
		delete(pc.pods, ip)
		pc.listener.PodDeleted(pod)
	}
}

func (pc *cacheImpl) updatePodLocality(nodeName string, locality string) {
	updatedPods := make([]Info, 0)
	for ip, pod := range pc.pods {
		if pod.NodeName == nodeName {
			// Update locality and store the change back into the map.
			pod.Locality = locality
			pc.pods[ip] = pod

			// Mark this pod as updated.
			updatedPods = append(updatedPods, pod)
		}
	}

	// Notify the listener that the pods have been updated.
	for _, pod := range updatedPods {
		pc.listener.PodUpdated(pod)
	}
}

func getLocality(region, zone string) string {
	if region == "" && zone == "" {
		return ""
	}

	return fmt.Sprintf("%v/%v", region, zone)
}

// kubeToIstioServiceAccount converts a K8s service account to an Istio service account
func kubeToIstioServiceAccount(saname string, ns string) string {
	return spiffe.MustGenSpiffeURI(ns, saname)
}
