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

package seed

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/gardener/gardener/charts"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	predicateutils "github.com/gardener/gardener/pkg/controllerutils/predicate"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
)

// ControllerName is the name of this controller.
const ControllerName = "seed"

// AddToManager adds Reconciler to the given manager.
func (r *Reconciler) AddToManager(mgr manager.Manager, gardenCluster cluster.Cluster) error {
	if r.GardenClient == nil {
		r.GardenClient = gardenCluster.GetClient()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(ControllerName + "-controller")
	}
	if r.GardenNamespace == "" {
		r.GardenNamespace = v1beta1constants.GardenNamespace
	}
	if r.ChartsPath == "" {
		r.ChartsPath = charts.Path
	}

	if r.ClientCertificateExpirationTimestamp == nil {
		gardenletClientCertificate, err := kutil.ClientCertificateFromRESTConfig(gardenCluster.GetConfig())
		if err != nil {
			return fmt.Errorf("failed to get gardenlet client certificate: %w", err)
		}
		r.ClientCertificateExpirationTimestamp = &metav1.Time{Time: gardenletClientCertificate.Leaf.NotAfter}
	}

	// It's not possible to overwrite the event handler when using the controller builder. Hence, we have to build up
	// the controller manually.
	c, err := controller.New(
		ControllerName,
		mgr,
		controller.Options{
			Reconciler:              r,
			MaxConcurrentReconciles: 1,
			RecoverPanic:            true,
		},
	)
	if err != nil {
		return err
	}

	c.GetLogger().Info("The client certificate used to communicate with the garden cluster has expiration date", "expirationDate", r.ClientCertificateExpirationTimestamp)

	return c.Watch(
		source.NewKindWithCache(&gardencorev1beta1.Seed{}, gardenCluster.GetCache()),
		&handler.EnqueueRequestForObject{},
		predicateutils.HasName(r.Config.SeedConfig.Name),
		predicate.GenerationChangedPredicate{},
	)
}
