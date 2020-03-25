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

package namespace

import (
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/framework/components/environment/native"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/framework/resource/environment"
)

// Config contains configuration information about the namespace instance
type Config struct {
	// Prefix to use for autogenerated namespace name
	Prefix string
	// Inject indicates whether to add sidecar injection label to this namespace
	Inject bool
	// Revision is the namespace of custom injector instance
	Revision string
	// Labels to be applied to namespace
	Labels map[string]string
}

// Instance represents an allocated namespace that can be used to create config, or deploy components in.
type Instance interface {
	Name() string
}

// Claim an existing namespace in all clusters, or create a new one if doesn't exist.
func Claim(ctx resource.Context, name string) (i Instance, err error) {
	err = resource.UnsupportedEnvironment(ctx.Environment())
	ctx.Environment().Case(environment.Native, func() {
		i = claimNative(ctx, name)
		err = nil
	})
	ctx.Environment().Case(environment.Kube, func() {
		i, err = claimKube(ctx, name)
	})
	return
}

// ClaimOrFail calls Claim and fails test if it returns error
func ClaimOrFail(t test.Failer, ctx resource.Context, name string) Instance {
	t.Helper()
	i, err := Claim(ctx, name)
	if err != nil {
		t.Fatalf("namespace.ClaimOrFail:: %v", err)
	}
	return i
}

// New creates a new Namespace in all clusters.
func New(ctx resource.Context, nsConfig Config) (i Instance, err error) {
	err = resource.UnsupportedEnvironment(ctx.Environment())
	ctx.Environment().Case(environment.Native, func() {
		i = newNative(ctx, nsConfig.Prefix, nsConfig.Inject)
		err = nil
	})
	ctx.Environment().Case(environment.Kube, func() {
		i, err = newKube(ctx, &nsConfig)
	})
	return
}

// NewOrFail calls New and fails test if it returns error
func NewOrFail(t test.Failer, ctx resource.Context, nsConfig Config) Instance {
	t.Helper()
	i, err := New(ctx, nsConfig)
	if err != nil {
		t.Fatalf("namespace.NewOrFail: %v", err)
	}
	return i
}

// ClaimSystemNamespace retrieves the namespace for the Istio system components from the environment.
func ClaimSystemNamespace(ctx resource.Context) (Instance, error) {
	switch ctx.Environment().EnvironmentName() {
	case environment.Kube:
		istioCfg, err := istio.DefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		return Claim(ctx, istioCfg.SystemNamespace)
	case environment.Native:
		ns := ctx.Environment().(*native.Environment).SystemNamespace
		return Claim(ctx, ns)
	default:
		return nil, resource.UnsupportedEnvironment(ctx.Environment())
	}
}

// ClaimSystemNamespaceOrFail calls ClaimSystemNamespace, failing the test if an error occurs.
func ClaimSystemNamespaceOrFail(t test.Failer, ctx resource.Context) Instance {
	t.Helper()
	i, err := ClaimSystemNamespace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
