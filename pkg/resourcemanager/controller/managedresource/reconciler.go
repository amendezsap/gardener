// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package managedresource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	hvpav1alpha1 "github.com/gardener/hvpa-controller/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/hashicorp/go-multierror"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	resourceshelper "github.com/gardener/gardener/pkg/apis/resources/v1alpha1/helper"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/resourcemanager/apis/config"
	"github.com/gardener/gardener/pkg/resourcemanager/controller/garbagecollector/references"
	resourcemanagerpredicate "github.com/gardener/gardener/pkg/resourcemanager/predicate"
	errorutils "github.com/gardener/gardener/pkg/utils/errors"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
)

var (
	deletePropagationForeground = metav1.DeletePropagationForeground
	foregroundDeletionAPIGroups = sets.NewString(appsv1.GroupName, extensionsv1beta1.GroupName, batchv1.GroupName)
)

// Reconciler manages the resources reference by ManagedResources.
type Reconciler struct {
	SourceClient                  client.Client
	TargetClient                  client.Client
	TargetScheme                  *runtime.Scheme
	TargetRESTMapper              meta.RESTMapper
	Config                        config.ManagedResourceControllerConfig
	ClassFilter                   *resourcemanagerpredicate.ClassFilter
	ClusterID                     string
	GarbageCollectorActivated     bool
	RequeueAfterOnDeletionPending *time.Duration
}

// Reconcile manages the resources reference by ManagedResources.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	mr := &resourcesv1alpha1.ManagedResource{}
	if err := r.SourceClient.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error retrieving object from store: %w", err)
	}

	if ignore(mr) && mr.DeletionTimestamp == nil {
		log.Info("Skipping reconciliation since ManagedResource is ignored")
		if err := r.updateConditionsForIgnoredManagedResource(ctx, mr); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}

		return reconcile.Result{}, nil
	}

	action, responsible := r.ClassFilter.Active(mr)
	log.Info("Reconciling ManagedResource", "actionRequired", action, "responsible", responsible)

	// If the object should be deleted or the responsibility changed
	// the actual deployments have to be deleted
	if mr.DeletionTimestamp != nil || (action && !responsible) {
		return r.delete(ctx, log, mr)
	}

	// If the deletion after a change of responsibility is still
	// pending, the handling of the object by the responsible controller
	// must be delayed, until the deletion is finished.
	if responsible && !action {
		return reconcile.Result{Requeue: true}, nil
	}
	return r.reconcile(ctx, log, mr)
}

func (r *Reconciler) reconcile(ctx context.Context, log logr.Logger, mr *resourcesv1alpha1.ManagedResource) (reconcile.Result, error) {
	log.Info("Starting to reconcile ManagedResource")

	if !controllerutil.ContainsFinalizer(mr, r.ClassFilter.FinalizerName()) {
		log.Info("Adding finalizer")
		if err := controllerutils.AddFinalizers(ctx, r.SourceClient, mr, r.ClassFilter.FinalizerName()); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	var (
		newResourcesObjects          []object
		newResourcesObjectReferences []resourcesv1alpha1.ObjectReference
		orphanedObjectReferences     []resourcesv1alpha1.ObjectReference

		equivalences           = NewEquivalences(mr.Spec.Equivalences...)
		existingResourcesIndex = NewObjectIndex(mr.Status.Resources, equivalences)
		origin                 = resourceshelper.OriginForManagedResource(r.ClusterID, mr)

		forceOverwriteLabels      bool
		forceOverwriteAnnotations bool

		decodingErrors []*decodingError

		hash = sha256.New()
	)

	if v := mr.Spec.ForceOverwriteLabels; v != nil {
		forceOverwriteLabels = *v
	}
	if v := mr.Spec.ForceOverwriteAnnotations; v != nil {
		forceOverwriteAnnotations = *v
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// Initialize condition based on the current status.
	conditionResourcesApplied := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesApplied)

	for _, ref := range mr.Spec.SecretRefs {
		secret := &corev1.Secret{}
		if err := r.SourceClient.Get(reconcileCtx, client.ObjectKey{Namespace: mr.Namespace, Name: ref.Name}, secret); err != nil {
			conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionFalse, "CannotReadSecret", err.Error())
			if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
				return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
			}

			return reconcile.Result{}, fmt.Errorf("could not read secret '%s': %+v", secret.Name, err)
		}

		// Sort secret's data key to keep consistent ordering while calculating checksum
		secretKeys := make([]string, 0, len(secret.Data))
		for secretKey := range secret.Data {
			secretKeys = append(secretKeys, secretKey)
		}
		sort.Strings(secretKeys)

		for _, secretKey := range secretKeys {
			value := secret.Data[secretKey]
			var (
				decoder    = yaml.NewYAMLOrJSONDecoder(bytes.NewReader(value), 1024)
				decodedObj map[string]interface{}
			)

			for indexInFile := 0; true; indexInFile++ {
				objLog := log.WithValues("secret", client.ObjectKeyFromObject(secret), "secretKey", secretKey, "indexInFile", indexInFile)

				err := decoder.Decode(&decodedObj)
				if err == io.EOF {
					break
				}
				if err != nil {
					dErr := &decodingError{
						err:         err,
						secret:      client.ObjectKeyFromObject(secret),
						secretKey:   secretKey,
						indexInFile: indexInFile,
					}
					decodingErrors = append(decodingErrors, dErr)
					objLog.Error(dErr.err, "Could not decode resource")
					continue
				}

				if decodedObj == nil {
					continue
				}

				obj := &unstructured.Unstructured{Object: decodedObj}
				objLog = objLog.WithValues("object", client.Object(obj))

				// look up scope of objects' kind to check, if we should default the namespace field
				mapping, err := r.TargetRESTMapper.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
				if err != nil || mapping == nil {
					// Cache miss most probably indicates, that the corresponding CRD is not yet applied.
					// CRD might be applied later as part of the ManagedResource reconciliation

					errMsg := "<nil>"
					if err != nil {
						errMsg = err.Error()
					}
					objLog.Info("Could not get RESTMapping for object", "err", errMsg)

					// default namespace on a best effort basis
					if obj.GetKind() != "Namespace" && obj.GetNamespace() == "" {
						obj.SetNamespace(metav1.NamespaceDefault)
					}
				} else {
					if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
						// default namespace field to `default` in case of namespaced kinds
						if obj.GetNamespace() == "" {
							obj.SetNamespace(metav1.NamespaceDefault)
						}
					} else {
						// unset namespace field in case of non-namespaced kinds
						obj.SetNamespace("")
					}
				}

				var (
					newObj = object{
						obj:                       obj,
						forceOverwriteLabels:      forceOverwriteLabels,
						forceOverwriteAnnotations: forceOverwriteAnnotations,
					}
					objectReference = resourcesv1alpha1.ObjectReference{
						ObjectReference: corev1.ObjectReference{
							APIVersion: newObj.obj.GetAPIVersion(),
							Kind:       newObj.obj.GetKind(),
							Name:       newObj.obj.GetName(),
							Namespace:  newObj.obj.GetNamespace(),
						},
						Labels:      mergeMaps(newObj.obj.GetLabels(), mr.Spec.InjectLabels),
						Annotations: newObj.obj.GetAnnotations(),
					}
				)

				objectReference.Labels[resourcesv1alpha1.ManagedBy] = *r.Config.ManagedByLabelValue

				var found bool
				newObj.oldInformation, found = existingResourcesIndex.Lookup(objectReference)
				decodedObj = nil

				if ignoreMode(obj) {
					if found {
						orphanedObjectReferences = append(orphanedObjectReferences, objectReference)
					}

					objLog.Info("Skipping object because it is marked to be ignored")
					continue
				}

				hash.Write(value)
				newResourcesObjects = append(newResourcesObjects, newObj)
				newResourcesObjectReferences = append(newResourcesObjectReferences, objectReference)
			}
		}
	}

	// calculate the checksum for the referenced secrets data.
	secretsDataChecksum := hex.EncodeToString(hash.Sum(nil))

	// sort object references before updating status, to keep consistent ordering
	// (otherwise, the order will be different on each update)
	sortObjectReferences(newResourcesObjectReferences)

	// invalidate conditions, if resources have been added/removed from the managed resource
	if !apiequality.Semantic.DeepEqual(mr.Status.Resources, newResourcesObjectReferences) || mr.Status.SecretsDataChecksum == nil || *mr.Status.SecretsDataChecksum != secretsDataChecksum {
		conditionResourcesHealthy := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesHealthy)
		conditionResourcesHealthy = v1beta1helper.UpdatedCondition(conditionResourcesHealthy, gardencorev1beta1.ConditionUnknown,
			resourcesv1alpha1.ConditionChecksPending, "The health checks have not yet been executed for the current set of resources.")
		conditionResourcesProgressing := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesProgressing)
		conditionResourcesProgressing = v1beta1helper.UpdatedCondition(conditionResourcesProgressing, gardencorev1beta1.ConditionUnknown,
			resourcesv1alpha1.ConditionChecksPending, "Checks have not yet been executed for the current set of resources.")

		reason := resourcesv1alpha1.ConditionApplyProgressing
		msg := "The resources are currently being reconciled."
		switch conditionResourcesApplied.Reason {
		case resourcesv1alpha1.ConditionApplyFailed, resourcesv1alpha1.ConditionDeletionFailed, resourcesv1alpha1.ConditionDeletionPending:
			// keep condition reason and message if last reconciliation failed
			reason = conditionResourcesApplied.Reason
			msg = conditionResourcesApplied.Message
		}
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionProgressing, reason, msg)

		if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesHealthy, conditionResourcesProgressing, conditionResourcesApplied); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}
	}

	if deletionPending, err := r.cleanOldResources(reconcileCtx, log, mr, existingResourcesIndex); err != nil {
		var (
			reason string
			status gardencorev1beta1.ConditionStatus
		)
		if deletionPending {
			reason = resourcesv1alpha1.ConditionDeletionPending
			status = gardencorev1beta1.ConditionProgressing
			log.Info("Deletion is still pending", "err", err)
		} else {
			reason = resourcesv1alpha1.ConditionDeletionFailed
			status = gardencorev1beta1.ConditionFalse
			log.Error(err, "Deletion of old resources failed")
		}

		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, status, reason, err.Error())
		if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}

		if deletionPending {
			return reconcile.Result{RequeueAfter: *r.RequeueAfterOnDeletionPending}, nil
		} else {
			return reconcile.Result{}, err
		}
	}

	if err := r.releaseOrphanedResources(ctx, log, orphanedObjectReferences, origin); err != nil {
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionFalse, resourcesv1alpha1.ReleaseOfOrphanedResourcesFailed, err.Error())
		if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}

		return reconcile.Result{}, fmt.Errorf("could not release all orphaned resources: %+v", err)
	}

	injectLabels := mergeMaps(mr.Spec.InjectLabels, map[string]string{resourcesv1alpha1.ManagedBy: *r.Config.ManagedByLabelValue})
	if err := r.applyNewResources(reconcileCtx, log, origin, newResourcesObjects, injectLabels, equivalences); err != nil {
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionFalse, resourcesv1alpha1.ConditionApplyFailed, err.Error())
		if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}

		return reconcile.Result{}, fmt.Errorf("could not apply all new resources: %+v", err)
	}

	if len(decodingErrors) != 0 {
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionFalse, resourcesv1alpha1.ConditionDecodingFailed, fmt.Sprintf("Could not decode all new resources: %v", decodingErrors))
	} else {
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionTrue, resourcesv1alpha1.ConditionApplySucceeded, "All resources are applied.")
	}

	if err := updateManagedResourceStatus(ctx, r.SourceClient, mr, &secretsDataChecksum, newResourcesObjectReferences, conditionResourcesApplied); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
	}

	log.Info("Finished to reconcile ManagedResource")
	return reconcile.Result{RequeueAfter: r.Config.SyncPeriod.Duration}, nil
}

func (r *Reconciler) delete(ctx context.Context, log logr.Logger, mr *resourcesv1alpha1.ManagedResource) (reconcile.Result, error) {
	log.Info("Starting to delete ManagedResource")

	deleteCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	if err := r.updateConditionsForDeletion(ctx, mr); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
	}

	conditionResourcesApplied := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesApplied)

	if keepObjects := mr.Spec.KeepObjects; keepObjects == nil || !*keepObjects {
		existingResourcesIndex := NewObjectIndex(mr.Status.Resources, nil)

		msg := "The resources are currently being deleted."
		switch conditionResourcesApplied.Reason {
		case resourcesv1alpha1.ConditionDeletionPending, resourcesv1alpha1.ConditionDeletionFailed:
			// keep condition message if deletion is pending / failed
			msg = conditionResourcesApplied.Message
		}
		conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionProgressing, resourcesv1alpha1.ConditionDeletionPending, msg)
		if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
		}

		if deletionPending, err := r.cleanOldResources(deleteCtx, log, mr, existingResourcesIndex); err != nil {
			var (
				reason string
				status gardencorev1beta1.ConditionStatus
			)
			if deletionPending {
				reason = resourcesv1alpha1.ConditionDeletionPending
				status = gardencorev1beta1.ConditionProgressing
				log.Info("Deletion is still pending", "err", err)
			} else {
				reason = resourcesv1alpha1.ConditionDeletionFailed
				status = gardencorev1beta1.ConditionFalse
				log.Error(err, "Deletion of all resources failed")
			}

			conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, status, reason, err.Error())
			if err := updateConditions(ctx, r.SourceClient, mr, conditionResourcesApplied); err != nil {
				return reconcile.Result{}, fmt.Errorf("could not update the ManagedResource status: %w", err)
			}

			if deletionPending {
				return reconcile.Result{RequeueAfter: *r.RequeueAfterOnDeletionPending}, nil
			} else {
				return reconcile.Result{}, err
			}
		}
	} else {
		log.Info("Skipping deletion of objects as ManagedResource is marked to keep objects")
	}

	log.Info("All resources have been deleted")

	if controllerutil.ContainsFinalizer(mr, r.ClassFilter.FinalizerName()) {
		log.Info("Removing finalizer")
		if err := controllerutils.RemoveFinalizers(ctx, r.SourceClient, mr, r.ClassFilter.FinalizerName()); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	log.Info("Finished to delete ManagedResource")
	return reconcile.Result{}, nil
}

func (r *Reconciler) updateConditionsForIgnoredManagedResource(ctx context.Context, mr *resourcesv1alpha1.ManagedResource) error {
	message := "ManagedResource is marked to be ignored."
	conditionResourcesApplied := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesApplied)
	conditionResourcesApplied = v1beta1helper.UpdatedCondition(conditionResourcesApplied, gardencorev1beta1.ConditionTrue, resourcesv1alpha1.ConditionManagedResourceIgnored, message)
	conditionResourcesHealthy := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesHealthy)
	conditionResourcesHealthy = v1beta1helper.UpdatedCondition(conditionResourcesHealthy, gardencorev1beta1.ConditionTrue, resourcesv1alpha1.ConditionManagedResourceIgnored, message)
	conditionResourcesProgressing := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesProgressing)
	conditionResourcesProgressing = v1beta1helper.UpdatedCondition(conditionResourcesProgressing, gardencorev1beta1.ConditionFalse, resourcesv1alpha1.ConditionManagedResourceIgnored, message)

	oldMr := mr.DeepCopy()
	mr.Status.Conditions = v1beta1helper.MergeConditions(mr.Status.Conditions, conditionResourcesApplied, conditionResourcesHealthy, conditionResourcesProgressing)
	if !apiequality.Semantic.DeepEqual(oldMr.Status.Conditions, mr.Status.Conditions) {
		return r.SourceClient.Status().Update(ctx, mr)
	}

	return nil
}

func (r *Reconciler) updateConditionsForDeletion(ctx context.Context, mr *resourcesv1alpha1.ManagedResource) error {
	conditionResourcesHealthy := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesHealthy)
	conditionResourcesHealthy = v1beta1helper.UpdatedCondition(conditionResourcesHealthy, gardencorev1beta1.ConditionFalse, resourcesv1alpha1.ConditionDeletionPending, "The resources are currently being deleted.")
	conditionResourcesProgressing := v1beta1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesProgressing)
	conditionResourcesProgressing = v1beta1helper.UpdatedCondition(conditionResourcesProgressing, gardencorev1beta1.ConditionTrue, resourcesv1alpha1.ConditionDeletionPending, "The resources are currently being deleted.")
	return updateConditions(ctx, r.SourceClient, mr, conditionResourcesHealthy, conditionResourcesProgressing)
}

func (r *Reconciler) applyNewResources(ctx context.Context, log logr.Logger, origin string, newResourcesObjects []object, labelsToInject map[string]string, equivalences Equivalences) error {
	newResourcesObjects = sortByKind(newResourcesObjects)

	// get all HPA and HVPA targetRefs to check if we should prevent overwriting replicas and/or resource requirements.
	// VPAs don't have to be checked, as they don't update the spec directly and only mutate Pods via a MutatingWebhook
	// and therefore don't interfere with the resource manager.
	horizontallyScaledObjects, verticallyScaledObjects, err := computeAllScaledObjectKeys(ctx, r.TargetClient)
	if err != nil {
		return fmt.Errorf("failed to compute all HPA and HVPA target ref object keys: %w", err)
	}

	for _, obj := range newResourcesObjects {
		var (
			current            = obj.obj.DeepCopy()
			resource           = unstructuredToString(obj.obj)
			scaledHorizontally = isScaled(obj.obj, horizontallyScaledObjects, equivalences)
			scaledVertically   = isScaled(obj.obj, verticallyScaledObjects, equivalences)
		)

		log.Info("Applying", "resource", resource)

		if operationResult, err := controllerutils.TypedCreateOrUpdate(ctx, r.TargetClient, r.TargetScheme, current, pointer.BoolDeref(r.Config.AlwaysUpdate, false), func() error {
			metadata, err := meta.Accessor(obj.obj)
			if err != nil {
				return fmt.Errorf("error getting metadata of object %q: %s", resource, err)
			}

			// if the ignore annotation is set to false, do nothing (ignore the resource)
			if ignore(metadata) {
				annotations := current.GetAnnotations()
				delete(annotations, descriptionAnnotation)
				current.SetAnnotations(annotations)
				return nil
			}

			if err := injectLabels(obj.obj, labelsToInject); err != nil {
				return fmt.Errorf("error injecting labels into object %q: %s", resource, err)
			}

			return merge(origin, obj.obj, current, obj.forceOverwriteLabels, obj.oldInformation.Labels, obj.forceOverwriteAnnotations, obj.oldInformation.Annotations, scaledHorizontally, scaledVertically)
		}); err != nil {
			if apierrors.IsConflict(err) {
				log.Info("Conflict while applying object", "object", resource, "err", err)
				// return conflict error directly, so that the update will be retried
				return err
			}

			if apierrors.IsInvalid(err) && operationResult == controllerutil.OperationResultUpdated && deleteOnInvalidUpdate(current) {
				if deleteErr := r.TargetClient.Delete(ctx, current); client.IgnoreNotFound(deleteErr) != nil {
					return fmt.Errorf("error deleting object %q after 'invalid' update error: %s", resource, deleteErr)
				}
				// return error directly, so that the create after delete will be retried
				return fmt.Errorf("deleted object %q because of 'invalid' update error and 'delete-on-invalid-update' annotation on object (%s)", resource, err)
			}

			return fmt.Errorf("error during apply of object %q: %s", resource, err)
		}
	}

	return nil
}

// computeAllScaledObjectKeys returns two sets containing object keys (in the form `Group/Kind/Namespace/Name`).
// The first one contains keys to objects that are horizontally scaled by either an HPA or HVPA. And the
// second one contains keys to objects that are vertically scaled by an HVPA.
// VPAs are not checked, as they don't update the spec of Deployments/StatefulSets/... and only mutate resource
// requirements via a MutatingWebhook. This way VPAs don't interfere with the resource manager and must not be considered.
func computeAllScaledObjectKeys(ctx context.Context, c client.Client) (horizontallyScaledObjects, verticallyScaledObjects sets.String, err error) {
	horizontallyScaledObjects = sets.NewString()
	verticallyScaledObjects = sets.NewString()

	// get all HPAs' targets
	hpaList := &autoscalingv1.HorizontalPodAutoscalerList{}
	if err := c.List(ctx, hpaList); err != nil && !meta.IsNoMatchError(err) {
		return horizontallyScaledObjects, verticallyScaledObjects, fmt.Errorf("failed to list all HPAs: %w", err)
	}

	for _, hpa := range hpaList.Items {
		if key, err := targetObjectKeyFromHPA(hpa); err != nil {
			return horizontallyScaledObjects, verticallyScaledObjects, err
		} else {
			horizontallyScaledObjects.Insert(key)
		}
	}

	// get all HVPAs' targets
	hvpaList := &hvpav1alpha1.HvpaList{}
	if err := c.List(ctx, hvpaList); err != nil && !meta.IsNoMatchError(err) {
		return horizontallyScaledObjects, verticallyScaledObjects, fmt.Errorf("failed to list all HVPAs: %w", err)
	}

	for _, hvpa := range hvpaList.Items {
		if key, err := targetObjectKeyFromHVPA(hvpa); err != nil {
			return horizontallyScaledObjects, verticallyScaledObjects, err
		} else {
			if hvpa.Spec.Hpa.Deploy {
				horizontallyScaledObjects.Insert(key)
			}
			if hvpa.Spec.Vpa.Deploy {
				verticallyScaledObjects.Insert(key)
			}
		}
	}

	return horizontallyScaledObjects, verticallyScaledObjects, nil
}

func targetObjectKeyFromHPA(hpa autoscalingv1.HorizontalPodAutoscaler) (string, error) {
	targetGV, err := schema.ParseGroupVersion(hpa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		return "", fmt.Errorf("invalid API version in scaleTargetReference of HorizontalPodAutoscaler '%s/%s': %w", hpa.Namespace, hpa.Name, err)
	}

	return objectKey(targetGV.Group, hpa.Spec.ScaleTargetRef.Kind, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name), nil
}

func targetObjectKeyFromHVPA(hvpa hvpav1alpha1.Hvpa) (string, error) {
	targetGV, err := schema.ParseGroupVersion(hvpa.Spec.TargetRef.APIVersion)
	if err != nil {
		return "", fmt.Errorf("invalid API version in scaleTargetReference of HorizontalPodAutoscaler '%s/%s': %w", hvpa.Namespace, hvpa.Name, err)
	}

	return objectKey(targetGV.Group, hvpa.Spec.TargetRef.Kind, hvpa.Namespace, hvpa.Spec.TargetRef.Name), nil
}

func isScaled(obj *unstructured.Unstructured, scaledObjectKeys sets.String, equivalences Equivalences) bool {
	key := objectKeyFromUnstructured(obj)

	if scaledObjectKeys.Has(key) {
		return true
	}

	// check if a HPA/HVPA targets this object via an equivalent API Group
	gk := metav1.GroupKind{
		Group: obj.GroupVersionKind().Group,
		Kind:  obj.GetKind(),
	}
	for equivalentGroupKind := range equivalences.GetEquivalencesFor(gk) {
		if scaledObjectKeys.Has(objectKey(equivalentGroupKind.Group, equivalentGroupKind.Kind, obj.GetNamespace(), obj.GetName())) {
			return true
		}
	}

	return false
}

func objectKeyFromUnstructured(o *unstructured.Unstructured) string {
	return objectKey(o.GroupVersionKind().Group, o.GetKind(), o.GetNamespace(), o.GetName())
}

func ignoreMode(meta metav1.Object) bool {
	annotations := meta.GetAnnotations()
	return annotations[resourcesv1alpha1.Mode] == resourcesv1alpha1.ModeIgnore
}

func ignore(meta metav1.Object) bool {
	return keyExistsAndValueTrue(meta.GetAnnotations(), resourcesv1alpha1.Ignore)
}

func deleteOnInvalidUpdate(meta metav1.Object) bool {
	return keyExistsAndValueTrue(meta.GetAnnotations(), resourcesv1alpha1.DeleteOnInvalidUpdate)
}

func keepObject(meta metav1.Object) bool {
	return keyExistsAndValueTrue(meta.GetAnnotations(), resourcesv1alpha1.KeepObject)
}

func isGarbageCollectableResource(meta metav1.Object) bool {
	return keyExistsAndValueTrue(meta.GetLabels(), references.LabelKeyGarbageCollectable)
}

func keyExistsAndValueTrue(kv map[string]string, key string) bool {
	if kv == nil {
		return false
	}
	val, exists := kv[key]
	valueTrue, _ := strconv.ParseBool(val)
	return exists && valueTrue
}

func (r *Reconciler) cleanOldResources(ctx context.Context, log logr.Logger, mr *resourcesv1alpha1.ManagedResource, index *objectIndex) (bool, error) {
	type output struct {
		obj             *unstructured.Unstructured
		deletionPending bool
		err             error
	}

	var (
		results         = make(chan *output)
		wg              sync.WaitGroup
		deletePVCs      = mr.Spec.DeletePersistentVolumeClaims != nil && *mr.Spec.DeletePersistentVolumeClaims
		deletionPending = false
		errorList       = &multierror.Error{
			ErrorFormat: errorutils.NewErrorFormatFuncWithPrefix("Could not clean all old resources"),
		}
	)

	for _, oldResource := range index.Objects() {
		if !index.Found(oldResource) {
			wg.Add(1)
			go func(ref resourcesv1alpha1.ObjectReference) {
				defer wg.Done()

				obj := &unstructured.Unstructured{}
				obj.SetAPIVersion(ref.APIVersion)
				obj.SetKind(ref.Kind)
				obj.SetNamespace(ref.Namespace)
				obj.SetName(ref.Name)

				resource := unstructuredToString(obj)
				log.Info("Deleting", "resource", resource)

				// get object before deleting to be able to do cleanup work for it
				if err := r.TargetClient.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
					if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
						log.Error(err, "Error during deletion", "resource", resource)
						results <- &output{obj, true, err}
						return
					}

					// resource already deleted, nothing to do here
					results <- &output{obj, false, nil}
					return
				}

				if keepObject(obj) {
					log.Info("Keeping object in the system as "+resourcesv1alpha1.KeepObject+" annotation found", "resource", unstructuredToString(obj))
					results <- &output{obj, false, nil}
					return
				}

				if r.GarbageCollectorActivated && isGarbageCollectableResource(obj) {
					log.Info("Keeping object in the system as it is marked as 'garbage-collectable'", "resource", unstructuredToString(obj))
					results <- &output{obj, false, nil}
					return
				}

				if err := cleanup(ctx, r.TargetClient, r.TargetScheme, obj, deletePVCs); err != nil {
					log.Error(err, "Error during cleanup", "resource", resource)
					results <- &output{obj, true, err}
					return
				}

				deleteOptions := &client.DeleteOptions{}

				// only delete resources in specific API groups with foreground deletion propagation
				// see https://github.com/kubernetes/kubernetes/issues/91621, https://github.com/kubernetes/kubernetes/issues/91287
				// and similar, because of which some objects (e.g `rbac/*` or `v1/Service`) cannot be deleted reliably
				// with foreground deletion propagation.
				if foregroundDeletionAPIGroups.Has(obj.GroupVersionKind().Group) {
					// delete with DeletePropagationForeground to be sure to cleanup all resources (e.g. batch/v1beta1.CronJob
					// defaults PropagationPolicy to Orphan for backwards compatibility, so it will orphan its Jobs)
					deleteOptions.PropagationPolicy = &deletePropagationForeground
				}

				if err := r.TargetClient.Delete(ctx, obj, deleteOptions); err != nil {
					if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
						log.Error(err, "Error during deletion", "resource", resource)
						results <- &output{obj, true, err}
						return
					}
					results <- &output{obj, false, nil}
					return
				}
				results <- &output{obj, true, nil}
			}(oldResource)
		}
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for out := range results {
		resource := unstructuredToString(out.obj)
		if out.deletionPending {
			deletionPending = true
			errMsg := fmt.Sprintf("deletion of old resource %q is still pending", resource)
			if out.err != nil {
				errMsg = fmt.Sprintf("%s: %v", errMsg, out.err)
			}

			// consult service events for more details
			eventsMsg, err := eventsForObject(ctx, r.TargetScheme, r.TargetClient, out.obj)
			if err != nil {
				log.Error(err, "Error reading events for more information", "resource", resource)
			} else if eventsMsg != "" {
				errMsg = fmt.Sprintf("%s\n\n%s", errMsg, eventsMsg)
			}

			errorList = multierror.Append(errorList, errors.New(errMsg))
			continue
		}

		if out.err != nil {
			errorList = multierror.Append(errorList, fmt.Errorf("error during deletion of old resource %q: %w", resource, out.err))
		}
	}

	return deletionPending, errorList.ErrorOrNil()
}

func (r *Reconciler) releaseOrphanedResources(ctx context.Context, log logr.Logger, orphanedResources []resourcesv1alpha1.ObjectReference, origin string) error {
	var (
		results   = make(chan error)
		wg        sync.WaitGroup
		errorList = &multierror.Error{
			ErrorFormat: errorutils.NewErrorFormatFuncWithPrefix("Could not release all orphaned resources"),
		}
	)

	for _, orphanedResource := range orphanedResources {
		wg.Add(1)

		go func(ref resourcesv1alpha1.ObjectReference) {
			defer wg.Done()

			err := r.releaseOrphanedResource(ctx, log, ref, origin)
			results <- err

		}(orphanedResource)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for err := range results {
		if err != nil {
			errorList = multierror.Append(errorList, err)
		}
	}

	return errorList.ErrorOrNil()
}

func (r *Reconciler) releaseOrphanedResource(ctx context.Context, log logr.Logger, ref resourcesv1alpha1.ObjectReference, origin string) error {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetKind(ref.Kind)
	obj.SetNamespace(ref.Namespace)
	obj.SetName(ref.Name)

	resource := unstructuredToString(obj)

	log.Info("Releasing orphan resource", "resource", resource)

	if err := r.TargetClient.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("error getting object %q: %w", resource, err)
		}

		return nil
	}

	// Skip the release of resource when the origin annotation has already changed
	objOrigin := obj.GetAnnotations()[resourcesv1alpha1.OriginAnnotation]
	if objOrigin != origin {
		log.Info("Skipping release for orphan resource as origin annotation has already changed", "resource", resource)
		return nil
	}

	oldObj := obj.DeepCopy()
	annotations := obj.GetAnnotations()
	delete(annotations, resourcesv1alpha1.OriginAnnotation)
	delete(annotations, descriptionAnnotation)
	obj.SetAnnotations(annotations)

	if err := r.TargetClient.Patch(ctx, obj, client.MergeFrom(oldObj)); err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("error patching object %q: %w", resource, err)
		}

		return nil
	}

	return nil
}

func eventsForObject(ctx context.Context, scheme *runtime.Scheme, c client.Client, obj *unstructured.Unstructured) (string, error) {
	var (
		relevantGKs = []schema.GroupKind{
			corev1.SchemeGroupVersion.WithKind("Service").GroupKind(),
		}
		eventLimit = 5
	)

	for _, gk := range relevantGKs {
		if gk == obj.GetObjectKind().GroupVersionKind().GroupKind() {
			return kutil.FetchEventMessages(ctx, scheme, c, obj, corev1.EventTypeWarning, eventLimit)
		}
	}
	return "", nil
}

func updateManagedResourceStatus(
	ctx context.Context,
	c client.Client,
	mr *resourcesv1alpha1.ManagedResource,
	secretsDataChecksum *string,
	resources []resourcesv1alpha1.ObjectReference,
	updatedConditions ...gardencorev1beta1.Condition,
) error {
	mr.Status.Conditions = v1beta1helper.MergeConditions(mr.Status.Conditions, updatedConditions...)
	mr.Status.SecretsDataChecksum = secretsDataChecksum
	mr.Status.Resources = resources
	mr.Status.ObservedGeneration = mr.Generation
	return c.Status().Update(ctx, mr)
}

func updateConditions(ctx context.Context, c client.Client, mr *resourcesv1alpha1.ManagedResource, conditions ...gardencorev1beta1.Condition) error {
	newConditions := v1beta1helper.MergeConditions(mr.Status.Conditions, conditions...)
	mr.Status.Conditions = newConditions
	return c.Status().Update(ctx, mr)
}

func unstructuredToString(u *unstructured.Unstructured) string {
	// return no key, but an description including the version
	apiVersion, kind := u.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	return objectKey(apiVersion, kind, u.GetNamespace(), u.GetName())
}

// injectLabels injects the given labels into the given object's metadata and if present also into the
// pod template's and volume claims templates' metadata
func injectLabels(obj *unstructured.Unstructured, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	obj.SetLabels(mergeMaps(obj.GetLabels(), labels))

	if err := injectLabelsIntoPodTemplate(obj, labels); err != nil {
		return err
	}

	return injectLabelsIntoVolumeClaimTemplate(obj, labels)
}

func injectLabelsIntoPodTemplate(obj *unstructured.Unstructured, labels map[string]string) error {
	_, found, err := unstructured.NestedMap(obj.Object, "spec", "template")
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	templateLabels, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return err
	}

	return unstructured.SetNestedField(obj.Object, mergeLabels(templateLabels, labels), "spec", "template", "metadata", "labels")
}

func injectLabelsIntoVolumeClaimTemplate(obj *unstructured.Unstructured, labels map[string]string) error {
	volumeClaimTemplates, templatesFound, err := unstructured.NestedSlice(obj.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return err
	}
	if !templatesFound {
		return nil
	}

	for i, t := range volumeClaimTemplates {
		template, ok := t.(map[string]interface{})
		if !ok {
			return fmt.Errorf("failed to inject labels into .spec.volumeClaimTemplates[%d], is not a map[string]interface{}", i)
		}

		templateLabels, _, err := unstructured.NestedStringMap(template, "metadata", "labels")
		if err != nil {
			return err
		}

		if err := unstructured.SetNestedField(template, mergeLabels(templateLabels, labels), "metadata", "labels"); err != nil {
			return err
		}
	}

	return unstructured.SetNestedSlice(obj.Object, volumeClaimTemplates, "spec", "volumeClaimTemplates")
}

func mergeLabels(existingLabels, newLabels map[string]string) map[string]interface{} {
	if existingLabels == nil {
		return stringMapToInterfaceMap(newLabels)
	}

	labels := make(map[string]interface{}, len(existingLabels)+len(newLabels))
	for k, v := range existingLabels {
		labels[k] = v
	}
	for k, v := range newLabels {
		labels[k] = v
	}
	return labels
}

func stringMapToInterfaceMap(in map[string]string) map[string]interface{} {
	m := make(map[string]interface{}, len(in))
	for k, v := range in {
		m[k] = v
	}
	return m
}

// mergeMaps merges the two string maps. If a key is present in both maps, the value in the second map takes precedence
func mergeMaps(one, two map[string]string) map[string]string {
	out := make(map[string]string, len(one)+len(two))
	for k, v := range one {
		out[k] = v
	}
	for k, v := range two {
		out[k] = v
	}
	return out
}

type object struct {
	obj                       *unstructured.Unstructured
	oldInformation            resourcesv1alpha1.ObjectReference
	forceOverwriteLabels      bool
	forceOverwriteAnnotations bool
}

type decodingError struct {
	err         error
	secret      client.ObjectKey
	secretKey   string
	indexInFile int
}

func (d *decodingError) StringShort() string {
	return fmt.Sprintf("Could not decode resource at index %d in '%s' in secret '%s'", d.indexInFile, d.secretKey, d.secret.String())
}

func (d *decodingError) String() string {
	return fmt.Sprintf("%s: %s.", d.StringShort(), d.err)
}
