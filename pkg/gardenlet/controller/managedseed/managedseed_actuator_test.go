// Copyright (c) 2021 SAP SE or an SAP affiliate company.All rights reserved.This file is licensed under the Apache Software License, v.2 except as noted otherwise in the LICENSE file
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

package managedseed

import (
	"context"
	"path/filepath"
	"time"

	"github.com/gardener/gardener/charts"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	mockclientmap "github.com/gardener/gardener/pkg/client/kubernetes/clientmap/mock"
	mockkubernetes "github.com/gardener/gardener/pkg/client/kubernetes/mock"
	configv1alpha1 "github.com/gardener/gardener/pkg/gardenlet/apis/config/v1alpha1"
	mockmanagedseed "github.com/gardener/gardener/pkg/gardenlet/controller/managedseed/mock"
	mockrecord "github.com/gardener/gardener/pkg/mock/client-go/tools/record"
	mockclient "github.com/gardener/gardener/pkg/mock/controller-runtime/client"
	"github.com/gardener/gardener/pkg/utils"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/config/v1alpha1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	seedName             = "test-seed"
	secretBindingName    = "test-secret-binding"
	secretName           = "test-secret"
	kubeconfigSecretName = "test.kubeconfig"
	backupSecretName     = "test-backup-secret"
	seedSecretName       = "test-seed-secret"
)

var _ = Describe("Actuator", func() {
	var (
		ctrl *gomock.Controller

		gardenClient      *mockclient.MockClient
		gardenAPIReader   *mockclient.MockReader
		seedClient        *mockclient.MockClient
		shootClientSet    *mockkubernetes.MockInterface
		shootClientMap    *mockclientmap.MockClientMap
		vh                *mockmanagedseed.MockValuesHelper
		shootClient       *mockclient.MockClient
		shootChartApplier *mockkubernetes.MockChartApplier
		recorder          *mockrecord.MockEventRecorder

		log      logr.Logger
		actuator Actuator

		ctx context.Context

		managedSeed      *seedmanagementv1alpha1.ManagedSeed
		shoot            *gardencorev1beta1.Shoot
		secretBinding    *gardencorev1beta1.SecretBinding
		secret           *corev1.Secret
		kubeconfigSecret *corev1.Secret

		seedTemplate *gardencorev1beta1.SeedTemplate
		gardenlet    *seedmanagementv1alpha1.Gardenlet

		gardenNamespace     *corev1.Namespace
		backupSecret        *corev1.Secret
		seedSecret          *corev1.Secret
		seed                *gardencorev1beta1.Seed
		gardenletDeployment *appsv1.Deployment

		mergedDeployment      *seedmanagementv1alpha1.GardenletDeployment
		mergedGardenletConfig *configv1alpha1.GardenletConfiguration
		gardenletChartValues  map[string]interface{}
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())

		gardenClient = mockclient.NewMockClient(ctrl)
		gardenAPIReader = mockclient.NewMockReader(ctrl)
		seedClient = mockclient.NewMockClient(ctrl)
		shootClient = mockclient.NewMockClient(ctrl)
		shootClientSet = mockkubernetes.NewMockInterface(ctrl)
		shootClientMap = mockclientmap.NewMockClientMap(ctrl)
		vh = mockmanagedseed.NewMockValuesHelper(ctrl)
		shootChartApplier = mockkubernetes.NewMockChartApplier(ctrl)
		recorder = mockrecord.NewMockEventRecorder(ctrl)

		shootClientSet.EXPECT().Client().Return(shootClient).AnyTimes()
		shootClientSet.EXPECT().ChartApplier().Return(shootChartApplier).AnyTimes()

		log = logr.Discard()
		actuator = newActuator(&rest.Config{}, gardenAPIReader, gardenClient, seedClient, shootClientMap, vh, recorder)

		ctx = context.TODO()

		managedSeed = &seedmanagementv1alpha1.ManagedSeed{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: seedmanagementv1alpha1.ManagedSeedSpec{
				Shoot: &seedmanagementv1alpha1.Shoot{
					Name: name,
				},
			},
		}
		shoot = &gardencorev1beta1.Shoot{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  namespace,
				Generation: 1,
			},
			Spec: gardencorev1beta1.ShootSpec{
				SecretBindingName: secretBindingName,
				SeedName:          pointer.String(seedName),
			},
			Status: gardencorev1beta1.ShootStatus{
				LastOperation: &gardencorev1beta1.LastOperation{
					State: gardencorev1beta1.LastOperationStateSucceeded,
				},
				ObservedGeneration: 1,
				TechnicalID:        "shoot--" + namespace + "--" + name,
			},
		}
		secretBinding = &gardencorev1beta1.SecretBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretBindingName,
				Namespace: namespace,
			},
			SecretRef: corev1.SecretReference{
				Name:      secretName,
				Namespace: namespace,
			},
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"foo": []byte("bar"),
			},
		}
		kubeconfigSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      kubeconfigSecretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"kubeconfig": []byte("kubeconfig"),
			},
		}

		seedTemplate = &gardencorev1beta1.SeedTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"foo": "bar",
				},
				Annotations: map[string]string{
					"bar": "baz",
				},
			},
			Spec: gardencorev1beta1.SeedSpec{
				Backup: &gardencorev1beta1.SeedBackup{
					SecretRef: corev1.SecretReference{
						Name:      backupSecretName,
						Namespace: namespace,
					},
				},
				SecretRef: &corev1.SecretReference{
					Name:      seedSecretName,
					Namespace: namespace,
				},
				Settings: &gardencorev1beta1.SeedSettings{
					VerticalPodAutoscaler: &gardencorev1beta1.SeedSettingVerticalPodAutoscaler{
						Enabled: true,
					},
				},
				Ingress: &gardencorev1beta1.Ingress{},
			},
		}
		gardenlet = &seedmanagementv1alpha1.Gardenlet{
			Deployment: &seedmanagementv1alpha1.GardenletDeployment{
				ReplicaCount:         pointer.Int32(1),
				RevisionHistoryLimit: pointer.Int32(1),
				Image: &seedmanagementv1alpha1.Image{
					PullPolicy: pullPolicyPtr(corev1.PullIfNotPresent),
				},
				VPA: pointer.Bool(true),
			},
			Config: runtime.RawExtension{
				Object: &configv1alpha1.GardenletConfiguration{
					TypeMeta: metav1.TypeMeta{
						APIVersion: configv1alpha1.SchemeGroupVersion.String(),
						Kind:       "GardenletConfiguration",
					},
					SeedConfig: &configv1alpha1.SeedConfig{
						SeedTemplate: *seedTemplate,
					},
				},
			},
			Bootstrap:       bootstrapPtr(seedmanagementv1alpha1.BootstrapToken),
			MergeWithParent: pointer.Bool(true),
		}

		gardenNamespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1beta1constants.GardenNamespace,
			},
		}
		backupSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      backupSecretName,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(managedSeed, seedmanagementv1alpha1.SchemeGroupVersion.WithKind("ManagedSeed")),
				},
			},
			Data: secret.Data,
			Type: corev1.SecretTypeOpaque,
		}
		seedSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      seedSecretName,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(managedSeed, seedmanagementv1alpha1.SchemeGroupVersion.WithKind("ManagedSeed")),
				},
			},
			Data: map[string][]byte{
				"kubeconfig": []byte("kubeconfig"),
			},
			Type: corev1.SecretTypeOpaque,
		}
		seed = &gardencorev1beta1.Seed{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: utils.MergeStringMaps(seedTemplate.Labels, map[string]string{
					v1beta1constants.GardenRole: v1beta1constants.GardenRoleSeed,
				}),
				Annotations: seedTemplate.Annotations,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(managedSeed, seedmanagementv1alpha1.SchemeGroupVersion.WithKind("ManagedSeed")),
				},
			},
			Spec: seedTemplate.Spec,
		}
		gardenletDeployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      v1beta1constants.DeploymentNameGardenlet,
				Namespace: v1beta1constants.GardenNamespace,
			},
		}
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	var (
		expectGetShoot = func() {
			gardenAPIReader.EXPECT().Get(ctx, kutil.Key(namespace, name), gomock.AssignableToTypeOf(&gardencorev1beta1.Shoot{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *gardencorev1beta1.Shoot, _ ...client.GetOption) error {
					*s = *shoot
					return nil
				},
			)
		}

		expectCreateGardenNamespace = func() {
			shootClient.EXPECT().Get(ctx, kutil.Key(v1beta1constants.GardenNamespace), gomock.AssignableToTypeOf(&corev1.Namespace{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, _ *corev1.Namespace, _ ...client.GetOption) error {
					return apierrors.NewNotFound(corev1.Resource("namespace"), v1beta1constants.GardenNamespace)
				},
			)
			shootClient.EXPECT().Create(ctx, gomock.AssignableToTypeOf(&corev1.Namespace{})).DoAndReturn(
				func(_ context.Context, ns *corev1.Namespace, _ ...client.CreateOption) error {
					Expect(ns.Name).To(Equal(v1beta1constants.GardenNamespace))
					return nil
				},
			)
		}

		expectDeleteGardenNamespace = func() {
			shootClient.EXPECT().Delete(ctx, gomock.AssignableToTypeOf(&corev1.Namespace{})).DoAndReturn(
				func(_ context.Context, ns *corev1.Namespace, _ ...client.DeleteOption) error {
					Expect(ns.Name).To(Equal(v1beta1constants.GardenNamespace))
					return nil
				},
			)
		}

		expectGetGardenNamespace = func(exists bool) {
			shootClient.EXPECT().Get(ctx, kutil.Key(v1beta1constants.GardenNamespace), gomock.AssignableToTypeOf(&corev1.Namespace{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, ns *corev1.Namespace, _ ...client.GetOption) error {
					if exists {
						*ns = *gardenNamespace
						return nil
					}
					return apierrors.NewNotFound(corev1.Resource("namespace"), v1beta1constants.GardenNamespace)
				},
			)
		}

		expectCheckSeedSpec = func() {
			// Check if the shoot namespace in the seed contains a vpa-admission-controller deployment
			seedClient.EXPECT().Get(ctx, kutil.Key(shoot.Status.TechnicalID, "vpa-admission-controller"), gomock.AssignableToTypeOf(&appsv1.Deployment{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, _ *appsv1.Deployment, _ ...client.GetOption) error {
					return apierrors.NewNotFound(appsv1.Resource("deployment"), "vpa-admission-controller")
				},
			)
		}

		expectCreateSeedSecrets = func() {
			// Get shoot secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, secretBindingName), gomock.AssignableToTypeOf(&gardencorev1beta1.SecretBinding{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, sb *gardencorev1beta1.SecretBinding, _ ...client.GetOption) error {
					*sb = *secretBinding
					return nil
				},
			)
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, secretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *corev1.Secret, _ ...client.GetOption) error {
					*s = *secret
					return nil
				},
			)

			// Create backup secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, backupSecretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, _ *corev1.Secret, _ ...client.GetOption) error {
					return apierrors.NewNotFound(corev1.Resource("secret"), backupSecretName)
				},
			)
			gardenClient.EXPECT().Create(ctx, gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, s *corev1.Secret, _ ...client.CreateOption) error {
					Expect(s).To(Equal(backupSecret))
					return nil
				},
			)

			// Create seed secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, kubeconfigSecretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *corev1.Secret, _ ...client.GetOption) error {
					*s = *kubeconfigSecret
					return nil
				},
			)
			gardenClient.EXPECT().Create(ctx, gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, s *corev1.Secret, _ ...client.CreateOption) error {
					Expect(s).To(Equal(seedSecret))
					return nil
				},
			)
		}

		expectDeleteSeedSecrets = func() {
			// Delete backup secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, backupSecretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *corev1.Secret, _ ...client.GetOption) error {
					*s = *backupSecret
					return nil
				},
			)
			gardenClient.EXPECT().Delete(ctx, gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, s *corev1.Secret, _ ...client.DeleteOption) error {
					Expect(s.Name).To(Equal(backupSecretName))
					Expect(s.Namespace).To(Equal(namespace))
					return nil
				},
			)

			// Delete seed secret
			gardenClient.EXPECT().Delete(ctx, gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, s *corev1.Secret, _ ...client.DeleteOption) error {
					Expect(s.Name).To(Equal(seedSecretName))
					Expect(s.Namespace).To(Equal(namespace))
					return nil
				},
			)
		}

		expectGetSeedSecrets = func(exist bool) {
			// Get backup secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, backupSecretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *corev1.Secret, _ ...client.GetOption) error {
					if exist {
						*s = *backupSecret
						return nil
					}
					return apierrors.NewNotFound(corev1.Resource("secret"), backupSecretName)
				},
			)

			// Get seed secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(namespace, seedSecretName), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *corev1.Secret, _ ...client.GetOption) error {
					if exist {
						*s = *seedSecret
						return nil
					}
					return apierrors.NewNotFound(corev1.Resource("secret"), seedSecretName)
				},
			)
		}

		expectCreateSeed = func() {
			gardenClient.EXPECT().Create(ctx, gomock.AssignableToTypeOf(&gardencorev1beta1.Seed{})).DoAndReturn(
				func(_ context.Context, s *gardencorev1beta1.Seed, _ ...client.CreateOption) error {
					Expect(s).To(Equal(seed))
					return nil
				},
			)
		}

		expectDeleteSeed = func() {
			gardenClient.EXPECT().Delete(ctx, gomock.AssignableToTypeOf(&gardencorev1beta1.Seed{})).DoAndReturn(
				func(_ context.Context, s *gardencorev1beta1.Seed, _ ...client.DeleteOption) error {
					Expect(s.Name).To(Equal(name))
					return nil
				},
			)
		}

		expectGetSeed = func(exists bool) {
			gardenClient.EXPECT().Get(ctx, kutil.Key(name), gomock.AssignableToTypeOf(&gardencorev1beta1.Seed{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, s *gardencorev1beta1.Seed, _ ...client.GetOption) error {
					if exists {
						*s = *seed
						return nil
					}
					return apierrors.NewNotFound(gardencorev1beta1.Resource("seed"), name)
				},
			)
		}

		expectMergeWithParent = func() {
			mergedDeployment = managedSeed.Spec.Gardenlet.Deployment.DeepCopy()
			mergedDeployment.Image = &seedmanagementv1alpha1.Image{
				Repository: pointer.String("repository"),
				Tag:        pointer.String("tag"),
				PullPolicy: pullPolicyPtr(corev1.PullIfNotPresent),
			}

			mergedGardenletConfig = managedSeed.Spec.Gardenlet.Config.Object.(*configv1alpha1.GardenletConfiguration).DeepCopy()
			mergedGardenletConfig.GardenClientConnection = &configv1alpha1.GardenClientConnection{
				ClientConnectionConfiguration: v1alpha1.ClientConnectionConfiguration{
					Kubeconfig: "kubeconfig",
				},
			}

			vh.EXPECT().MergeGardenletDeployment(managedSeed.Spec.Gardenlet.Deployment, shoot).Return(mergedDeployment, nil)
			vh.EXPECT().MergeGardenletConfiguration(managedSeed.Spec.Gardenlet.Config.Object).Return(mergedGardenletConfig, nil)
		}

		expectDeleteKubeconfigSecret = func() {
			shootClient.EXPECT().Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1beta1constants.GardenNamespace, Name: "gardenlet-kubeconfig"}})
		}

		expectPrepareGardenClientConnection = func(withAlreadyBootstrappedCheck bool) {
			if withAlreadyBootstrappedCheck {
				// Check if kubeconfig secret exists
				shootClient.EXPECT().Get(ctx, kutil.Key(v1beta1constants.GardenNamespace, "gardenlet-kubeconfig"), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
					func(_ context.Context, _ client.ObjectKey, _ *corev1.Secret, _ ...client.GetOption) error {
						return apierrors.NewNotFound(corev1.Resource("secret"), "gardenlet-kubeconfig")
					},
				)
			}

			// Create bootstrap token secret
			gardenClient.EXPECT().Get(ctx, kutil.Key(metav1.NamespaceSystem, "bootstrap-token-a82f8a"), gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, _ *corev1.Secret, _ ...client.GetOption) error {
					return apierrors.NewNotFound(corev1.Resource("secret"), "bootstrap-token-a82f8a")
				},
			).Times(3)
			gardenClient.EXPECT().Create(ctx, gomock.AssignableToTypeOf(&corev1.Secret{})).DoAndReturn(
				func(_ context.Context, s *corev1.Secret, _ ...client.CreateOption) error {
					Expect(s.Name).To(Equal("bootstrap-token-a82f8a"))
					Expect(s.Namespace).To(Equal(metav1.NamespaceSystem))
					Expect(s.Type).To(Equal(corev1.SecretTypeBootstrapToken))
					Expect(s.Data).To(HaveKeyWithValue("token-id", []byte("a82f8a")))
					Expect(s.Data).To(HaveKey("token-secret"))
					Expect(s.Data).To(HaveKeyWithValue("usage-bootstrap-signing", []byte("true")))
					Expect(s.Data).To(HaveKeyWithValue("usage-bootstrap-authentication", []byte("true")))
					return nil
				},
			)
		}

		expectGetGardenletChartValues = func(withBootstrap bool) {
			gardenletChartValues = map[string]interface{}{"foo": "bar"}

			vh.EXPECT().GetGardenletChartValues(mergedDeployment, gomock.AssignableToTypeOf(&configv1alpha1.GardenletConfiguration{}), gomock.AssignableToTypeOf("")).DoAndReturn(
				func(_ *seedmanagementv1alpha1.GardenletDeployment, gc *configv1alpha1.GardenletConfiguration, _ string) (map[string]interface{}, error) {
					if withBootstrap {
						Expect(gc.GardenClientConnection.Kubeconfig).To(Equal(""))
						Expect(gc.GardenClientConnection.KubeconfigSecret).To(Equal(&corev1.SecretReference{
							Name:      "gardenlet-kubeconfig",
							Namespace: v1beta1constants.GardenNamespace,
						}))
						Expect(gc.GardenClientConnection.BootstrapKubeconfig).To(Equal(&corev1.SecretReference{
							Name:      "gardenlet-kubeconfig-bootstrap",
							Namespace: v1beta1constants.GardenNamespace,
						}))
					} else {
						Expect(gc.GardenClientConnection.Kubeconfig).To(Equal("kubeconfig"))
						Expect(gc.GardenClientConnection.KubeconfigSecret).To(BeNil())
						Expect(gc.GardenClientConnection.BootstrapKubeconfig).To(BeNil())
					}
					Expect(gc.SeedConfig.SeedTemplate).To(Equal(gardencorev1beta1.SeedTemplate{
						ObjectMeta: metav1.ObjectMeta{
							Name:        name,
							Labels:      seedTemplate.Labels,
							Annotations: seedTemplate.Annotations,
						},
						Spec: seedTemplate.Spec,
					}))

					return gardenletChartValues, nil
				},
			)
		}

		expectApplyGardenletChart = func() {
			shootChartApplier.EXPECT().Apply(ctx, filepath.Join(charts.Path, "gardener", "gardenlet"), v1beta1constants.GardenNamespace, "gardenlet", kubernetes.Values(gardenletChartValues)).Return(nil)
		}

		expectDeleteGardenletChart = func() {
			shootChartApplier.EXPECT().Delete(ctx, filepath.Join(charts.Path, "gardener", "gardenlet"), v1beta1constants.GardenNamespace, "gardenlet", kubernetes.Values(gardenletChartValues)).Return(nil)
		}

		expectGetGardenletDeployment = func(exists bool) {
			shootClient.EXPECT().Get(ctx, kutil.Key(v1beta1constants.GardenNamespace, v1beta1constants.DeploymentNameGardenlet), gomock.AssignableToTypeOf(&appsv1.Deployment{})).DoAndReturn(
				func(_ context.Context, _ client.ObjectKey, d *appsv1.Deployment, _ ...client.GetOption) error {
					if exists {
						*d = *gardenletDeployment
						return nil
					}
					return apierrors.NewNotFound(appsv1.Resource("deployment"), v1beta1constants.DeploymentNameGardenlet)
				},
			)
		}
	)

	Describe("#Reconcile", func() {
		BeforeEach(func() {
			shootClientMap.EXPECT().GetClient(ctx, keys.ForShoot(shoot)).Return(shootClientSet, nil).AnyTimes()
			gardenClient.EXPECT().Scheme().Return(kubernetes.GardenScheme).AnyTimes()
		})

		It("should wait if the Shoot is still reconciling", func() {
			shoot.ObjectMeta.Generation = 2
			expectGetShoot()
			recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Waiting for shoot \""+client.ObjectKeyFromObject(shoot).String()+"\" to be reconciled")

			status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
			Expect(err).ToNot(HaveOccurred())
			Expect(status.Conditions).To(ConsistOf(
				MatchFields(IgnoreExtras, Fields{
					"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
					"Status": Equal(gardencorev1beta1.ConditionFalse),
					"Reason": Equal(gardencorev1beta1.EventReconciling),
				}),
			))
			Expect(wait).To(Equal(true))
		})

		Context("seed template", func() {
			BeforeEach(func() {
				managedSeed.Spec.SeedTemplate = seedTemplate
			})

			It("should create the garden namespace, seed secrets, and seed", func() {
				expectGetShoot()
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed object %q", name)
				expectCreateSeed()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})
		})

		Context("gardenlet", func() {
			BeforeEach(func() {
				managedSeed.Spec.Gardenlet = gardenlet
			})

			It("should create the garden namespace and seed secrets, and deploy gardenlet (with bootstrap)", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Deploying gardenlet into shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectPrepareGardenClientConnection(true)
				expectGetGardenletChartValues(true)
				expectApplyGardenletChart()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})

			It("should create the garden namespace and seed secrets, and deploy gardenlet (with bootstrap and non-expired gardenlet client cert)", func() {
				seed.Status.ClientCertificateExpirationTimestamp = &metav1.Time{Time: time.Now().Add(time.Hour)}

				expectGetShoot()
				expectGetSeed(true)
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Deploying gardenlet into shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectPrepareGardenClientConnection(true)
				expectGetGardenletChartValues(true)
				expectApplyGardenletChart()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})

			It("should create the garden namespace and seed secrets, and deploy gardenlet (with bootstrap and expired gardenlet client cert)", func() {
				seed.Status.ClientCertificateExpirationTimestamp = &metav1.Time{Time: time.Now().Add(-time.Hour)}

				expectGetShoot()
				expectDeleteKubeconfigSecret()
				expectGetSeed(true)
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Deploying gardenlet into shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectPrepareGardenClientConnection(false)
				expectGetGardenletChartValues(true)
				expectApplyGardenletChart()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})

			It("should create the garden namespace and seed secrets, and deploy gardenlet (with bootstrap, non-expired gardenlet client cert, and renew-kubeconfig annotation)", func() {
				seed.Status.ClientCertificateExpirationTimestamp = &metav1.Time{Time: time.Now().Add(time.Hour)}
				managedSeed.Annotations = map[string]string{
					v1beta1constants.GardenerOperation: v1beta1constants.GardenerOperationRenewKubeconfig,
				}

				expectGetShoot()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Renewing gardenlet kubeconfig secret due to operation annotation")
				expectDeleteKubeconfigSecret()
				gardenClient.EXPECT().Patch(ctx, gomock.AssignableToTypeOf(&seedmanagementv1alpha1.ManagedSeed{}), gomock.Any()).DoAndReturn(func(_ context.Context, o client.Object, patch client.Patch, opts ...client.PatchOption) error {
					Expect(patch.Data(o)).To(BeEquivalentTo(`{"metadata":{"annotations":null}}`))
					return nil
				})
				expectGetSeed(true)
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Deploying gardenlet into shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectPrepareGardenClientConnection(false)
				expectGetGardenletChartValues(true)
				expectApplyGardenletChart()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})

			It("should create the garden namespace and seed secrets, and deploy gardenlet (without bootstrap)", func() {
				managedSeed.Spec.Gardenlet.Bootstrap = bootstrapPtr(seedmanagementv1alpha1.BootstrapNone)

				expectGetShoot()
				expectGetSeed(false)
				expectCheckSeedSpec()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Ensuring garden namespace in shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectCreateGardenNamespace()
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Reconciling seed secrets")
				expectCreateSeedSecrets()
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventReconciling, "Deploying gardenlet into shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectGetGardenletChartValues(false)
				expectApplyGardenletChart()

				status, wait, err := actuator.Reconcile(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedShootReconciled),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionTrue),
						"Reason": Equal(gardencorev1beta1.EventReconciled),
					}),
				))
				Expect(wait).To(Equal(false))
			})
		})
	})

	Describe("#Delete", func() {
		BeforeEach(func() {
			shootClientMap.EXPECT().GetClient(ctx, keys.ForShoot(shoot)).Return(shootClientSet, nil)
		})

		Context("seed template", func() {
			BeforeEach(func() {
				managedSeed.Spec.SeedTemplate = seedTemplate
			})

			It("should delete the seed if it still exists", func() {
				expectGetShoot()
				expectGetSeed(true)
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting seed %s", name)
				expectDeleteSeed()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(false))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should delete the seed secrets if they still exist", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetSeedSecrets(true)
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting seed secrets")
				expectDeleteSeedSecrets()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(true))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should delete the garden namespace if it still exists, and set wait to true", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetSeedSecrets(false)
				expectGetGardenNamespace(true)
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting garden namespace from shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectDeleteGardenNamespace()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(true))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should do nothing if neither the seed, nor the seed secrets, nor the garden namespace exist, and set removeFinalizer to true", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetSeedSecrets(false)
				expectGetGardenNamespace(false)

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleted),
					}),
				))
				Expect(wait).To(Equal(false))
				Expect(removeFinalizer).To(Equal(true))
			})
		})

		Context("gardenlet", func() {
			BeforeEach(func() {
				managedSeed.Spec.Gardenlet = gardenlet
			})

			It("should delete the seed if it still exists", func() {
				expectGetShoot()
				expectGetSeed(true)
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting seed %s", name)
				expectDeleteSeed()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(false))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should delete gardenlet if it still exists", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetGardenletDeployment(true)
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting gardenlet from shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectMergeWithParent()
				expectPrepareGardenClientConnection(true)
				expectGetGardenletChartValues(true)
				expectDeleteGardenletChart()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(true))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should delete the seed secrets if they still exist", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetGardenletDeployment(false)
				expectGetSeedSecrets(true)
				recorder.EXPECT().Event(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting seed secrets")
				expectDeleteSeedSecrets()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(true))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should delete the garden namespace if it still exists, and set wait to true", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetGardenletDeployment(false)
				expectGetSeedSecrets(false)
				expectGetGardenNamespace(true)
				recorder.EXPECT().Eventf(managedSeed, corev1.EventTypeNormal, gardencorev1beta1.EventDeleting, "Deleting garden namespace from shoot %q", client.ObjectKeyFromObject(shoot).String())
				expectDeleteGardenNamespace()

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleting),
					}),
				))
				Expect(wait).To(Equal(true))
				Expect(removeFinalizer).To(Equal(false))
			})

			It("should do nothing if neither the seed, nor gardenlet, nor the seed secrets, nor the garden namespace exist, and set removeFinalizer to true", func() {
				expectGetShoot()
				expectGetSeed(false)
				expectGetGardenletDeployment(false)
				expectGetSeedSecrets(false)
				expectGetGardenNamespace(false)

				status, wait, removeFinalizer, err := actuator.Delete(ctx, log, managedSeed)
				Expect(err).ToNot(HaveOccurred())
				Expect(status.Conditions).To(ConsistOf(
					MatchFields(IgnoreExtras, Fields{
						"Type":   Equal(seedmanagementv1alpha1.ManagedSeedSeedRegistered),
						"Status": Equal(gardencorev1beta1.ConditionFalse),
						"Reason": Equal(gardencorev1beta1.EventDeleted),
					}),
				))
				Expect(wait).To(Equal(false))
				Expect(removeFinalizer).To(Equal(true))
			})
		})
	})
})

var _ = Describe("Utils", func() {
	Describe("#ensureGardenletEnvironment", func() {
		const (
			kubernetesServiceHost = "KUBERNETES_SERVICE_HOST"
			preserveDomain        = "preserve-value.example.com"
		)
		var (
			otherEnvDeployment = &seedmanagementv1alpha1.GardenletDeployment{
				Env: []corev1.EnvVar{
					{Name: "TEST_VAR", Value: "TEST_VALUE"},
				},
			}
			kubernetesServiceHostEnvDeployment = &seedmanagementv1alpha1.GardenletDeployment{
				Env: []corev1.EnvVar{
					{Name: kubernetesServiceHost, Value: preserveDomain},
				},
			}

			dnsWithDomain = &gardencorev1beta1.DNS{
				Domain: pointer.String("my-shoot.example.com"),
			}
			dnsWithoutDomain = &gardencorev1beta1.DNS{
				Domain: nil,
			}
		)

		It("should not overwrite existing KUBERNETES_SERVICE_HOST environment", func() {
			ensuredDeploymentWithDomain := ensureGardenletEnvironment(kubernetesServiceHostEnvDeployment, dnsWithDomain)
			ensuredDeploymentWithoutDomain := ensureGardenletEnvironment(kubernetesServiceHostEnvDeployment, dnsWithoutDomain)

			Expect(ensuredDeploymentWithDomain.Env[0].Name).To(Equal(kubernetesServiceHost))
			Expect(ensuredDeploymentWithDomain.Env[0].Value).To(Equal(preserveDomain))
			Expect(ensuredDeploymentWithDomain.Env[0].Value).ToNot(Equal(gutil.GetAPIServerDomain(*dnsWithDomain.Domain)))

			Expect(ensuredDeploymentWithoutDomain.Env[0].Name).To(Equal(kubernetesServiceHost))
			Expect(ensuredDeploymentWithoutDomain.Env[0].Value).To(Equal(preserveDomain))

		})

		It("should should not inject KUBERNETES_SERVICE_HOST environemnt", func() {
			ensuredDeploymentWithoutDomain := ensureGardenletEnvironment(otherEnvDeployment, dnsWithoutDomain)

			Expect(ensuredDeploymentWithoutDomain.Env).To(HaveLen(1))
			Expect(ensuredDeploymentWithoutDomain.Env[0].Name).ToNot(Equal(kubernetesServiceHost))
		})
		It("should should inject KUBERNETES_SERVICE_HOST environemnt", func() {
			ensuredDeploymentWithoutDomain := ensureGardenletEnvironment(otherEnvDeployment, dnsWithDomain)

			Expect(ensuredDeploymentWithoutDomain.Env).To(HaveLen(2))
			Expect(ensuredDeploymentWithoutDomain.Env[0].Name).ToNot(Equal(kubernetesServiceHost))
			Expect(ensuredDeploymentWithoutDomain.Env[1].Name).To(Equal(kubernetesServiceHost))
			Expect(ensuredDeploymentWithoutDomain.Env[1].Value).To(Equal(gutil.GetAPIServerDomain(*dnsWithDomain.Domain)))

		})
	})
})

func pullPolicyPtr(v corev1.PullPolicy) *corev1.PullPolicy { return &v }

func bootstrapPtr(v seedmanagementv1alpha1.Bootstrap) *seedmanagementv1alpha1.Bootstrap { return &v }
