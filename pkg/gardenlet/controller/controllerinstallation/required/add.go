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

package required

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/gardener/gardener/pkg/api/extensions"
	"github.com/gardener/gardener/pkg/apis/core"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/controllerutils/mapper"
)

// ControllerName is the name of this controller.
const ControllerName = "controllerinstallation-required"

// AddToManager adds Reconciler to the given manager.
func (r *Reconciler) AddToManager(mgr manager.Manager, gardenCluster, seedCluster cluster.Cluster) error {
	if r.GardenClient == nil {
		r.GardenClient = gardenCluster.GetClient()
	}
	if r.SeedClient == nil {
		r.SeedClient = seedCluster.GetClient()
	}
	r.Lock = &sync.RWMutex{}
	r.KindToRequiredTypes = make(map[string]sets.String)

	// It's not possible to overwrite the event handler when using the controller builder. Hence, we have to build up
	// the controller manually.
	c, err := controller.New(
		ControllerName,
		mgr,
		controller.Options{
			Reconciler:              r,
			MaxConcurrentReconciles: pointer.IntDeref(r.Config.ConcurrentSyncs, 0),
			RecoverPanic:            true,
		},
	)
	if err != nil {
		return err
	}

	for _, extension := range []struct {
		objectKind        string
		object            client.Object
		newObjectListFunc func() client.ObjectList
	}{
		{extensionsv1alpha1.BackupBucketResource, &extensionsv1alpha1.BackupBucket{}, func() client.ObjectList { return &extensionsv1alpha1.BackupBucketList{} }},
		{extensionsv1alpha1.BackupEntryResource, &extensionsv1alpha1.BackupEntry{}, func() client.ObjectList { return &extensionsv1alpha1.BackupEntryList{} }},
		{extensionsv1alpha1.BastionResource, &extensionsv1alpha1.Bastion{}, func() client.ObjectList { return &extensionsv1alpha1.BastionList{} }},
		{extensionsv1alpha1.ContainerRuntimeResource, &extensionsv1alpha1.ContainerRuntime{}, func() client.ObjectList { return &extensionsv1alpha1.ContainerRuntimeList{} }},
		{extensionsv1alpha1.ControlPlaneResource, &extensionsv1alpha1.ControlPlane{}, func() client.ObjectList { return &extensionsv1alpha1.ControlPlaneList{} }},
		{extensionsv1alpha1.DNSRecordResource, &extensionsv1alpha1.DNSRecord{}, func() client.ObjectList { return &extensionsv1alpha1.DNSRecordList{} }},
		{extensionsv1alpha1.ExtensionResource, &extensionsv1alpha1.Extension{}, func() client.ObjectList { return &extensionsv1alpha1.ExtensionList{} }},
		{extensionsv1alpha1.InfrastructureResource, &extensionsv1alpha1.Infrastructure{}, func() client.ObjectList { return &extensionsv1alpha1.InfrastructureList{} }},
		{extensionsv1alpha1.NetworkResource, &extensionsv1alpha1.Network{}, func() client.ObjectList { return &extensionsv1alpha1.NetworkList{} }},
		{extensionsv1alpha1.OperatingSystemConfigResource, &extensionsv1alpha1.OperatingSystemConfig{}, func() client.ObjectList { return &extensionsv1alpha1.OperatingSystemConfigList{} }},
		{extensionsv1alpha1.WorkerResource, &extensionsv1alpha1.Worker{}, func() client.ObjectList { return &extensionsv1alpha1.WorkerList{} }},
	} {
		eventHandler := mapper.EnqueueRequestsFrom(
			r.MapObjectKindToControllerInstallations(extension.objectKind, extension.newObjectListFunc),
			mapper.UpdateWithNew,
			c.GetLogger(),
		)

		// Execute the mapper function at least once to initialize the `KindToRequiredTypes` map.
		// This is necessary for extension kinds which are registered but for which no extension objects exist in the
		// seed (e.g. when backups are disabled). In such cases, no regular watch event would be triggered. Hence, the
		// mapping function would never be executed. Hence, the extension kind would never be part of the
		// `KindToRequiredTypes` map. Hence, the reconciler would not be able to decide whether the
		// ControllerInstallation is required.
		if err = c.Watch(controllerutils.HandleOnce, eventHandler); err != nil {
			return err
		}

		if err := c.Watch(source.NewKindWithCache(extension.object, seedCluster.GetCache()), eventHandler, r.ObjectPredicate()); err != nil {
			return err
		}
	}

	return nil
}

// ObjectPredicate returns true for 'create' and 'update' events. For updates, it only returns true when the extension
// type has changed.
func (r *Reconciler) ObjectPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(_ event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// enqueue on periodic cache resyncs
			if e.ObjectOld.GetResourceVersion() == e.ObjectNew.GetResourceVersion() {
				return true
			}

			extensionObj, ok := e.ObjectNew.(extensionsv1alpha1.Object)
			if !ok {
				return false
			}

			oldExtensionObj, ok := e.ObjectOld.(extensionsv1alpha1.Object)
			if !ok {
				return false
			}

			return oldExtensionObj.GetExtensionSpec().GetExtensionType() != extensionObj.GetExtensionSpec().GetExtensionType()
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
}

// MapObjectKindToControllerInstallations returns a mapper function for the given extension kind that lists all existing
// extension resources of the given kind and stores the respective types in the `KindToRequiredTypes` map. Afterwards,
// it enqueue all ControllerInstallations for the seed that are referring to ControllerRegistrations responsible for
// the given kind.
// The returned reconciler doesn't care about which object was created/updated/deleted, it just cares about being
// triggered when some object of the kind, it is responsible for, is created/updated/deleted.
func (r *Reconciler) MapObjectKindToControllerInstallations(objectKind string, newObjectListFunc func() client.ObjectList) mapper.MapFunc {
	return func(ctx context.Context, log logr.Logger, _ client.Reader, _ client.Object) []reconcile.Request {
		log = log.WithValues("extensionKind", objectKind)

		listObj := newObjectListFunc()
		if err := r.SeedClient.List(ctx, listObj); err != nil && !meta.IsNoMatchError(err) {
			// Let's ignore bootstrap situations where extension CRDs were not yet applied. They will be deployed
			// eventually by the seed controller.
			log.Error(err, "Failed to list extension objects")
			return nil
		}

		r.Lock.RLock()
		oldRequiredTypes, kindCalculated := r.KindToRequiredTypes[objectKind]
		r.Lock.RUnlock()
		newRequiredTypes := sets.NewString()

		if err := meta.EachListItem(listObj, func(o runtime.Object) error {
			obj, err := extensions.Accessor(o)
			if err != nil {
				return err
			}

			newRequiredTypes.Insert(obj.GetExtensionSpec().GetExtensionType())
			return nil
		}); err != nil {
			log.Error(err, "Failed while iterating over extension objects")
			return nil
		}

		// if there is no difference compared to before then exit early
		if kindCalculated && oldRequiredTypes.Equal(newRequiredTypes) {
			return nil
		}

		r.Lock.Lock()
		r.KindToRequiredTypes[objectKind] = newRequiredTypes
		r.Lock.Unlock()

		// Step 2: List all existing controller registrations and filter for those that are supporting resources for the
		// extension kind this particular reconciler is responsible for.

		controllerRegistrationList := &gardencorev1beta1.ControllerRegistrationList{}
		if err := r.GardenClient.List(ctx, controllerRegistrationList); err != nil {
			log.Error(err, "Failed to list ControllerRegistrations")
			return nil
		}

		controllerRegistrationNamesForKind := sets.NewString()
		for _, controllerRegistration := range controllerRegistrationList.Items {
			for _, resource := range controllerRegistration.Spec.Resources {
				if resource.Kind == objectKind {
					controllerRegistrationNamesForKind.Insert(controllerRegistration.Name)
					break
				}
			}
		}

		// Step 3: List all existing controller installation objects for the seed cluster this controller is responsible
		// for and filter for those that reference registrations collected above. Then requeue those installations for
		// the other reconciler to decide whether it is required or not.

		controllerInstallationList := &gardencorev1beta1.ControllerInstallationList{}
		if err := r.GardenClient.List(ctx, controllerInstallationList, client.MatchingFields{core.SeedRefName: r.SeedName}); err != nil {
			log.Error(err, "Failed to list ControllerInstallations")
			return nil
		}

		var requests []reconcile.Request

		for _, obj := range controllerInstallationList.Items {
			if !controllerRegistrationNamesForKind.Has(obj.Spec.RegistrationRef.Name) {
				continue
			}

			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: obj.Name}})
		}

		return requests
	}
}
