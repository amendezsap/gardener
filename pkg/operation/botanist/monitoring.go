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
	"path/filepath"
	"strings"
	"time"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/features"
	gardenlethelper "github.com/gardener/gardener/pkg/gardenlet/apis/config/helper"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	"github.com/gardener/gardener/pkg/operation/botanist/component/etcd"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/utils"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/images"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/secrets"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpaautoscalingv1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	secretNameIngressOperators = v1beta1constants.SecretNameObservabilityIngress
	secretNameIngressUsers     = v1beta1constants.SecretNameObservabilityIngressUsers

	grafanaOperatorsRole = "operators"
	grafanaUsersRole     = "users"
)

func observabilityIngressSecretConfig(name string) *secrets.BasicAuthSecretConfig {
	return &secrets.BasicAuthSecretConfig{
		Name:           name,
		Format:         secrets.BasicAuthFormatNormal,
		Username:       "admin",
		PasswordLength: 32,
	}
}

// DeploySeedMonitoring installs the Helm release "seed-monitoring" in the Seed clusters. It comprises components
// to monitor the Shoot cluster whose control plane runs in the Seed cluster.
func (b *Botanist) DeploySeedMonitoring(ctx context.Context) error {
	if !b.IsShootMonitoringEnabled() {
		return b.DeleteSeedMonitoring(ctx)
	}

	credentialsSecret, found := b.SecretsManager.Get(secretNameIngressOperators)
	if !found {
		return fmt.Errorf("secret %q not found", secretNameIngressOperators)
	}

	credentialsUsersSecret, found := b.SecretsManager.Get(secretNameIngressUsers)
	if !found {
		return fmt.Errorf("secret %q not found", secretNameIngressUsers)
	}

	var (
		alertingRules = strings.Builder{}
		scrapeConfigs = strings.Builder{}
	)

	// Fetch component-specific monitoring configuration
	monitoringComponents := []component.MonitoringComponent{
		b.Shoot.Components.ControlPlane.EtcdMain,
		b.Shoot.Components.ControlPlane.EtcdEvents,
		b.Shoot.Components.ControlPlane.KubeAPIServer,
		b.Shoot.Components.ControlPlane.KubeScheduler,
		b.Shoot.Components.ControlPlane.KubeControllerManager,
		b.Shoot.Components.ControlPlane.KubeStateMetrics,
		b.Shoot.Components.ControlPlane.ResourceManager,
		b.Shoot.Components.SystemComponents.CoreDNS,
		b.Shoot.Components.SystemComponents.KubeProxy,
		b.Shoot.Components.SystemComponents.VPNShoot,
	}

	if b.Shoot.NodeLocalDNSEnabled {
		monitoringComponents = append(monitoringComponents, b.Shoot.Components.SystemComponents.NodeLocalDNS)
	}

	if b.Shoot.ReversedVPNEnabled {
		monitoringComponents = append(monitoringComponents, b.Shoot.Components.ControlPlane.VPNSeedServer)
	}

	if b.Shoot.WantsClusterAutoscaler {
		monitoringComponents = append(monitoringComponents, b.Shoot.Components.ControlPlane.ClusterAutoscaler)
	}

	if gardenletfeatures.FeatureGate.Enabled(features.HVPA) {
		monitoringComponents = append(monitoringComponents, b.Shoot.Components.HVPA)
	}

	for _, component := range monitoringComponents {
		componentsScrapeConfigs, err := component.ScrapeConfigs()
		if err != nil {
			return err
		}
		for _, config := range componentsScrapeConfigs {
			scrapeConfigs.WriteString(fmt.Sprintf("- %s\n", utils.Indent(config, 2)))
		}

		componentsAlertingRules, err := component.AlertingRules()
		if err != nil {
			return err
		}
		for filename, rule := range componentsAlertingRules {
			alertingRules.WriteString(fmt.Sprintf("%s: |\n  %s\n", filename, utils.Indent(rule, 2)))
		}
	}

	// Fetch extensions provider-specific monitoring configuration
	existingConfigMaps := &corev1.ConfigMapList{}
	if err := b.SeedClientSet.Client().List(ctx, existingConfigMaps,
		client.InNamespace(b.Shoot.SeedNamespace),
		client.MatchingLabels{v1beta1constants.LabelExtensionConfiguration: v1beta1constants.LabelMonitoring}); err != nil {
		return err
	}

	// Need stable order before passing the dashboards to Grafana config to avoid unnecessary changes
	kutil.ByName().Sort(existingConfigMaps)

	// Read extension monitoring configurations
	for _, cm := range existingConfigMaps.Items {
		alertingRules.WriteString(fmt.Sprintln(cm.Data[v1beta1constants.PrometheusConfigMapAlertingRules]))
		scrapeConfigs.WriteString(fmt.Sprintln(cm.Data[v1beta1constants.PrometheusConfigMapScrapeConfig]))
	}

	// Create shoot token secret for prometheus component
	if err := gutil.NewShootAccessSecret(v1beta1constants.StatefulSetNamePrometheus, b.Shoot.SeedNamespace).Reconcile(ctx, b.SeedClientSet.Client()); err != nil {
		return err
	}

	alerting, err := b.getCustomAlertingConfigs(ctx, b.GetSecretKeysOfRole(v1beta1constants.GardenRoleAlerting))
	if err != nil {
		return err
	}

	var prometheusIngressTLSSecretName string
	if b.ControlPlaneWildcardCert != nil {
		prometheusIngressTLSSecretName = b.ControlPlaneWildcardCert.GetName()
	} else {
		ingressTLSSecret, err := b.SecretsManager.Generate(ctx, &secrets.CertificateSecretConfig{
			Name:                        "prometheus-tls",
			CommonName:                  "prometheus",
			Organization:                []string{"gardener.cloud:monitoring:ingress"},
			DNSNames:                    b.ComputePrometheusHosts(),
			CertType:                    secrets.ServerCert,
			Validity:                    &ingressTLSCertificateValidity,
			SkipPublishingCACertificate: true,
		}, secretsmanager.SignedByCA(v1beta1constants.SecretNameCACluster))
		if err != nil {
			return err
		}
		prometheusIngressTLSSecretName = ingressTLSSecret.Name
	}

	ingressClass, err := gutil.ComputeNginxIngressClassForSeed(b.Seed.GetInfo(), b.Seed.GetInfo().Status.KubernetesVersion)
	if err != nil {
		return err
	}

	clusterCASecret, found := b.SecretsManager.Get(v1beta1constants.SecretNameCACluster)
	if !found {
		return fmt.Errorf("secret %q not found", v1beta1constants.SecretNameCACluster)
	}

	etcdCASecret, found := b.SecretsManager.Get(v1beta1constants.SecretNameCAETCD)
	if !found {
		return fmt.Errorf("secret %q not found", v1beta1constants.SecretNameCAETCD)
	}

	etcdClientSecret, found := b.SecretsManager.Get(etcd.SecretNameClient)
	if !found {
		return fmt.Errorf("secret %q not found", etcd.SecretNameClient)
	}

	var (
		networks = map[string]interface{}{
			"pods":     b.Shoot.Networks.Pods.String(),
			"services": b.Shoot.Networks.Services.String(),
		}
		prometheusConfig = map[string]interface{}{
			"secretNameClusterCA":      clusterCASecret.Name,
			"secretNameEtcdCA":         etcdCASecret.Name,
			"secretNameEtcdClientCert": etcdClientSecret.Name,
			"kubernetesVersion":        b.Shoot.GetInfo().Spec.Kubernetes.Version,
			"nodeLocalDNS": map[string]interface{}{
				"enabled": b.Shoot.NodeLocalDNSEnabled,
			},
			"reversedVPN": map[string]interface{}{
				"enabled": b.Shoot.ReversedVPNEnabled,
			},
			"ingress": map[string]interface{}{
				"class":          ingressClass,
				"authSecretName": credentialsSecret.Name,
				"hosts": []map[string]interface{}{
					{
						"hostName":   b.ComputePrometheusHost(),
						"secretName": prometheusIngressTLSSecretName,
					},
				},
			},
			"namespace": map[string]interface{}{
				"uid": b.SeedNamespaceObject.UID,
			},
			"replicas":           b.Shoot.GetReplicas(1),
			"apiserverServiceIP": b.Shoot.Networks.APIServer.String(),
			"seed": map[string]interface{}{
				"apiserver": b.SeedClientSet.RESTConfig().Host,
				"region":    b.Seed.GetInfo().Spec.Provider.Region,
				"provider":  b.Seed.GetInfo().Spec.Provider.Type,
			},
			"rules": map[string]interface{}{
				"optional": map[string]interface{}{
					"alertmanager": map[string]interface{}{
						"enabled": b.Shoot.WantsAlertmanager,
					},
					"loki": map[string]interface{}{
						"enabled": gardenlethelper.IsLoggingEnabled(b.Config),
					},
					"lokiTelegraf": map[string]interface{}{
						"enabled": b.isShootNodeLoggingEnabled(),
					},
				},
			},
			"shoot": map[string]interface{}{
				"apiserver":           fmt.Sprintf("https://%s", gutil.GetAPIServerDomain(b.Shoot.InternalClusterDomain)),
				"apiserverServerName": gutil.GetAPIServerDomain(b.Shoot.InternalClusterDomain),
				"sniEnabled":          b.APIServerSNIEnabled(),
				"provider":            b.Shoot.GetInfo().Spec.Provider.Type,
				"name":                b.Shoot.GetInfo().Name,
				"project":             b.Garden.Project.Name,
			},
			"ignoreAlerts":            b.Shoot.IgnoreAlerts,
			"alerting":                alerting,
			"additionalRules":         alertingRules.String(),
			"additionalScrapeConfigs": scrapeConfigs.String(),
		}
	)

	if v := b.Shoot.GetInfo().Spec.Networking.Nodes; v != nil {
		networks["nodes"] = *v
	}
	prometheusConfig["networks"] = networks

	// Add remotewrite to prometheus when enabled
	if b.Config.Monitoring != nil &&
		b.Config.Monitoring.Shoot != nil &&
		b.Config.Monitoring.Shoot.RemoteWrite != nil &&
		b.Config.Monitoring.Shoot.RemoteWrite.URL != "" {
		// if remoteWrite Url is set add config into values
		remoteWriteConfig := map[string]interface{}{
			"url": b.Config.Monitoring.Shoot.RemoteWrite.URL,
		}
		// get secret for basic_auth in remote write
		remoteWriteBasicAuth := b.LoadSecret(v1beta1constants.GardenRoleGlobalShootRemoteWriteMonitoring)
		if remoteWriteBasicAuth != nil {
			remoteWriteUsername := string(remoteWriteBasicAuth.Data["username"])
			remoteWritePassword := string(remoteWriteBasicAuth.Data["password"])
			if remoteWriteUsername != "" &&
				remoteWritePassword != "" {
				remoteWriteConfig["basic_auth"] = map[string]interface{}{
					"username": remoteWriteUsername,
					"password": remoteWritePassword,
				}
			}
		}
		// add list with keep metrics if set
		if len(b.Config.Monitoring.Shoot.RemoteWrite.Keep) != 0 {
			remoteWriteConfig["keep"] = b.Config.Monitoring.Shoot.RemoteWrite.Keep
		}
		// add queue_config if set
		if b.Config.Monitoring.Shoot.RemoteWrite.QueueConfig != nil &&
			len(*b.Config.Monitoring.Shoot.RemoteWrite.QueueConfig) != 0 {
			remoteWriteConfig["queue_config"] = b.Config.Monitoring.Shoot.RemoteWrite.QueueConfig
		}
		prometheusConfig["remoteWrite"] = remoteWriteConfig
	}

	// set externalLabels
	if b.Config.Monitoring != nil &&
		b.Config.Monitoring.Shoot != nil &&
		len(b.Config.Monitoring.Shoot.ExternalLabels) != 0 {
		prometheusConfig["externalLabels"] = b.Config.Monitoring.Shoot.ExternalLabels
	}

	prometheus, err := b.InjectSeedShootImages(prometheusConfig, images.ImageNamePrometheus, images.ImageNameConfigmapReloader, images.ImageNameBlackboxExporter)
	if err != nil {
		return err
	}

	coreValues := map[string]interface{}{
		"global": map[string]interface{}{
			"shootKubeVersion": map[string]interface{}{
				"gitVersion": b.Shoot.GetInfo().Spec.Kubernetes.Version,
			},
		},
		"prometheus": prometheus,
	}

	if err := b.SeedClientSet.ChartApplier().Apply(ctx, filepath.Join(ChartsPath, "seed-monitoring", "charts", "core"), b.Shoot.SeedNamespace, fmt.Sprintf("%s-monitoring", b.Shoot.SeedNamespace), kubernetes.Values(coreValues)); err != nil {
		return err
	}

	// Check if we want to deploy an alertmanager into the shoot namespace.
	if b.Shoot.WantsAlertmanager {
		var (
			alertingSMTPKeys = b.GetSecretKeysOfRole(v1beta1constants.GardenRoleAlerting)
			emailConfigs     = []map[string]interface{}{}
		)

		if b.Shoot.GetInfo().Spec.Monitoring != nil && b.Shoot.GetInfo().Spec.Monitoring.Alerting != nil {
			for _, email := range b.Shoot.GetInfo().Spec.Monitoring.Alerting.EmailReceivers {
				for _, key := range alertingSMTPKeys {
					secret := b.LoadSecret(key)

					if string(secret.Data["auth_type"]) != "smtp" {
						continue
					}
					emailConfigs = append(emailConfigs, map[string]interface{}{
						"to":            email,
						"from":          string(secret.Data["from"]),
						"smarthost":     string(secret.Data["smarthost"]),
						"auth_username": string(secret.Data["auth_username"]),
						"auth_identity": string(secret.Data["auth_identity"]),
						"auth_password": string(secret.Data["auth_password"]),
					})
				}
			}
		}

		var alertManagerIngressTLSSecretName string
		if b.ControlPlaneWildcardCert != nil {
			alertManagerIngressTLSSecretName = b.ControlPlaneWildcardCert.GetName()
		} else {
			ingressTLSSecret, err := b.SecretsManager.Generate(ctx, &secrets.CertificateSecretConfig{
				Name:                        "alertmanager-tls",
				CommonName:                  "alertmanager",
				Organization:                []string{"gardener.cloud:monitoring:ingress"},
				DNSNames:                    b.ComputeAlertManagerHosts(),
				CertType:                    secrets.ServerCert,
				Validity:                    &ingressTLSCertificateValidity,
				SkipPublishingCACertificate: true,
			}, secretsmanager.SignedByCA(v1beta1constants.SecretNameCACluster))
			if err != nil {
				return err
			}
			alertManagerIngressTLSSecretName = ingressTLSSecret.Name
		}

		alertManagerValues, err := b.InjectSeedShootImages(map[string]interface{}{
			"ingress": map[string]interface{}{
				"class":          ingressClass,
				"authSecretName": credentialsUsersSecret.Name,
				"hosts": []map[string]interface{}{
					{
						"hostName":   b.ComputeAlertManagerHost(),
						"secretName": alertManagerIngressTLSSecretName,
					},
				},
			},
			"replicas":     b.Shoot.GetReplicas(1),
			"storage":      b.Seed.GetValidVolumeSize("1Gi"),
			"emailConfigs": emailConfigs,
		}, images.ImageNameAlertmanager, images.ImageNameConfigmapReloader)
		if err != nil {
			return err
		}

		return b.SeedClientSet.ChartApplier().Apply(ctx, filepath.Join(ChartsPath, "seed-monitoring", "charts", "alertmanager"), b.Shoot.SeedNamespace, fmt.Sprintf("%s-monitoring", b.Shoot.SeedNamespace), kubernetes.Values(alertManagerValues))
	}

	return common.DeleteAlertmanager(ctx, b.SeedClientSet.Client(), b.Shoot.SeedNamespace)
}

// DeploySeedGrafana deploys the grafana charts to the Seed cluster.
func (b *Botanist) DeploySeedGrafana(ctx context.Context) error {
	// disable monitoring if shoot has purpose testing or monitoring and loki is disabled
	if !b.Operation.WantsGrafana() {
		if err := b.DeleteGrafana(ctx); err != nil {
			return err
		}

		secretName := gutil.ComputeShootProjectSecretName(b.Shoot.GetInfo().Name, gutil.ShootProjectSecretSuffixMonitoring)
		return kutil.DeleteObject(ctx, b.GardenClient, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: b.Shoot.GetInfo().Namespace}})
	}

	credentialsSecret, err := b.SecretsManager.Generate(ctx, observabilityIngressSecretConfig(secretNameIngressOperators),
		secretsmanager.Persist(),
		secretsmanager.Validity(30*24*time.Hour),
	)
	if err != nil {
		return err
	}

	credentialsUsersSecret, err := b.SecretsManager.Generate(ctx, observabilityIngressSecretConfig(secretNameIngressUsers),
		secretsmanager.Persist(),
		secretsmanager.Rotate(secretsmanager.InPlace),
	)
	if err != nil {
		return err
	}

	var (
		operatorsDashboards = strings.Builder{}
		usersDashboards     = strings.Builder{}
	)

	// Fetch extensions provider-specific monitoring configuration
	existingConfigMaps := &corev1.ConfigMapList{}
	if err := b.SeedClientSet.Client().List(ctx, existingConfigMaps,
		client.InNamespace(b.Shoot.SeedNamespace),
		client.MatchingLabels{v1beta1constants.LabelExtensionConfiguration: v1beta1constants.LabelMonitoring}); err != nil {
		return err
	}

	// Need stable order before passing the dashboards to Grafana config to avoid unnecessary changes
	kutil.ByName().Sort(existingConfigMaps)

	// Read extension monitoring configurations
	for _, cm := range existingConfigMaps.Items {
		if operatorsDashboard, ok := cm.Data[v1beta1constants.GrafanaConfigMapOperatorDashboard]; ok && operatorsDashboard != "" {
			operatorsDashboards.WriteString(fmt.Sprintln(operatorsDashboard))
		}
		if usersDashboard, ok := cm.Data[v1beta1constants.GrafanaConfigMapUserDashboard]; ok && usersDashboard != "" {
			usersDashboards.WriteString(fmt.Sprintln(usersDashboard))
		}
	}

	var ingressTLSSecretName string
	if b.ControlPlaneWildcardCert != nil {
		ingressTLSSecretName = b.ControlPlaneWildcardCert.GetName()
	} else {
		ingressTLSSecret, err := b.SecretsManager.Generate(ctx, &secrets.CertificateSecretConfig{
			Name:                        "grafana-tls",
			CommonName:                  "grafana",
			Organization:                []string{"gardener.cloud:monitoring:ingress"},
			DNSNames:                    b.ComputeGrafanaHosts(),
			CertType:                    secrets.ServerCert,
			Validity:                    &ingressTLSCertificateValidity,
			SkipPublishingCACertificate: true,
		}, secretsmanager.SignedByCA(v1beta1constants.SecretNameCACluster))
		if err != nil {
			return err
		}
		ingressTLSSecretName = ingressTLSSecret.Name
	}

	if err := b.deployGrafanaCharts(ctx, credentialsSecret, grafanaOperatorsRole, operatorsDashboards.String(), common.GrafanaOperatorsPrefix, ingressTLSSecretName); err != nil {
		return err
	}

	if err := b.deployGrafanaCharts(ctx, credentialsUsersSecret, grafanaUsersRole, usersDashboards.String(), common.GrafanaUsersPrefix, ingressTLSSecretName); err != nil {
		return err
	}

	return b.syncShootCredentialToGarden(
		ctx,
		gutil.ShootProjectSecretSuffixMonitoring,
		map[string]string{v1beta1constants.GardenRole: v1beta1constants.GardenRoleMonitoring},
		map[string]string{"url": "https://" + b.ComputeGrafanaUsersHost()},
		credentialsUsersSecret.Data,
	)
}

func (b *Botanist) getCustomAlertingConfigs(ctx context.Context, alertingSecretKeys []string) (map[string]interface{}, error) {
	configs := map[string]interface{}{
		"auth_type": map[string]interface{}{},
	}

	for _, key := range alertingSecretKeys {
		secret := b.LoadSecret(key)

		if string(secret.Data["auth_type"]) == "none" {

			if url, ok := secret.Data["url"]; ok {
				configs["auth_type"] = map[string]interface{}{
					"none": map[string]interface{}{
						"url": string(url),
					},
				}
			}
			break
		}

		if string(secret.Data["auth_type"]) == "basic" {
			url, urlOk := secret.Data["url"]
			username, usernameOk := secret.Data["username"]
			password, passwordOk := secret.Data["password"]

			if urlOk && usernameOk && passwordOk {
				configs["auth_type"] = map[string]interface{}{
					"basic": map[string]interface{}{
						"url":      string(url),
						"username": string(username),
						"password": string(password),
					},
				}
			}
			break
		}

		if string(secret.Data["auth_type"]) == "certificate" {
			data := map[string][]byte{}
			url, urlOk := secret.Data["url"]
			ca, caOk := secret.Data["ca.crt"]
			cert, certOk := secret.Data["tls.crt"]
			key, keyOk := secret.Data["tls.key"]
			insecure, insecureOk := secret.Data["insecure_skip_verify"]

			if urlOk && caOk && certOk && keyOk && insecureOk {
				configs["auth_type"] = map[string]interface{}{
					"certificate": map[string]interface{}{
						"url":                  string(url),
						"insecure_skip_verify": string(insecure),
					},
				}
				data["ca.crt"] = ca
				data["tls.crt"] = cert
				data["tls.key"] = key
				amSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prometheus-remote-am-tls",
						Namespace: b.Shoot.SeedNamespace,
					},
				}

				if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, b.SeedClientSet.Client(), amSecret, func() error {
					amSecret.Data = data
					amSecret.Type = corev1.SecretTypeOpaque
					return nil
				}); err != nil {
					return nil, err
				}
			}
			break
		}
	}

	return configs, nil
}

func (b *Botanist) deployGrafanaCharts(ctx context.Context, credentialsSecret *corev1.Secret, role, dashboards, subDomain, ingressTLSSecretName string) error {
	ingressClass, err := gutil.ComputeNginxIngressClassForSeed(b.Seed.GetInfo(), b.Seed.GetInfo().Status.KubernetesVersion)
	if err != nil {
		return err
	}

	values, err := b.InjectSeedShootImages(map[string]interface{}{
		"ingress": map[string]interface{}{
			"class":          ingressClass,
			"authSecretName": credentialsSecret.Name,
			"hosts": []map[string]interface{}{
				{
					"hostName":   b.ComputeIngressHost(subDomain),
					"secretName": ingressTLSSecretName,
				},
			},
		},
		"replicas": b.Shoot.GetReplicas(1),
		"role":     role,
		"extensions": map[string]interface{}{
			"dashboards": dashboards,
		},
		"vpaEnabled": b.Shoot.WantsVerticalPodAutoscaler,
		"sni": map[string]interface{}{
			"enabled": b.APIServerSNIEnabled(),
		},
		"nodeLocalDNS": map[string]interface{}{
			"enabled": b.Shoot.NodeLocalDNSEnabled,
		},
	}, images.ImageNameGrafana)
	if err != nil {
		return err
	}

	return b.SeedClientSet.ChartApplier().Apply(ctx, filepath.Join(ChartsPath, "seed-monitoring", "charts", "grafana"), b.Shoot.SeedNamespace, fmt.Sprintf("%s-monitoring", b.Shoot.SeedNamespace), kubernetes.Values(values))
}

// DeleteGrafana will delete all grafana instances from the seed cluster.
func (b *Botanist) DeleteGrafana(ctx context.Context) error {
	if err := common.DeleteGrafanaByRole(ctx, b.SeedClientSet, b.Shoot.SeedNamespace, grafanaOperatorsRole); err != nil {
		return err
	}

	return common.DeleteGrafanaByRole(ctx, b.SeedClientSet, b.Shoot.SeedNamespace, grafanaUsersRole)
}

// DeleteSeedMonitoring will delete the monitoring stack from the Seed cluster to avoid phantom alerts
// during the deletion process. More precisely, the Alertmanager and Prometheus StatefulSets will be
// deleted.
func (b *Botanist) DeleteSeedMonitoring(ctx context.Context) error {
	if err := common.DeleteAlertmanager(ctx, b.SeedClientSet.Client(), b.Shoot.SeedNamespace); err != nil {
		return err
	}

	objects := []client.Object{
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "allow-from-prometheus",
			},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "allow-prometheus",
			},
		},
		gutil.NewShootAccessSecret(v1beta1constants.StatefulSetNamePrometheus, b.Shoot.SeedNamespace).Secret,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-config",
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-rules",
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "blackbox-exporter-config-prometheus",
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-basic-auth",
			},
		},
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus",
			},
		},
		&vpaautoscalingv1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-vpa",
			},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus",
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-web",
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus",
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-" + b.Shoot.SeedNamespace,
			},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.Shoot.SeedNamespace,
				Name:      "prometheus-db-prometheus-0",
			},
		},
	}

	return kutil.DeleteObjects(ctx, b.SeedClientSet.Client(), objects...)
}
