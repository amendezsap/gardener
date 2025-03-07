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
	"context"
	"errors"
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/features"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	"github.com/gardener/gardener/pkg/operation/botanist/component/clusterautoscaler"
	"github.com/gardener/gardener/pkg/operation/botanist/component/clusteridentity"
	"github.com/gardener/gardener/pkg/operation/botanist/component/dependencywatchdog"
	"github.com/gardener/gardener/pkg/operation/botanist/component/etcd"
	"github.com/gardener/gardener/pkg/operation/botanist/component/gardenerkubescheduler"
	"github.com/gardener/gardener/pkg/operation/botanist/component/hvpa"
	"github.com/gardener/gardener/pkg/operation/botanist/component/istio"
	"github.com/gardener/gardener/pkg/operation/botanist/component/kubestatemetrics"
	"github.com/gardener/gardener/pkg/operation/botanist/component/networkpolicies"
	"github.com/gardener/gardener/pkg/operation/botanist/component/nginxingress"
	"github.com/gardener/gardener/pkg/operation/botanist/component/resourcemanager"
	"github.com/gardener/gardener/pkg/operation/botanist/component/seedadmissioncontroller"
	"github.com/gardener/gardener/pkg/operation/botanist/component/seedsystem"
	"github.com/gardener/gardener/pkg/operation/botanist/component/vpa"
	"github.com/gardener/gardener/pkg/operation/botanist/component/vpnauthzserver"
	seedpkg "github.com/gardener/gardener/pkg/operation/seed"
	"github.com/gardener/gardener/pkg/utils/flow"
)

func (r *Reconciler) delete(ctx context.Context, log logr.Logger, seedObj *seedpkg.Seed) (reconcile.Result, error) {
	seed := seedObj.GetInfo()

	if !sets.NewString(seed.Finalizers...).Has(gardencorev1beta1.GardenerName) {
		return reconcile.Result{}, nil
	}

	if seed.Spec.Backup != nil {
		backupBucket := &gardencorev1beta1.BackupBucket{ObjectMeta: metav1.ObjectMeta{Name: string(seed.UID)}}
		if err := r.GardenClient.Delete(ctx, backupBucket); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
	}

	// Before deletion, it has to be ensured that no Shoots nor BackupBuckets depend on the Seed anymore.
	// When this happens the controller will remove the finalizers from the Seed so that it can be garbage collected.
	associatedShoots, err := controllerutils.DetermineShootsAssociatedTo(ctx, r.GardenClient, seed)
	if err != nil {
		return reconcile.Result{}, err
	}

	associatedBackupBuckets, err := controllerutils.DetermineBackupBucketAssociations(ctx, r.GardenClient, seed.Name)
	if err != nil {
		return reconcile.Result{}, err
	}

	if len(associatedShoots) > 0 || len(associatedBackupBuckets) > 0 {
		parentLogMessage := "Can't delete Seed, because the following objects are still referencing it:"

		if len(associatedShoots) != 0 {
			log.Info("Cannot delete Seed because the following Shoots are still referencing it", "shoots", associatedShoots)
			r.Recorder.Event(seed, corev1.EventTypeNormal, v1beta1constants.EventResourceReferenced, fmt.Sprintf("%s Shoots=%v", parentLogMessage, associatedShoots))
		}

		if len(associatedBackupBuckets) != 0 {
			log.Info("Cannot delete Seed because the following BackupBuckets are still referencing it", "backupBuckets", associatedBackupBuckets)
			r.Recorder.Event(seed, corev1.EventTypeNormal, v1beta1constants.EventResourceReferenced, fmt.Sprintf("%s BackupBuckets=%v", parentLogMessage, associatedBackupBuckets))
		}

		return reconcile.Result{}, errors.New("seed still has references")
	}

	log.Info("No Shoots or BackupBuckets are referencing the Seed, deletion accepted")

	if err := r.runDeleteSeedFlow(ctx, log, seedObj); err != nil {
		conditionSeedBootstrapped := gardencorev1beta1helper.GetOrInitCondition(seedObj.GetInfo().Status.Conditions, gardencorev1beta1.SeedBootstrapped)
		conditionSeedBootstrapped = gardencorev1beta1helper.UpdatedCondition(conditionSeedBootstrapped, gardencorev1beta1.ConditionFalse, "DebootstrapFailed", fmt.Sprintf("Failed to delete Seed Cluster (%s).", err.Error()))
		if err := r.patchSeedStatus(ctx, r.GardenClient, seed, "<unknown>", nil, nil, conditionSeedBootstrapped); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not patch seed status after deletion flow failed: %w", err)
		}
		return reconcile.Result{}, err
	}

	// Remove finalizer from referenced secret
	if seed.Spec.SecretRef != nil {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: seed.Spec.SecretRef.Name, Namespace: seed.Spec.SecretRef.Namespace}}
		if err := r.GardenClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err == nil {
			if controllerutil.ContainsFinalizer(secret, gardencorev1beta1.ExternalGardenerName) {
				log.Info("Removing finalizer from secret", "secret", client.ObjectKeyFromObject(secret))
				if err := controllerutils.RemoveFinalizers(ctx, r.GardenClient, secret, gardencorev1beta1.ExternalGardenerName); err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to remove finalizer from secret: %w", err)
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("failed to get Seed secret '%s/%s': %w", secret.Namespace, secret.Name, err)
		}
	}

	// Remove finalizer from Seed
	if controllerutil.ContainsFinalizer(seed, gardencorev1beta1.GardenerName) {
		log.Info("Removing finalizer")
		if err := controllerutils.RemoveFinalizers(ctx, r.GardenClient, seed, gardencorev1beta1.GardenerName); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return reconcile.Result{}, nil
}

func (r *Reconciler) runDeleteSeedFlow(ctx context.Context, log logr.Logger, seed *seedpkg.Seed) error {
	seedClient := r.SeedClientSet.Client()
	kubernetesVersion, err := semver.NewVersion(r.SeedClientSet.Version())
	if err != nil {
		return err
	}

	secretData, err := getDNSProviderSecretData(ctx, r.GardenClient, seed.GetInfo())
	if err != nil {
		return err
	}

	istioIngressGateway := []istio.IngressGateway{{Namespace: *r.Config.SNI.Ingress.Namespace}}
	// Add for each ExposureClass handler in the config an own Ingress Gateway.
	for _, handler := range r.Config.ExposureClassHandlers {
		istioIngressGateway = append(istioIngressGateway, istio.IngressGateway{Namespace: *handler.SNI.Ingress.Namespace})
	}

	// Delete all ingress objects in garden namespace which are not created as part of ManagedResources. This can be
	// removed once all seed system components are deployed as part of ManagedResources.
	// See https://github.com/gardener/gardener/issues/6062 for details.
	if err := seedClient.DeleteAllOf(ctx, &networkingv1.Ingress{}, client.InNamespace(r.GardenNamespace)); err != nil {
		return err
	}

	// setup for flow graph
	var (
		dnsRecord        = getManagedIngressDNSRecord(log, seedClient, r.GardenNamespace, seed.GetInfo().Spec.DNS, secretData, seed.GetIngressFQDN("*"), "")
		autoscaler       = clusterautoscaler.NewBootstrapper(seedClient, r.GardenNamespace)
		gsac             = seedadmissioncontroller.New(seedClient, r.GardenNamespace, nil, seedadmissioncontroller.Values{})
		hvpa             = hvpa.New(seedClient, r.GardenNamespace, hvpa.Values{})
		kubeStateMetrics = kubestatemetrics.New(seedClient, r.GardenNamespace, nil, kubestatemetrics.Values{ClusterType: component.ClusterTypeSeed})
		resourceManager  = resourcemanager.New(seedClient, r.GardenNamespace, nil, resourcemanager.Values{Version: kubernetesVersion})
		nginxIngress     = nginxingress.New(seedClient, r.GardenNamespace, nginxingress.Values{})
		etcdDruid        = etcd.NewBootstrapper(seedClient, r.GardenNamespace, &r.Config, "", nil)
		networkPolicies  = networkpolicies.NewBootstrapper(seedClient, r.GardenNamespace, networkpolicies.GlobalValues{})
		clusterIdentity  = clusteridentity.NewForSeed(seedClient, r.GardenNamespace, "")
		dwdEndpoint      = dependencywatchdog.NewBootstrapper(seedClient, r.GardenNamespace, dependencywatchdog.BootstrapperValues{Role: dependencywatchdog.RoleEndpoint})
		dwdProbe         = dependencywatchdog.NewBootstrapper(seedClient, r.GardenNamespace, dependencywatchdog.BootstrapperValues{Role: dependencywatchdog.RoleProbe})
		systemResources  = seedsystem.New(seedClient, r.GardenNamespace, seedsystem.Values{})
		vpa              = vpa.New(seedClient, r.GardenNamespace, nil, vpa.Values{ClusterType: component.ClusterTypeSeed})
		vpnAuthzServer   = vpnauthzserver.New(seedClient, r.GardenNamespace, "", 1, kubernetesVersion)
		istioCRDs        = istio.NewIstioCRD(r.SeedClientSet.ChartApplier(), seedClient)
		istio            = istio.NewIstio(seedClient, r.SeedClientSet.ChartRenderer(), istio.IstiodValues{}, v1beta1constants.IstioSystemNamespace, istioIngressGateway, nil)
	)

	scheduler, err := gardenerkubescheduler.Bootstrap(seedClient, nil, r.GardenNamespace, nil, kubernetesVersion)
	if err != nil {
		return err
	}

	var (
		g                = flow.NewGraph("Seed cluster deletion")
		destroyDNSRecord = g.Add(flow.Task{
			Name: "Destroying managed ingress DNS record (if existing)",
			Fn:   func(ctx context.Context) error { return destroyDNSResources(ctx, dnsRecord) },
		})
		noControllerInstallations = g.Add(flow.Task{
			Name:         "Ensuring no ControllerInstallations are left",
			Fn:           ensureNoControllerInstallations(r.GardenClient, seed.GetInfo().Name),
			Dependencies: flow.NewTaskIDs(destroyDNSRecord),
		})
		destroyEtcdDruid = g.Add(flow.Task{
			Name: "Destroying etcd druid",
			Fn:   component.OpDestroyAndWait(etcdDruid).Destroy,
			// only destroy Etcd CRD once all extension controllers are gone, otherwise they might not be able to start up
			// again (e.g. after being evicted by VPA)
			// see https://github.com/gardener/gardener/issues/6487#issuecomment-1220597217
			Dependencies: flow.NewTaskIDs(noControllerInstallations),
		})
		destroyClusterIdentity = g.Add(flow.Task{
			Name: "Destroying cluster-identity",
			Fn:   component.OpDestroyAndWait(clusterIdentity).Destroy,
		})
		destroyClusterAutoscaler = g.Add(flow.Task{
			Name: "Destroying cluster-autoscaler",
			Fn:   component.OpDestroyAndWait(autoscaler).Destroy,
		})
		destroySeedAdmissionController = g.Add(flow.Task{
			Name: "Destroying gardener-seed-admission-controller",
			Fn:   component.OpDestroyAndWait(gsac).Destroy,
		})
		destroyNginxIngress = g.Add(flow.Task{
			Name: "Destroying nginx-ingress",
			Fn:   component.OpDestroyAndWait(nginxIngress).Destroy,
		})
		destroyKubeScheduler = g.Add(flow.Task{
			Name: "Destroying kube-scheduler",
			Fn:   component.OpDestroyAndWait(scheduler).Destroy,
		})
		destroyNetworkPolicies = g.Add(flow.Task{
			Name: "Destroy network policies",
			Fn:   component.OpDestroyAndWait(networkPolicies).Destroy,
		})
		destroyDWDEndpoint = g.Add(flow.Task{
			Name: "Destroy dependency-watchdog-endpoint",
			Fn:   component.OpDestroyAndWait(dwdEndpoint).Destroy,
		})
		destroyDWDProbe = g.Add(flow.Task{
			Name: "Destroy dependency-watchdog-probe",
			Fn:   component.OpDestroyAndWait(dwdProbe).Destroy,
		})
		destroyHVPA = g.Add(flow.Task{
			Name: "Destroy HVPA controller",
			Fn:   component.OpDestroyAndWait(hvpa).Destroy,
		})
		destroyVPA = g.Add(flow.Task{
			Name: "Destroy Kubernetes vertical pod autoscaler",
			Fn:   component.OpDestroyAndWait(vpa).Destroy,
		})
		destroyKubeStateMetrics = g.Add(flow.Task{
			Name: "Destroy kube-state-metrics",
			Fn:   component.OpDestroyAndWait(kubeStateMetrics).Destroy,
		})
		destroyVPNAuthzServer = g.Add(flow.Task{
			Name: "Destroy VPN authorization server",
			Fn:   component.OpDestroyAndWait(vpnAuthzServer).Destroy,
		})
		destroyIstio = g.Add(flow.Task{
			Name: "Destroy Istio",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return component.OpDestroyAndWait(istio).Destroy(ctx)
			}).DoIf(gardenletfeatures.FeatureGate.Enabled(features.ManagedIstio)),
		})
		destroyIstioCRDs = g.Add(flow.Task{
			Name: "Destroy Istio CRDs",
			Fn: flow.TaskFn(func(ctx context.Context) error {
				return component.OpDestroyAndWait(istioCRDs).Destroy(ctx)
			}).DoIf(gardenletfeatures.FeatureGate.Enabled(features.ManagedIstio)),
			Dependencies: flow.NewTaskIDs(destroyIstio),
		})
		syncPointCleanedUp = flow.NewTaskIDs(
			destroySeedAdmissionController,
			destroyNginxIngress,
			destroyEtcdDruid,
			destroyClusterIdentity,
			destroyClusterAutoscaler,
			destroyKubeScheduler,
			destroyNetworkPolicies,
			destroyDWDEndpoint,
			destroyDWDProbe,
			destroyHVPA,
			destroyVPA,
			destroyKubeStateMetrics,
			destroyVPNAuthzServer,
			destroyIstio,
			destroyIstioCRDs,
			noControllerInstallations,
		)
		destroySystemResources = g.Add(flow.Task{
			Name:         "Destroy system resources",
			Fn:           component.OpDestroyAndWait(systemResources).Destroy,
			Dependencies: flow.NewTaskIDs(syncPointCleanedUp),
		})
		_ = g.Add(flow.Task{
			Name:         "Destroying gardener-resource-manager",
			Fn:           resourceManager.Destroy,
			Dependencies: flow.NewTaskIDs(destroySystemResources),
		})
	)

	if err := g.Compile().Run(ctx, flow.Opts{Log: log}); err != nil {
		return flow.Errors(err)
	}

	return nil
}

func ensureNoControllerInstallations(c client.Client, seedName string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		associatedControllerInstallations, err := controllerutils.DetermineControllerInstallationAssociations(ctx, c, seedName)
		if err != nil {
			return err
		}

		if associatedControllerInstallations != nil {
			return fmt.Errorf("can't continue with Seed deletion, because the following objects are still referencing it: ControllerInstallations=%v", associatedControllerInstallations)
		}

		return nil
	}
}
