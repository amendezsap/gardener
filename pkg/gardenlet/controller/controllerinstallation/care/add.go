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

package care

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener/pkg/controllerutils/mapper"
	predicateutils "github.com/gardener/gardener/pkg/controllerutils/predicate"
	"github.com/gardener/gardener/pkg/gardenlet/controller/controllerinstallation/utils"
)

// ControllerName is the name of this controller.
const ControllerName = "controllerinstallation-care"

// AddToManager adds Reconciler to the given manager.
func (r *Reconciler) AddToManager(mgr manager.Manager, gardenCluster, seedCluster cluster.Cluster) error {
	if r.GardenClient == nil {
		r.GardenClient = gardenCluster.GetClient()
	}
	if r.SeedClient == nil {
		r.SeedClient = seedCluster.GetClient()
	}
	if r.GardenNamespace == "" {
		r.GardenNamespace = v1beta1constants.GardenNamespace
	}

	// It's not possible to overwrite the event handler when using the controller builder. Hence, we have to build up
	// the controller manually.
	c, err := controller.New(
		ControllerName,
		mgr,
		controller.Options{
			Reconciler:              r,
			MaxConcurrentReconciles: pointer.IntDeref(r.Config.ConcurrentSyncs, 0),
			RecoverPanic:            true,
			// if going into exponential backoff, wait at most the configured sync period
			RateLimiter: workqueue.NewWithMaxWaitRateLimiter(workqueue.DefaultControllerRateLimiter(), r.Config.SyncPeriod.Duration),
		},
	)
	if err != nil {
		return err
	}

	if err := c.Watch(
		source.NewKindWithCache(&gardencorev1beta1.ControllerInstallation{}, gardenCluster.GetCache()),
		&handler.EnqueueRequestForObject{},
		predicateutils.ForEventTypes(predicateutils.Create),
	); err != nil {
		return err
	}

	return c.Watch(
		source.NewKindWithCache(&resourcesv1alpha1.ManagedResource{}, seedCluster.GetCache()),
		mapper.EnqueueRequestsFrom(mapper.MapFunc(r.MapManagedResourceToControllerInstallation), mapper.UpdateWithNew, c.GetLogger()),
		r.IsExtensionDeployment(),
		predicateutils.ManagedResourceConditionsChanged(),
	)
}

// IsExtensionDeployment returns a predicate which evaluates to true in case the object is in the garden namespace and
// the 'controllerinstallation-name' label is present.
func (r *Reconciler) IsExtensionDeployment() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == v1beta1constants.GardenNamespace &&
			obj.GetLabels()[utils.LabelKeyControllerInstallationName] != ""
	})
}

// MapManagedResourceToControllerInstallation is a mapper.MapFunc for mapping a ManagedResource to the owning
// ControllerInstallation.
func (r *Reconciler) MapManagedResourceToControllerInstallation(_ context.Context, _ logr.Logger, _ client.Reader, obj client.Object) []reconcile.Request {
	managedResource, ok := obj.(*resourcesv1alpha1.ManagedResource)
	if !ok {
		return nil
	}

	controllerInstallationName, ok := managedResource.Labels[utils.LabelKeyControllerInstallationName]
	if !ok || len(controllerInstallationName) == 0 {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: controllerInstallationName}}}
}
