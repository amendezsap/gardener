// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package botanist

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gardener/gardener/charts"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/operation"
	"github.com/gardener/gardener/pkg/operation/botanist/component/etcd"
	"github.com/gardener/gardener/pkg/operation/botanist/component/hvpa"
	"github.com/gardener/gardener/pkg/operation/botanist/component/logging/kuberbacproxy"
	shootpkg "github.com/gardener/gardener/pkg/operation/shoot"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultInterval is the default interval for retry operations.
const DefaultInterval = 5 * time.Second

var (
	// ChartsPath is an alias for charts.Path. Exposed for testing.
	ChartsPath = charts.Path

	ingressTLSCertificateValidity = 730 * 24 * time.Hour // ~2 years, see https://support.apple.com/en-us/HT210176
)

// New takes an operation object <o> and creates a new Botanist object. It checks whether the given Shoot DNS
// domain is covered by a default domain, and if so, it sets the <DefaultDomainSecret> attribute on the Botanist
// object.
func New(ctx context.Context, o *operation.Operation) (*Botanist, error) {
	var (
		b   = &Botanist{Operation: o}
		err error
	)

	// Determine all default domain secrets and check whether the used Shoot domain matches a default domain.
	if o.Shoot != nil && o.Shoot.GetInfo().Spec.DNS != nil && o.Shoot.GetInfo().Spec.DNS.Domain != nil {
		var (
			prefix            = fmt.Sprintf("%s-", v1beta1constants.GardenRoleDefaultDomain)
			defaultDomainKeys = o.GetSecretKeysOfRole(v1beta1constants.GardenRoleDefaultDomain)
		)
		sort.Slice(defaultDomainKeys, func(i, j int) bool { return len(defaultDomainKeys[i]) >= len(defaultDomainKeys[j]) })
		for _, key := range defaultDomainKeys {
			defaultDomain := strings.SplitAfter(key, prefix)[1]
			if strings.HasSuffix(*(o.Shoot.GetInfo().Spec.DNS.Domain), defaultDomain) {
				b.DefaultDomainSecret = b.LoadSecret(prefix + defaultDomain)
				break
			}
		}
	}

	o.SecretsManager, err = secretsmanager.New(
		ctx,
		b.Logger.WithName("secretsmanager"),
		clock.RealClock{},
		b.SeedClientSet.Client(),
		b.Shoot.SeedNamespace,
		v1beta1constants.SecretManagerIdentityGardenlet,
		secretsmanager.Config{
			CASecretAutoRotation: false,
			SecretNamesToTimes:   b.lastSecretRotationStartTimes(),
		},
	)
	if err != nil {
		return nil, err
	}

	// extension components
	o.Shoot.Components.Extensions.ContainerRuntime = b.DefaultContainerRuntime()
	o.Shoot.Components.Extensions.ControlPlane = b.DefaultControlPlane(extensionsv1alpha1.Normal)
	o.Shoot.Components.Extensions.ControlPlaneExposure = b.DefaultControlPlane(extensionsv1alpha1.Exposure)
	o.Shoot.Components.Extensions.ExternalDNSRecord = b.DefaultExternalDNSRecord()
	o.Shoot.Components.Extensions.InternalDNSRecord = b.DefaultInternalDNSRecord()
	o.Shoot.Components.Extensions.IngressDNSRecord = b.DefaultIngressDNSRecord()
	o.Shoot.Components.Extensions.OwnerDNSRecord = b.DefaultOwnerDNSRecord()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.Extensions.Extension, err = b.DefaultExtension(ctx)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.Extensions.Infrastructure = b.DefaultInfrastructure()
	o.Shoot.Components.Extensions.Network = b.DefaultNetwork()
	o.Shoot.Components.Extensions.OperatingSystemConfig, err = b.DefaultOperatingSystemConfig()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.Extensions.Worker = b.DefaultWorker()

	sniPhase, err := b.SNIPhase(ctx)
	if err != nil {
		return nil, err
	}

	// control plane components

	o.Shoot.Components.ControlPlane.ClusterAutoscaler, err = b.DefaultClusterAutoscaler()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.EtcdCopyBackupsTask = b.DefaultEtcdCopyBackupsTask()
	o.Shoot.Components.ControlPlane.EtcdMain, err = b.DefaultEtcd(v1beta1constants.ETCDRoleMain, etcd.ClassImportant)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.EtcdEvents, err = b.DefaultEtcd(v1beta1constants.ETCDRoleEvents, etcd.ClassNormal)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.KubeAPIServerService = b.DefaultKubeAPIServerService(sniPhase)
	o.Shoot.Components.ControlPlane.KubeAPIServerSNI = b.DefaultKubeAPIServerSNI()
	o.Shoot.Components.ControlPlane.KubeAPIServerSNIPhase = sniPhase
	o.Shoot.Components.ControlPlane.KubeAPIServer, err = b.DefaultKubeAPIServer(ctx)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.KubeScheduler, err = b.DefaultKubeScheduler()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.KubeControllerManager, err = b.DefaultKubeControllerManager()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.KubeStateMetrics, err = b.DefaultKubeStateMetrics()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.ResourceManager, err = b.DefaultResourceManager()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.VerticalPodAutoscaler, err = b.DefaultVerticalPodAutoscaler()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.ControlPlane.VPNSeedServer, err = b.DefaultVPNSeedServer()
	if err != nil {
		return nil, err
	}

	// system components
	o.Shoot.Components.SystemComponents.ClusterIdentity = b.DefaultClusterIdentity()
	o.Shoot.Components.SystemComponents.Namespaces = b.DefaultShootNamespaces()
	o.Shoot.Components.SystemComponents.CoreDNS, err = b.DefaultCoreDNS()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.SystemComponents.NodeLocalDNS, err = b.DefaultNodeLocalDNS()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.SystemComponents.MetricsServer, err = b.DefaultMetricsServer()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.SystemComponents.Resources = b.DefaultShootSystem()
	o.Shoot.Components.SystemComponents.VPNShoot, err = b.DefaultVPNShoot()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.SystemComponents.NodeProblemDetector, err = b.DefaultNodeProblemDetector()
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.SystemComponents.KubeProxy, err = b.DefaultKubeProxy()
	if err != nil {
		return nil, err
	}

	// other components
	o.Shoot.Components.SourceBackupEntry = b.SourceBackupEntry()
	o.Shoot.Components.BackupEntry = b.DefaultCoreBackupEntry()
	o.Shoot.Components.DependencyWatchdogAccess = b.DefaultDependencyWatchdogAccess()
	o.Shoot.Components.GardenerAccess = b.DefaultGardenerAccess()
	o.Shoot.Components.NetworkPolicies, err = b.DefaultNetworkPolicies(sniPhase)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.HVPA = hvpa.New(nil, b.Shoot.SeedNamespace, hvpa.Values{})

	// Logging
	o.Shoot.Components.Logging.ShootRBACProxy, err = kuberbacproxy.New(b.SeedClientSet.Client(), b.Shoot.SeedNamespace)
	if err != nil {
		return nil, err
	}
	o.Shoot.Components.Logging.ShootEventLogger, err = b.DefaultEventLogger()
	if err != nil {
		return nil, err
	}

	return b, nil
}

// RequiredExtensionsReady checks whether all required extensions needed for a shoot operation exist and are ready.
func (b *Botanist) RequiredExtensionsReady(ctx context.Context) error {
	controllerRegistrationList := &gardencorev1beta1.ControllerRegistrationList{}
	if err := b.GardenClient.List(ctx, controllerRegistrationList); err != nil {
		return err
	}

	controllerInstallationList := &gardencorev1beta1.ControllerInstallationList{}
	if err := b.GardenClient.List(ctx, controllerInstallationList); err != nil {
		return err
	}

	requiredExtensions := shootpkg.ComputeRequiredExtensions(b.Shoot.GetInfo(), b.Seed.GetInfo(), controllerRegistrationList, b.Garden.InternalDomain, b.Shoot.ExternalDomain)

	for _, controllerInstallation := range controllerInstallationList.Items {
		if controllerInstallation.Spec.SeedRef.Name != b.Seed.GetInfo().Name {
			continue
		}

		controllerRegistration := &gardencorev1beta1.ControllerRegistration{}
		if err := b.GardenClient.Get(ctx, client.ObjectKey{Name: controllerInstallation.Spec.RegistrationRef.Name}, controllerRegistration); err != nil {
			return err
		}

		for _, kindType := range requiredExtensions.UnsortedList() {
			split := strings.Split(kindType, "/")
			if len(split) != 2 {
				return fmt.Errorf("unexpected required extension: %q", kindType)
			}
			extensionKind, extensionType := split[0], split[1]

			if helper.IsResourceSupported(controllerRegistration.Spec.Resources, extensionKind, extensionType) && helper.IsControllerInstallationSuccessful(controllerInstallation) {
				requiredExtensions.Delete(kindType)
			}
		}
	}

	if len(requiredExtensions) > 0 {
		return fmt.Errorf("extension controllers missing or unready: %+v", requiredExtensions)
	}

	return nil
}

// GetFailureToleranceType determines the FailureToleranceType by looking at both the HAControlPlanes feature gate and alpha HA annotations/Shoot.spec.ControlPlane.
func (b *Botanist) GetFailureToleranceType() *gardencorev1beta1.FailureToleranceType {
	return helper.GetFailureToleranceType(b.Shoot.GetInfo())
}
