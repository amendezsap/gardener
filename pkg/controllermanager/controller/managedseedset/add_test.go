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

package managedseedset

import (
	"context"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
)

const (
	name      = "test"
	namespace = "garden"
)

var _ = Describe("Add", func() {
	var (
		ctx        = context.TODO()
		log        logr.Logger
		fakeClient client.Client
		reconciler *Reconciler
		pred       predicate.Predicate

		now = metav1.Now()

		seed           *gardencorev1beta1.Seed
		managedSeed    *seedmanagementv1alpha1.ManagedSeed
		managedSeedSet *seedmanagementv1alpha1.ManagedSeedSet
	)

	BeforeEach(func() {
		reconciler = &Reconciler{}
		fakeClient = fakeclient.NewClientBuilder().WithScheme(kubernetes.GardenScheme).Build()

		managedSeedSet = &seedmanagementv1alpha1.ManagedSeedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: seedmanagementv1alpha1.ManagedSeedSetSpec{
				Selector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": name,
					},
				},
			},
		}
	})

	Describe("#ShootPredicate", func() {
		var (
			oldShoot, newShoot *gardencorev1beta1.Shoot
		)

		BeforeEach(func() {
			pred = reconciler.ShootPredicate()

			oldShoot = &gardencorev1beta1.Shoot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
			}

			newShoot = &gardencorev1beta1.Shoot{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
			}

			Expect(inject.StopChannelInto(ctx.Done(), pred)).To(BeTrue())
			Expect(inject.ClientInto(fakeClient, pred)).To(BeTrue())
		})

		It("should return false for create, update and delete event when Shoot does not references any ManagedSeedSet", func() {
			newShoot.OwnerReferences = nil
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Shoot is not present", func() {
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for update event and false for create and delete event when deletion timestamp is set", func() {
			newShoot.DeletionTimestamp = &now
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Shoot does not have any pending replica", func() {
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Shoot has other Shoot in pending replica", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for update and false for create and delete event when Shoot health status changes", func() {
			newShoot.Labels = map[string]string{
				v1beta1constants.ShootStatus: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootReconciling status and Shoot reconciliation failed", func() {
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeCreate,
				State: gardencorev1beta1.LastOperationStateFailed,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootReconcilingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootReconciling status and Shoot reconciliation succeeded", func() {
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeCreate,
				State: gardencorev1beta1.LastOperationStateSucceeded,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootReconcilingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootReconciling status and Shoot deletion timestamp is set", func() {
			newShoot.DeletionTimestamp = &now
			oldShoot.DeletionTimestamp = &now
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootReconcilingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootDeleting status and Shoot last operation is not delete", func() {
			newShoot.DeletionTimestamp = &now
			oldShoot.DeletionTimestamp = &now
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeReconcile,
				State: gardencorev1beta1.LastOperationStateFailed,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootDeletingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootDeleting status and Shoot deletion failed", func() {
			newShoot.DeletionTimestamp = &now
			oldShoot.DeletionTimestamp = &now
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeDelete,
				State: gardencorev1beta1.LastOperationStateFailed,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootDeletingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when pending replica has ShootReconcileFailed status and Shoot reconciliation failed", func() {
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeReconcile,
				State: gardencorev1beta1.LastOperationStateFailed,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootReconcileFailedReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when pending replica has ShootDeleteFailed status and Shoot deletion failed", func() {
			newShoot.DeletionTimestamp = &now
			oldShoot.DeletionTimestamp = &now
			newShoot.Status.LastOperation = &gardencorev1beta1.LastOperation{
				Type:  gardencorev1beta1.LastOperationTypeDelete,
				State: gardencorev1beta1.LastOperationStateFailed,
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootDeleteFailedReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ShootNotHealthy status and Shoot got healthy", func() {
			newShoot.Labels = map[string]string{
				v1beta1constants.ShootStatus: "healthy",
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: seedmanagementv1alpha1.ShootNotHealthyReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event in default case", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newShoot.Name,
				Reason: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldShoot, ObjectNew: newShoot})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newShoot})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})
	})

	Describe("#ManagedSeedPredicate", func() {
		var (
			oldManagedSeed, newManagedSeed *seedmanagementv1alpha1.ManagedSeed
		)

		BeforeEach(func() {
			pred = reconciler.ManagedSeedPredicate()

			oldManagedSeed = &seedmanagementv1alpha1.ManagedSeed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
			}

			newManagedSeed = &seedmanagementv1alpha1.ManagedSeed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
			}

			Expect(inject.StopChannelInto(ctx.Done(), pred)).To(BeTrue())
			Expect(inject.ClientInto(fakeClient, pred)).To(BeTrue())
		})

		It("should return true for update event and false for create and delete event when deletion timestamp is set", func() {
			newManagedSeed.DeletionTimestamp = &now
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeed does not references any ManagedSeedSet", func() {
			newManagedSeed.OwnerReferences = nil
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by ManagedSeed is not present", func() {
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by ManagedSeed does not have any pending replica", func() {
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by ManagedSeed have other managed seed in pending replica", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ManagedSeedPreparingReason status and ManagedSeed's Seed is registered", func() {
			newManagedSeed.Status.Conditions = []gardencorev1beta1.Condition{
				{Type: seedmanagementv1alpha1.ManagedSeedSeedRegistered, Status: gardencorev1beta1.ConditionTrue},
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newManagedSeed.Name,
				Reason: seedmanagementv1alpha1.ManagedSeedPreparingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has ManagedSeedPreparingReason status and ManagedSeed deletion timestamp is set", func() {
			newManagedSeed.DeletionTimestamp = &now
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newManagedSeed.Name,
				Reason: seedmanagementv1alpha1.ManagedSeedPreparingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for delete and false for update and delete event when pending replica has ManagedSeedDeletingReason status", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newManagedSeed.Name,
				Reason: seedmanagementv1alpha1.ManagedSeedDeletingReason,
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event in default case", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newManagedSeed.Name,
				Reason: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldManagedSeed, ObjectNew: newManagedSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newManagedSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})
	})

	Describe("#SeedPredicate", func() {
		var (
			oldSeed, newSeed *gardencorev1beta1.Seed
			managedSeed      *seedmanagementv1alpha1.ManagedSeed
		)

		BeforeEach(func() {
			pred = reconciler.SeedPredicate()

			managedSeed = &seedmanagementv1alpha1.ManagedSeed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
				Spec: seedmanagementv1alpha1.ManagedSeedSpec{},
			}

			oldSeed = &gardencorev1beta1.Seed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
				},
			}

			newSeed = &gardencorev1beta1.Seed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
				},
			}

			Expect(inject.StopChannelInto(ctx.Done(), pred)).To(BeTrue())
			Expect(inject.ClientInto(fakeClient, pred)).To(BeTrue())
		})

		It("should return true for update and false for create and delete event when Seed Ready status changes", func() {
			newSeed.Status.Conditions = []gardencorev1beta1.Condition{
				{Type: gardencorev1beta1.SeedGardenletReady, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBootstrapped, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedSystemComponentsHealthy, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBackupBucketsReady, Status: gardencorev1beta1.ConditionTrue},
			}
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeed referenced by Seed does not exist", func() {
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeed referenced by Seed does not refer to ManagedSeedSet", func() {
			managedSeed.OwnerReferences = nil
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Seed's ManagedSeed does not exist", func() {
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Seed's ManagedSeed does not have pending replica", func() {
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event when ManagedSeedSet referenced by Seed's ManagedSeed has other Seed in pending replica", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return true for create, update and delete event when pending replica has SeedNotReady status and Seed is ready", func() {
			oldSeed.Status.Conditions = []gardencorev1beta1.Condition{
				{Type: gardencorev1beta1.SeedGardenletReady, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBootstrapped, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedSystemComponentsHealthy, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBackupBucketsReady, Status: gardencorev1beta1.ConditionTrue},
			}
			newSeed.Status.Conditions = []gardencorev1beta1.Condition{
				{Type: gardencorev1beta1.SeedGardenletReady, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBootstrapped, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedSystemComponentsHealthy, Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.SeedBackupBucketsReady, Status: gardencorev1beta1.ConditionTrue},
			}
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newSeed.Name,
				Reason: seedmanagementv1alpha1.SeedNotReadyReason,
			}
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeTrue())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeTrue())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeTrue())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})

		It("should return false for create, update and delete event in default case", func() {
			managedSeedSet.Status.PendingReplica = &seedmanagementv1alpha1.PendingReplica{
				Name:   newSeed.Name,
				Reason: "foo",
			}
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())
			Expect(pred.Create(event.CreateEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Update(event.UpdateEvent{ObjectOld: oldSeed, ObjectNew: newSeed})).To(BeFalse())
			Expect(pred.Delete(event.DeleteEvent{Object: newSeed})).To(BeFalse())
			Expect(pred.Generic(event.GenericEvent{})).To(BeFalse())
		})
	})

	Describe("#MapSeedToManagedSeedSet", func() {
		BeforeEach(func() {
			log = logr.Discard()

			seed = &gardencorev1beta1.Seed{
				ObjectMeta: metav1.ObjectMeta{
					Name: name + "0",
				},
			}

			managedSeed = &seedmanagementv1alpha1.ManagedSeed{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name + "0",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "ManagedSeedSet",
							Name:       name,
							Controller: pointer.Bool(true),
						},
					},
				},
				Spec: seedmanagementv1alpha1.ManagedSeedSpec{},
			}
		})

		It("should do nothing if the object is no Seed", func() {
			Expect(reconciler.MapSeedToManagedSeedSet(ctx, log, fakeClient, &corev1.Secret{})).To(BeEmpty())
		})

		It("should do nothing if there is no related ManagedSeed", func() {
			Expect(reconciler.MapSeedToManagedSeedSet(ctx, log, fakeClient, seed)).To(BeEmpty())
		})

		It("should do nothing if the ManagedSeed does not reference any ManagedSeedSet", func() {
			managedSeed.OwnerReferences = nil
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(reconciler.MapSeedToManagedSeedSet(ctx, log, fakeClient, seed)).To(BeEmpty())
		})

		It("should do nothing if the referenced ManagedSeedSet does not exist", func() {
			managedSeedSet.Name = "foo"
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(reconciler.MapSeedToManagedSeedSet(ctx, log, fakeClient, seed)).To(BeEmpty())
		})

		It("should map the Seed to the ManagedSeedSet", func() {
			Expect(fakeClient.Create(ctx, managedSeed)).To(Succeed())
			Expect(fakeClient.Create(ctx, managedSeedSet)).To(Succeed())

			Expect(reconciler.MapSeedToManagedSeedSet(ctx, log, fakeClient, seed)).To(ConsistOf(
				reconcile.Request{NamespacedName: types.NamespacedName{Name: managedSeedSet.Name, Namespace: managedSeedSet.Namespace}},
			))
		})
	})
})
