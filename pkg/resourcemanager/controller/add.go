// Copyright (c) 2022 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/wait"
	kubernetesclientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/clock"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener/pkg/resourcemanager/apis/config"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/csrapprover"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/garbagecollector"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/health"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/managedresource"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/rootcapublisher"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/secret"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/tokeninvalidator"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/tokenrequestor"
	managerpredicate "github.com/gardener/gardener/pkg/resourcemanager/predicate"
)

// AddToManager adds all controllers to the given manager.
func AddToManager(mgr manager.Manager, sourceCluster, targetCluster cluster.Cluster, cfg *config.ResourceManagerConfiguration) error {
	targetClientSet, err := kubernetesclientset.NewForConfig(targetCluster.GetConfig())
	if err != nil {
		return fmt.Errorf("failed creating Kubernetes client: %w", err)
	}

	var targetCacheDisabled bool
	if cfg.TargetClientConnection != nil {
		targetCacheDisabled = pointer.BoolDeref(cfg.TargetClientConnection.DisableCachedClient, false)
	}

	if cfg.Controllers.KubeletCSRApprover.Enabled {
		if err := (&csrapprover.Reconciler{
			CertificatesClient: targetClientSet.CertificatesV1().CertificateSigningRequests(),
			Config:             cfg.Controllers.KubeletCSRApprover,
			SourceNamespace:    *cfg.SourceClientConnection.Namespace,
		}).AddToManager(mgr, sourceCluster, targetCluster); err != nil {
			return fmt.Errorf("failed adding Kubelet CSR Approver controller: %w", err)
		}
	}

	if cfg.Controllers.GarbageCollector.Enabled {
		if err := (&garbagecollector.Reconciler{
			Config: cfg.Controllers.GarbageCollector,
			Clock:  clock.RealClock{},
		}).AddToManager(mgr, targetCluster); err != nil {
			return fmt.Errorf("failed adding garbage collector controller: %w", err)
		}
	}

	if err := health.AddToManager(mgr, sourceCluster, targetCluster, *cfg, targetCacheDisabled); err != nil {
		return fmt.Errorf("failed adding health controller: %w", err)
	}

	if err := (&managedresource.Reconciler{
		Config:                    cfg.Controllers.ManagedResource,
		ClassFilter:               managerpredicate.NewClassFilter(*cfg.Controllers.ResourceClass),
		ClusterID:                 *cfg.Controllers.ClusterID,
		GarbageCollectorActivated: cfg.Controllers.GarbageCollector.Enabled,
	}).AddToManager(mgr, sourceCluster, targetCluster); err != nil {
		return fmt.Errorf("failed adding managed resource controller: %w", err)
	}

	if cfg.Controllers.RootCAPublisher.Enabled {
		if err := (&rootcapublisher.Reconciler{
			Config: cfg.Controllers.RootCAPublisher,
		}).AddToManager(mgr, targetCluster); err != nil {
			return fmt.Errorf("failed adding root CA publisher controller: %w", err)
		}
	}

	if err := (&secret.Reconciler{
		Config:      cfg.Controllers.Secret,
		ClassFilter: managerpredicate.NewClassFilter(*cfg.Controllers.ResourceClass),
	}).AddToManager(mgr, targetCluster); err != nil {
		return fmt.Errorf("failed adding secret controller: %w", err)
	}

	if cfg.Controllers.TokenInvalidator.Enabled {
		if err := (&tokeninvalidator.Reconciler{
			Config: cfg.Controllers.TokenInvalidator,
		}).AddToManager(mgr, targetCluster); err != nil {
			return fmt.Errorf("failed adding token invalidator controller: %w", err)
		}
	}

	if cfg.Controllers.TokenRequestor.Enabled {
		if err := (&tokenrequestor.Reconciler{
			Config:     cfg.Controllers.TokenRequestor,
			Clock:      clock.RealClock{},
			JitterFunc: wait.Jitter,
		}).AddToManager(mgr, sourceCluster, targetCluster); err != nil {
			return fmt.Errorf("failed adding token requestor controller: %w", err)
		}
	}

	return nil
}
