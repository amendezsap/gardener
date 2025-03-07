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

package shoot

import (
	"context"
	"fmt"
	"time"

	"github.com/gardener/gardener/pkg/apis/core"
	versionutils "github.com/gardener/gardener/pkg/utils/version"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

// GetWarnings returns warnings for the provided shoot.
func GetWarnings(_ context.Context, shoot, oldShoot *core.Shoot, credentialsRotationInterval time.Duration) []string {
	if shoot == nil {
		return nil
	}

	var warnings []string

	if pointer.BoolDeref(shoot.Spec.Kubernetes.EnableStaticTokenKubeconfig, true) {
		warnings = append(warnings, "you should consider disabling the static token kubeconfig, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_access.md for details")
	}

	if oldShoot != nil {
		warnings = append(warnings, getWarningsForDueCredentialsRotations(shoot, credentialsRotationInterval)...)
		warnings = append(warnings, getWarningsForIncompleteCredentialsRotation(shoot, credentialsRotationInterval)...)

		// Errors are ignored here because we cannot do anything meaningful with them - variables will default to `false`.
		k8sLess125, _ := versionutils.CheckVersionMeetsConstraint(shoot.Spec.Kubernetes.Version, "< 1.25")
		k8sGreaterEqual123, _ := versionutils.CheckVersionMeetsConstraint(shoot.Spec.Kubernetes.Version, ">= 1.23")
		if k8sLess125 && k8sGreaterEqual123 {
			if warning := getWarningsForPSPAdmissionPlugin(shoot); warning != "" {
				warnings = append(warnings, warning)
			}
		}
	}

	return warnings
}

func getWarningsForDueCredentialsRotations(shoot *core.Shoot, credentialsRotationInterval time.Duration) []string {
	if !isOldEnough(shoot.CreationTimestamp.Time, credentialsRotationInterval) {
		return nil
	}

	if shoot.Status.Credentials == nil || shoot.Status.Credentials.Rotation == nil {
		return []string{"you should consider rotating the shoot credentials, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#gardener-provided-credentials for details"}
	}

	var (
		rotation = shoot.Status.Credentials.Rotation
		warnings []string
	)

	if rotation.CertificateAuthorities == nil || initiationDue(rotation.CertificateAuthorities.LastInitiationTime, credentialsRotationInterval) {
		warnings = append(warnings, "you should consider rotating the certificate authorities, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#certificate-authorities for details")
	}

	if rotation.ETCDEncryptionKey == nil || initiationDue(rotation.ETCDEncryptionKey.LastInitiationTime, credentialsRotationInterval) {
		warnings = append(warnings, "you should consider rotating the ETCD encryption key, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#etcd-encryption-key for details")
	}

	if pointer.BoolDeref(shoot.Spec.Kubernetes.EnableStaticTokenKubeconfig, true) &&
		(rotation.Kubeconfig == nil || initiationDue(rotation.Kubeconfig.LastInitiationTime, credentialsRotationInterval)) {
		warnings = append(warnings, "you should consider rotating the static token kubeconfig, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#kubeconfig for details")
	}

	if (shoot.Spec.Purpose == nil || *shoot.Spec.Purpose != core.ShootPurposeTesting) &&
		(rotation.Observability == nil || initiationDue(rotation.Observability.LastInitiationTime, credentialsRotationInterval)) {
		warnings = append(warnings, "you should consider rotating the observability passwords, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#observability-passwords-for-grafana for details")
	}

	if rotation.ServiceAccountKey == nil || initiationDue(rotation.ServiceAccountKey.LastInitiationTime, credentialsRotationInterval) {
		warnings = append(warnings, "you should consider rotating the ServiceAccount token signing key, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#serviceaccount-token-signing-key for details")
	}

	if rotation.SSHKeypair == nil || initiationDue(rotation.SSHKeypair.LastInitiationTime, credentialsRotationInterval) {
		warnings = append(warnings, "you should consider rotating the SSH keypair, see https://github.com/gardener/gardener/blob/master/docs/usage/shoot_credentials_rotation.md#ssh-key-pair-for-worker-nodes for details")
	}

	return warnings
}

func getWarningsForIncompleteCredentialsRotation(shoot *core.Shoot, credentialsRotationInterval time.Duration) []string {
	if shoot.Status.Credentials == nil || shoot.Status.Credentials.Rotation == nil {
		return nil
	}

	var (
		warnings                      []string
		recommendedCompletionInterval = credentialsRotationInterval / 3
		rotation                      = shoot.Status.Credentials.Rotation
	)

	// Only consider credentials for which completion must be triggered explicitly by the user. Credentials which are
	// rotated in "one phase" are excluded.
	if rotation.CertificateAuthorities != nil && completionDue(rotation.CertificateAuthorities.LastInitiationTime, rotation.CertificateAuthorities.LastCompletionTime, recommendedCompletionInterval) {
		warnings = append(warnings, completionWarning("certificate authorities", recommendedCompletionInterval))
	}
	if rotation.ETCDEncryptionKey != nil && completionDue(rotation.ETCDEncryptionKey.LastInitiationTime, rotation.ETCDEncryptionKey.LastCompletionTime, recommendedCompletionInterval) {
		warnings = append(warnings, completionWarning("ETCD encryption key", recommendedCompletionInterval))
	}
	if rotation.ServiceAccountKey != nil && completionDue(rotation.ServiceAccountKey.LastInitiationTime, rotation.ServiceAccountKey.LastCompletionTime, recommendedCompletionInterval) {
		warnings = append(warnings, completionWarning("ServiceAccount token signing key", recommendedCompletionInterval))
	}

	return warnings
}

func initiationDue(lastInitiationTime *metav1.Time, threshold time.Duration) bool {
	return lastInitiationTime == nil || isOldEnough(lastInitiationTime.Time, threshold)
}

func completionDue(lastInitiationTime, lastCompletionTime *metav1.Time, threshold time.Duration) bool {
	if lastInitiationTime == nil {
		return false
	}
	if lastCompletionTime != nil && lastCompletionTime.Time.UTC().After(lastInitiationTime.Time.UTC()) {
		return false
	}
	return isOldEnough(lastInitiationTime.Time, threshold)
}

func isOldEnough(t time.Time, threshold time.Duration) bool {
	return t.UTC().Add(threshold).Before(time.Now().UTC())
}

func completionWarning(credentials string, recommendedCompletionInterval time.Duration) string {
	return fmt.Sprintf("the %s rotation was initiated more than %s ago and should be completed", credentials, recommendedCompletionInterval)
}

func getWarningsForPSPAdmissionPlugin(shoot *core.Shoot) string {
	if shoot.Spec.Kubernetes.KubeAPIServer != nil {
		for _, plugin := range shoot.Spec.Kubernetes.KubeAPIServer.AdmissionPlugins {
			if plugin.Name == "PodSecurityPolicy" && pointer.BoolDeref(plugin.Disabled, false) {
				return ""
			}
		}
	}

	return "you should consider migrating to PodSecurity, see https://github.com/gardener/gardener/blob/master/docs/usage/pod-security.md#migrating-from-podsecuritypolicys-to-podsecurity-admission-controller for details"
}
