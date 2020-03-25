// Copyright 2020 Istio Authors. All Rights Reserved.
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

package v1alpha3

import (
	"fmt"

	apiv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/types"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/util/gogo"
)

var (
	defaultDestinationRule = networking.DestinationRule{}
)

// ClusterBuilder interface provides an abstraction for building Envoy Clusters.
type ClusterBuilder struct {
	proxy *model.Proxy
	push  *model.PushContext
}

// NewClusterBuilder builds an instance of ClusterBuilder.
func NewClusterBuilder(proxy *model.Proxy, push *model.PushContext) *ClusterBuilder {
	return &ClusterBuilder{
		proxy: proxy,
		push:  push,
	}
}

// applyDestinationRule applies the destination rule if it exists for the Service. It returns the subset clusters if any created as it
// applies the destination rule.
func (cb *ClusterBuilder) applyDestinationRule(cluster *apiv2.Cluster, clusterMode ClusterMode, service *model.Service, port *model.Port,
	proxyNetworkView map[string]bool) []*apiv2.Cluster {
	destRule := cb.push.DestinationRule(cb.proxy, service)
	destinationRule := castDestinationRuleOrDefault(destRule)

	opts := buildClusterOpts{
		push:        cb.push,
		cluster:     cluster,
		policy:      destinationRule.TrafficPolicy,
		port:        port,
		clusterMode: clusterMode,
		direction:   model.TrafficDirectionOutbound,
		proxy:       cb.proxy,
	}

	if clusterMode == DefaultClusterMode {
		opts.serviceAccounts = cb.push.ServiceAccounts[service.Hostname][port.Port]
		opts.istioMtlsSni = model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, "", service.Hostname, port.Port)
		opts.simpleTLSSni = string(service.Hostname)
		opts.meshExternal = service.MeshExternal
		opts.serviceMTLSMode = cb.push.BestEffortInferServiceMTLSMode(service, port)
	}

	// Apply traffic policy for the main default cluster.
	applyTrafficPolicy(opts)

	// Apply EdsConfig if needed. This should be called after traffic policy is applied because, traffic policy might change
	// discovery type.
	maybeApplyEdsConfig(cluster)

	var clusterMetadata *core.Metadata
	if destRule != nil {
		clusterMetadata = util.BuildConfigInfoMetadata(destRule.ConfigMeta)
		cluster.Metadata = clusterMetadata
	}
	subsetClusters := make([]*apiv2.Cluster, 0)
	for _, subset := range destinationRule.Subsets {
		var subsetClusterName string
		var defaultSni string
		if clusterMode == DefaultClusterMode {
			subsetClusterName = model.BuildSubsetKey(model.TrafficDirectionOutbound, subset.Name, service.Hostname, port.Port)
			defaultSni = model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, subset.Name, service.Hostname, port.Port)

		} else {
			subsetClusterName = model.BuildDNSSrvSubsetKey(model.TrafficDirectionOutbound, subset.Name, service.Hostname, port.Port)
		}
		// clusters with discovery type STATIC, STRICT_DNS rely on cluster.hosts field
		// ServiceEntry's need to filter hosts based on subset.labels in order to perform weighted routing
		var lbEndpoints []*endpoint.LocalityLbEndpoints
		if cluster.GetType() != apiv2.Cluster_EDS && len(subset.Labels) != 0 {
			lbEndpoints = buildLocalityLbEndpoints(cb.push, proxyNetworkView, service, port.Port, []labels.Instance{subset.Labels})
		}

		subsetCluster := cb.buildDefaultCluster(subsetClusterName, cluster.GetType(), lbEndpoints,
			model.TrafficDirectionOutbound, nil, service.MeshExternal)

		if subsetCluster == nil {
			continue
		}
		if len(cb.push.Mesh.OutboundClusterStatName) != 0 {
			subsetCluster.AltStatName = util.BuildStatPrefix(cb.push.Mesh.OutboundClusterStatName, string(service.Hostname), subset.Name, port, service.Attributes)
		}
		setUpstreamProtocol(cb.proxy, subsetCluster, port, model.TrafficDirectionOutbound)

		// Apply traffic policy for subset cluster with the destination rule traffice policy.
		opts.cluster = subsetCluster
		opts.policy = destinationRule.TrafficPolicy
		opts.istioMtlsSni = defaultSni
		applyTrafficPolicy(opts)

		// If subset has a traffic policy, apply it so that it overrides the destination rule traffic policy.
		if subset.TrafficPolicy != nil {
			opts.policy = subset.TrafficPolicy
			applyTrafficPolicy(opts)
		}

		maybeApplyEdsConfig(subsetCluster)

		subsetCluster.Metadata = util.AddSubsetToMetadata(clusterMetadata, subset.Name)
		subsetClusters = append(subsetClusters, subsetCluster)
	}
	return subsetClusters
}

// buildDefaultCluster builds the default cluster and also applies default traffic policy.
func (cb *ClusterBuilder) buildDefaultCluster(name string, discoveryType apiv2.Cluster_DiscoveryType,
	localityLbEndpoints []*endpoint.LocalityLbEndpoints, direction model.TrafficDirection,
	port *model.Port, meshExternal bool) *apiv2.Cluster {
	cluster := &apiv2.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &apiv2.Cluster_Type{Type: discoveryType},
	}

	switch discoveryType {
	case apiv2.Cluster_STRICT_DNS:
		cluster.DnsLookupFamily = apiv2.Cluster_V4_ONLY
		dnsRate := gogo.DurationToProtoDuration(cb.push.Mesh.DnsRefreshRate)
		cluster.DnsRefreshRate = dnsRate
		cluster.RespectDnsTtl = true
		fallthrough
	case apiv2.Cluster_STATIC:
		if len(localityLbEndpoints) == 0 {
			cb.push.AddMetric(model.DNSNoEndpointClusters, cluster.Name, cb.proxy,
				fmt.Sprintf("%s cluster without endpoints %s found while pushing CDS", discoveryType.String(), cluster.Name))
			return nil
		}
		cluster.LoadAssignment = &apiv2.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints:   localityLbEndpoints,
		}
	}

	// For inbound clusters, the default traffic policy is used. For outbound clusters, the default traffic policy
	// will be applied, which would be overridden by traffic policy specified in destination rule, if any.
	opts := buildClusterOpts{
		push:            cb.push,
		cluster:         cluster,
		policy:          cb.defaultTrafficPolicy(discoveryType),
		port:            port,
		serviceAccounts: nil,
		istioMtlsSni:    "",
		clusterMode:     DefaultClusterMode,
		direction:       direction,
		proxy:           cb.proxy,
		meshExternal:    meshExternal,
	}
	applyTrafficPolicy(opts)

	return cluster
}

// buildInboundPassthroughClusters builds passthrough clusters for inbound.
func (cb *ClusterBuilder) buildInboundPassthroughClusters() []*apiv2.Cluster {
	// ipv4 and ipv6 feature detection. Envoy cannot ignore a config where the ip version is not supported
	clusters := make([]*apiv2.Cluster, 0, 2)
	if cb.proxy.SupportsIPv4() {
		inboundPassthroughClusterIpv4 := cb.buildDefaultPassthroughCluster()
		inboundPassthroughClusterIpv4.Name = util.InboundPassthroughClusterIpv4
		inboundPassthroughClusterIpv4.UpstreamBindConfig = &core.BindConfig{
			SourceAddress: &core.SocketAddress{
				Address: util.InboundPassthroughBindIpv4,
				PortSpecifier: &core.SocketAddress_PortValue{
					PortValue: uint32(0),
				},
			},
		}
		clusters = append(clusters, inboundPassthroughClusterIpv4)
	}
	if cb.proxy.SupportsIPv6() {
		inboundPassthroughClusterIpv6 := cb.buildDefaultPassthroughCluster()
		inboundPassthroughClusterIpv6.Name = util.InboundPassthroughClusterIpv6
		inboundPassthroughClusterIpv6.UpstreamBindConfig = &core.BindConfig{
			SourceAddress: &core.SocketAddress{
				Address: util.InboundPassthroughBindIpv6,
				PortSpecifier: &core.SocketAddress_PortValue{
					PortValue: uint32(0),
				},
			},
		}
		clusters = append(clusters, inboundPassthroughClusterIpv6)
	}
	return clusters
}

// generates a cluster that sends traffic to dummy localport 0
// This cluster is used to catch all traffic to unresolved destinations in virtual service
func (cb *ClusterBuilder) buildBlackHoleCluster() *apiv2.Cluster {
	cluster := &apiv2.Cluster{
		Name:                 util.BlackHoleCluster,
		ClusterDiscoveryType: &apiv2.Cluster_Type{Type: apiv2.Cluster_STATIC},
		ConnectTimeout:       gogo.DurationToProtoDuration(cb.push.Mesh.ConnectTimeout),
		LbPolicy:             apiv2.Cluster_ROUND_ROBIN,
	}
	return cluster
}

// generates a cluster that sends traffic to the original destination.
// This cluster is used to catch all traffic to unknown listener ports
func (cb *ClusterBuilder) buildDefaultPassthroughCluster() *apiv2.Cluster {
	cluster := &apiv2.Cluster{
		Name:                 util.PassthroughCluster,
		ClusterDiscoveryType: &apiv2.Cluster_Type{Type: apiv2.Cluster_ORIGINAL_DST},
		ConnectTimeout:       gogo.DurationToProtoDuration(cb.push.Mesh.ConnectTimeout),
		LbPolicy:             apiv2.Cluster_CLUSTER_PROVIDED,
	}
	passthroughSettings := &networking.ConnectionPoolSettings{}
	applyConnectionPool(cb.push, cluster, passthroughSettings)
	return cluster
}

// defaultTrafficPolicy builds a default traffic policy applying default connection timeouts.
func (cb *ClusterBuilder) defaultTrafficPolicy(discoveryType apiv2.Cluster_DiscoveryType) *networking.TrafficPolicy {
	lbPolicy := DefaultLbType
	if discoveryType == apiv2.Cluster_ORIGINAL_DST {
		lbPolicy = networking.LoadBalancerSettings_PASSTHROUGH
	}
	return &networking.TrafficPolicy{
		LoadBalancer: &networking.LoadBalancerSettings{
			LbPolicy: &networking.LoadBalancerSettings_Simple{
				Simple: lbPolicy,
			},
		},
		ConnectionPool: &networking.ConnectionPoolSettings{
			Tcp: &networking.ConnectionPoolSettings_TCPSettings{
				ConnectTimeout: &types.Duration{
					Seconds: cb.push.Mesh.ConnectTimeout.Seconds,
					Nanos:   cb.push.Mesh.ConnectTimeout.Nanos,
				},
			},
		},
	}
}

// castDestinationRuleOrDefault returns the destination rule enclosed by the config, if not null.
// Otherwise, return default (empty) DR.
func castDestinationRuleOrDefault(config *model.Config) *networking.DestinationRule {
	if config != nil {
		return config.Spec.(*networking.DestinationRule)
	}

	return &defaultDestinationRule
}

// maybeApplyEdsConfig applies EdsClusterConfig on the passed in cluster if it is an EDS type of cluster.
func maybeApplyEdsConfig(cluster *apiv2.Cluster) {
	switch v := cluster.ClusterDiscoveryType.(type) {
	case *apiv2.Cluster_Type:
		if v.Type != apiv2.Cluster_EDS {
			return
		}
	}
	cluster.EdsClusterConfig = &apiv2.Cluster_EdsClusterConfig{
		ServiceName: cluster.Name,
		EdsConfig: &core.ConfigSource{
			ConfigSourceSpecifier: &core.ConfigSource_Ads{
				Ads: &core.AggregatedConfigSource{},
			},
			InitialFetchTimeout: features.InitialFetchTimeout,
		},
	}
}
