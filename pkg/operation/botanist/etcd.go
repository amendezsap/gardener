// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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
	"hash/crc32"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/features"
	"github.com/gardener/gardener/pkg/gardenlet/apis/config"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	"github.com/gardener/gardener/pkg/operation/botanist/component/etcd"
	"github.com/gardener/gardener/pkg/operation/shoot"
	"github.com/gardener/gardener/pkg/utils/flow"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/timewindow"

	hvpav1alpha1 "github.com/gardener/hvpa-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
)

// NewEtcd is a function exposed for testing.
var NewEtcd = etcd.New

// DefaultEtcd returns a deployer for the etcd.
func (b *Botanist) DefaultEtcd(role string, class etcd.Class) (etcd.Interface, error) {
	defragmentationSchedule, err := determineDefragmentationSchedule(b.Shoot.GetInfo(), b.ManagedSeed, class)
	if err != nil {
		return nil, err
	}

	var replicas *int32
	if !b.Shoot.HibernationEnabled {
		replicas = pointer.Int32(getEtcdReplicas(b.Shoot.GetInfo()))
	}

	e := NewEtcd(
		b.SeedClientSet.Client(),
		b.Logger,
		b.Shoot.SeedNamespace,
		b.SecretsManager,
		role,
		class,
		b.Shoot.GetInfo().ObjectMeta.Annotations,
		b.GetFailureToleranceType(),
		replicas,
		b.Seed.GetValidVolumeSize("10Gi"),
		&defragmentationSchedule,
		gardencorev1beta1helper.GetShootCARotationPhase(b.Shoot.GetInfo().Status.Credentials),
		b.ShootVersion(),
	)

	hvpaEnabled := gardenletfeatures.FeatureGate.Enabled(features.HVPA)
	if b.ManagedSeed != nil {
		hvpaEnabled = gardenletfeatures.FeatureGate.Enabled(features.HVPAForShootedSeed)
	}

	e.SetHVPAConfig(&etcd.HVPAConfig{
		Enabled:               hvpaEnabled,
		MaintenanceTimeWindow: *b.Shoot.GetInfo().Spec.Maintenance.TimeWindow,
		ScaleDownUpdateMode:   getScaleDownUpdateMode(class, b.Shoot),
	})

	return e, nil
}

func getScaleDownUpdateMode(c etcd.Class, s *shoot.Shoot) *string {
	if c == etcd.ClassImportant && (s.Purpose == gardencorev1beta1.ShootPurposeProduction || s.Purpose == gardencorev1beta1.ShootPurposeInfrastructure) {
		return pointer.String(hvpav1alpha1.UpdateModeOff)
	}
	if metav1.HasAnnotation(s.GetInfo().ObjectMeta, v1beta1constants.ShootAlphaControlPlaneScaleDownDisabled) {
		return pointer.String(hvpav1alpha1.UpdateModeOff)
	}
	return pointer.String(hvpav1alpha1.UpdateModeMaintenanceWindow)
}

// DeployEtcd deploys the etcd main and events.
func (b *Botanist) DeployEtcd(ctx context.Context) error {
	if b.Seed.GetInfo().Spec.Backup != nil {
		secret := &corev1.Secret{}
		if err := b.SeedClientSet.Client().Get(ctx, kutil.Key(b.Shoot.SeedNamespace, v1beta1constants.BackupSecretName), secret); err != nil {
			return err
		}

		snapshotSchedule, err := determineBackupSchedule(b.Shoot.GetInfo())
		if err != nil {
			return err
		}

		var backupLeaderElection *config.ETCDBackupLeaderElection
		if b.Config != nil && b.Config.ETCDConfig != nil {
			backupLeaderElection = b.Config.ETCDConfig.BackupLeaderElection
		}

		b.Shoot.Components.ControlPlane.EtcdMain.SetBackupConfig(&etcd.BackupConfig{
			Provider:             b.Seed.GetInfo().Spec.Backup.Provider,
			SecretRefName:        v1beta1constants.BackupSecretName,
			Prefix:               b.Shoot.BackupEntryName,
			Container:            string(secret.Data[v1beta1constants.DataKeyBackupBucketName]),
			FullSnapshotSchedule: snapshotSchedule,
			LeaderElection:       backupLeaderElection,
		})

		// Owner checks are only enabled if the `etcd-main` resource is deployed with 1 replica.
		// They must not be used for clustered etcd. Ref: https://github.com/gardener/gardener/issues/6302
		if gardencorev1beta1helper.SeedSettingOwnerChecksEnabled(b.Seed.GetInfo().Spec.Settings) &&
			getEtcdReplicas(b.Shoot.GetInfo()) == 1 {
			b.Shoot.Components.ControlPlane.EtcdMain.SetOwnerCheckConfig(&etcd.OwnerCheckConfig{
				Name: gutil.GetOwnerDomain(b.Shoot.InternalClusterDomain),
				ID:   *b.Seed.GetInfo().Status.ClusterIdentity,
			})
		}
	}

	// Roll out the new peer CA first so that every member in the cluster trusts the old and the new CA.
	// This is required because peer certificates which are used for client and server authentication at the same time,
	// are re-created with the new CA in the `Deploy` step.
	if gardencorev1beta1helper.GetShootCARotationPhase(b.Shoot.GetInfo().Status.Credentials) == gardencorev1beta1.RotationPreparing {
		if err := flow.Parallel(
			b.Shoot.Components.ControlPlane.EtcdMain.RolloutPeerCA,
			b.Shoot.Components.ControlPlane.EtcdEvents.RolloutPeerCA,
		)(ctx); err != nil {
			return err
		}

		if err := b.WaitUntilEtcdsReady(ctx); err != nil {
			return err
		}
	}

	return flow.Parallel(
		b.Shoot.Components.ControlPlane.EtcdMain.Deploy,
		b.Shoot.Components.ControlPlane.EtcdEvents.Deploy,
	)(ctx)
}

// WaitUntilEtcdsReady waits until both etcd-main and etcd-events are ready.
func (b *Botanist) WaitUntilEtcdsReady(ctx context.Context) error {
	return flow.Parallel(
		b.Shoot.Components.ControlPlane.EtcdMain.Wait,
		b.Shoot.Components.ControlPlane.EtcdEvents.Wait,
	)(ctx)
}

// DestroyEtcd destroys the etcd main and events.
func (b *Botanist) DestroyEtcd(ctx context.Context) error {
	return flow.Parallel(
		b.Shoot.Components.ControlPlane.EtcdMain.Destroy,
		b.Shoot.Components.ControlPlane.EtcdEvents.Destroy,
	)(ctx)
}

// WaitUntilEtcdsDeleted waits until both etcd-main and etcd-events are deleted.
func (b *Botanist) WaitUntilEtcdsDeleted(ctx context.Context) error {
	return flow.Parallel(
		b.Shoot.Components.ControlPlane.EtcdMain.WaitCleanup,
		b.Shoot.Components.ControlPlane.EtcdEvents.WaitCleanup,
	)(ctx)
}

// SnapshotEtcd executes into the etcd-main pod and triggers a full snapshot.
func (b *Botanist) SnapshotEtcd(ctx context.Context) error {
	return b.Shoot.Components.ControlPlane.EtcdMain.Snapshot(ctx, kubernetes.NewPodExecutor(b.SeedClientSet.RESTConfig()))
}

// ScaleETCDToZero scales ETCD main and events replicas to zero.
func (b *Botanist) ScaleETCDToZero(ctx context.Context) error {
	return b.scaleETCD(ctx, 0)
}

// ScaleUpETCD scales ETCD main and events replicas to the configured replica count.
func (b *Botanist) ScaleUpETCD(ctx context.Context) error {
	return b.scaleETCD(ctx, getEtcdReplicas(b.Shoot.GetInfo()))
}

func (b *Botanist) scaleETCD(ctx context.Context, replicas int32) error {
	if err := b.Shoot.Components.ControlPlane.EtcdMain.Scale(ctx, replicas); err != nil {
		return err
	}
	return b.Shoot.Components.ControlPlane.EtcdEvents.Scale(ctx, replicas)
}

func determineBackupSchedule(shoot *gardencorev1beta1.Shoot) (string, error) {
	schedule := "%d %d * * *"

	return determineSchedule(shoot, schedule, func(maintenanceTimeWindow *timewindow.MaintenanceTimeWindow, shootUID types.UID) string {
		// Randomize the snapshot timing daily but within last hour.
		// The 15 minutes buffer is set to snapshot upload time before actual maintenance window start.
		snapshotWindowBegin := maintenanceTimeWindow.Begin().Add(-1, -15, 0)
		randomMinutes := int(crc32.ChecksumIEEE([]byte(shootUID)) % 60)
		snapshotTime := snapshotWindowBegin.Add(0, randomMinutes, 0)
		return fmt.Sprintf(schedule, snapshotTime.Minute(), snapshotTime.Hour())
	})
}

func determineDefragmentationSchedule(shoot *gardencorev1beta1.Shoot, managedSeed *seedmanagementv1alpha1.ManagedSeed, class etcd.Class) (string, error) {
	schedule := "%d %d */3 * *"
	if managedSeed != nil && class == etcd.ClassImportant {
		// defrag important etcds of ManagedSeeds daily in the maintenance window
		schedule = "%d %d * * *"
	}

	return determineSchedule(shoot, schedule, func(maintenanceTimeWindow *timewindow.MaintenanceTimeWindow, shootUID types.UID) string {
		// Randomize the defragmentation timing but within the maintenance window.
		maintenanceWindowBegin := maintenanceTimeWindow.Begin()
		windowInMinutes := uint32(maintenanceTimeWindow.Duration().Minutes())
		randomMinutes := int(crc32.ChecksumIEEE([]byte(shootUID)) % windowInMinutes)
		maintenanceTime := maintenanceWindowBegin.Add(0, randomMinutes, 0)
		return fmt.Sprintf(schedule, maintenanceTime.Minute(), maintenanceTime.Hour())
	})
}

func determineSchedule(shoot *gardencorev1beta1.Shoot, schedule string, f func(*timewindow.MaintenanceTimeWindow, types.UID) string) (string, error) {
	var (
		begin, end string
		shootUID   types.UID
	)

	if shoot.Spec.Maintenance != nil && shoot.Spec.Maintenance.TimeWindow != nil {
		begin = shoot.Spec.Maintenance.TimeWindow.Begin
		end = shoot.Spec.Maintenance.TimeWindow.End
		shootUID = shoot.Status.UID
	}

	if len(begin) != 0 && len(end) != 0 {
		maintenanceTimeWindow, err := timewindow.ParseMaintenanceTimeWindow(begin, end)
		if err != nil {
			return "", err
		}

		if !maintenanceTimeWindow.Equal(timewindow.AlwaysTimeWindow) {
			return f(maintenanceTimeWindow, shootUID), nil
		}
	}

	creationMinute := shoot.CreationTimestamp.Minute()
	creationHour := shoot.CreationTimestamp.Hour()
	return fmt.Sprintf(schedule, creationMinute, creationHour), nil
}

func getEtcdReplicas(shoot *gardencorev1beta1.Shoot) int32 {
	if gardencorev1beta1helper.IsHAControlPlaneConfigured(shoot) {
		return 3
	}
	return 1
}
