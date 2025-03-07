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
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/gardenlet/apis/config"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
)

// Reconciler reconciles ControllerInstallations. It checks whether they are still required by using the
// <KindToRequiredTypes> map.
type Reconciler struct {
	GardenClient client.Client
	SeedClient   client.Client
	Config       config.ControllerInstallationRequiredControllerConfiguration
	SeedName     string

	Lock                *sync.RWMutex
	KindToRequiredTypes map[string]sets.String
}

// Reconcile performs the main reconciliation logic.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	controllerInstallation := &gardencorev1beta1.ControllerInstallation{}
	if err := r.GardenClient.Get(ctx, request.NamespacedName, controllerInstallation); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error retrieving object from store: %w", err)
	}

	controllerRegistration := &gardencorev1beta1.ControllerRegistration{}
	if err := r.GardenClient.Get(ctx, kutil.Key(controllerInstallation.Spec.RegistrationRef.Name), controllerRegistration); err != nil {
		return reconcile.Result{}, err
	}

	var (
		allKindsCalculated = true
		required           *bool
		requiredKindTypes  = sets.NewString()
		message            string
	)

	r.Lock.RLock()
	for _, resource := range controllerRegistration.Spec.Resources {
		requiredTypes, ok := r.KindToRequiredTypes[resource.Kind]
		if !ok {
			allKindsCalculated = false
			continue
		}

		if requiredTypes.Has(resource.Type) {
			required = pointer.Bool(true)
			requiredKindTypes.Insert(fmt.Sprintf("%s/%s", resource.Kind, resource.Type))
		}
	}
	r.Lock.RUnlock()

	if required == nil {
		if !allKindsCalculated {
			// if required wasn't set yet then but not all kinds were calculated then the it's not possible to
			// decide yet whether it's required or not
			return reconcile.Result{}, nil
		}

		// if required wasn't set yet then but all kinds were calculated then the installation is no longer required
		required = pointer.Bool(false)
		message = "no extension objects exist in the seed having the kind/type combinations the controller is responsible for"
	} else if *required {
		message = fmt.Sprintf("extension objects still exist in the seed: %+v", requiredKindTypes.UnsortedList())
	}

	if err := updateControllerInstallationRequiredCondition(ctx, r.GardenClient, controllerInstallation, *required, message); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func updateControllerInstallationRequiredCondition(ctx context.Context, c client.StatusClient, controllerInstallation *gardencorev1beta1.ControllerInstallation, required bool, message string) error {
	var (
		conditionRequired = gardencorev1beta1helper.GetOrInitCondition(controllerInstallation.Status.Conditions, gardencorev1beta1.ControllerInstallationRequired)

		status = gardencorev1beta1.ConditionTrue
		reason = "ExtensionObjectsExist"
	)

	if !required {
		status = gardencorev1beta1.ConditionFalse
		reason = "NoExtensionObjects"
	}

	patch := client.StrategicMergeFrom(controllerInstallation.DeepCopy())
	controllerInstallation.Status.Conditions = gardencorev1beta1helper.MergeConditions(
		controllerInstallation.Status.Conditions,
		gardencorev1beta1helper.UpdatedCondition(conditionRequired, status, reason, message),
	)

	return c.Status().Patch(ctx, controllerInstallation, patch)
}
