// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package tokeninvalidator

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/gardener/gardener/pkg/resourcemanager/apis/config"
)

// Reconciler labels secrets whose tokens should be invalidated.
type Reconciler struct {
	TargetClient client.Client
	TargetReader client.Reader
	Config       config.TokenInvalidatorControllerConfig

	// RateLimiter allows limiting exponential backoff for testing purposes
	RateLimiter ratelimiter.RateLimiter
}

// Reconcile labels secrets whose tokens should be invalidated.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	if err := r.TargetClient.Get(ctx, request.NamespacedName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error retrieving object from store: %w", err)
	}

	serviceAccount := &corev1.ServiceAccount{}
	if err := r.TargetClient.Get(ctx, client.ObjectKey{Namespace: secret.Namespace, Name: secret.Annotations[corev1.ServiceAccountNameKey]}, serviceAccount); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not fetch ServiceAccount: %w", err)
	}

	if !metav1.HasLabel(secret.ObjectMeta, resourcesv1alpha1.ResourceManagerPurpose) {
		if err := r.addPurposeLabel(ctx, secret); err != nil {
			log.Info("Adding 'purpose' label")
			return reconcile.Result{}, err
		}
	}

	if metav1.HasLabel(serviceAccount.ObjectMeta, resourcesv1alpha1.StaticTokenSkip) ||
		serviceAccount.AutomountServiceAccountToken == nil ||
		*serviceAccount.AutomountServiceAccountToken {

		log.Info("Removing 'consider' label since secret is either explicitly skipped or `.automountServiceAccountToken` != false")
		return reconcile.Result{}, r.removeConsiderLabel(ctx, secret)
	}

	if metav1.HasLabel(secret.ObjectMeta, resourcesv1alpha1.StaticTokenConsider) {
		log.Info("Secret already has 'consider' label, nothing to be done")
		return reconcile.Result{}, nil
	}

	podList := &corev1.PodList{}
	if err := r.TargetReader.List(ctx, podList, client.InNamespace(secret.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not list Pods: %w", err)
	}

	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.Secret != nil && volume.Secret.SecretName == secret.Name {
				log.Info("Requeueing since there is still at least one pod mounting secret", "pod", client.ObjectKeyFromObject(&pod))
				return reconcile.Result{Requeue: true}, nil
			}
		}
	}

	log.Info("Adding 'consider' label")
	return reconcile.Result{}, r.addConsiderLabel(ctx, secret)
}

func (r *Reconciler) addPurposeLabel(ctx context.Context, secret *metav1.PartialObjectMetadata) error {
	return r.patchSecret(ctx, secret, func() {
		metav1.SetMetaDataLabel(&secret.ObjectMeta, resourcesv1alpha1.ResourceManagerPurpose, resourcesv1alpha1.LabelPurposeTokenInvalidation)
	})
}

func (r *Reconciler) addConsiderLabel(ctx context.Context, secret *metav1.PartialObjectMetadata) error {
	return r.patchSecret(ctx, secret, func() {
		metav1.SetMetaDataLabel(&secret.ObjectMeta, resourcesv1alpha1.StaticTokenConsider, "true")
	})
}

func (r *Reconciler) removeConsiderLabel(ctx context.Context, secret *metav1.PartialObjectMetadata) error {
	return r.patchSecret(ctx, secret, func() {
		delete(secret.Labels, resourcesv1alpha1.StaticTokenConsider)
	})
}

func (r *Reconciler) patchSecret(ctx context.Context, secret *metav1.PartialObjectMetadata, transform func()) error {
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	transform()
	return r.TargetClient.Patch(ctx, secret, patch)
}
