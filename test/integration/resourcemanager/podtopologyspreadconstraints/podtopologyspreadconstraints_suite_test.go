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

package podtopologyspreadconstraints_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener/pkg/logger"
	"github.com/gardener/gardener/pkg/operation/botanist/component/resourcemanager"
	resourcemanagerclient "github.com/gardener/gardener/pkg/resourcemanager/client"
	"github.com/gardener/gardener/pkg/resourcemanager/webhook/podtopologyspreadconstraints"
	"github.com/gardener/gardener/pkg/utils"
	. "github.com/gardener/gardener/pkg/utils/test/matchers"
)

func TestPodTopologySpreadConstraints(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "PodTopologySpreadConstraints Integration Test Suite")
}

const testName = "podtopologyspreadconstraints-webhook-test"

var (
	ctx = context.Background()
	log logr.Logger

	restConfig *rest.Config
	testEnv    *envtest.Environment
	testClient client.Client

	testNamespace *corev1.Namespace
)

var _ = BeforeSuite(func() {
	// determine a unique testID
	testID := testName + "-" + utils.ComputeSHA256Hex([]byte(uuid.NewUUID()))[:8]

	logf.SetLogger(logger.MustNewZapLogger(logger.DebugLevel, logger.FormatJSON, zap.WriteTo(GinkgoWriter)))
	log = logf.Log.WithName(testID)

	By("starting test environment")
	testEnv = &envtest.Environment{
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			MutatingWebhooks: getMutatingWebhookConfigurations(testID),
		},
	}

	var err error
	restConfig, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(restConfig).NotTo(BeNil())

	DeferCleanup(func() {
		By("stopping test environment")
		Expect(testEnv.Stop()).To(Succeed())
	})

	By("creating test client")
	testClient, err = client.New(restConfig, client.Options{Scheme: resourcemanagerclient.CombinedScheme})
	Expect(err).NotTo(HaveOccurred())

	By("creating test namespace")
	testNamespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			// create dedicated namespace for each test run, so that we can run multiple tests concurrently for stress tests
			Name: testID,
		},
	}
	Expect(testClient.Create(ctx, testNamespace)).To(Succeed())
	log.Info("Created Namespace for test", "namespaceName", testNamespace.Name)

	DeferCleanup(func() {
		By("deleting test namespace")
		Expect(testClient.Delete(ctx, testNamespace)).To(Or(Succeed(), BeNotFoundError()))
	})

	By("setting up manager")
	mgr, err := manager.New(restConfig, manager.Options{
		Port:               testEnv.WebhookInstallOptions.LocalServingPort,
		Host:               testEnv.WebhookInstallOptions.LocalServingHost,
		CertDir:            testEnv.WebhookInstallOptions.LocalServingCertDir,
		MetricsBindAddress: "0",
		Namespace:          testNamespace.Name,
	})
	Expect(err).NotTo(HaveOccurred())

	By("registering webhook")
	Expect((&podtopologyspreadconstraints.Handler{
		Logger: log,
	}).AddToManager(mgr)).To(Succeed())

	By("starting manager")
	mgrContext, mgrCancel := context.WithCancel(ctx)

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(mgrContext)).To(Succeed())
	}()

	// Wait for the webhook server to start
	Eventually(func() error {
		checker := mgr.GetWebhookServer().StartedChecker()
		return checker(&http.Request{})
	}).Should(BeNil())

	DeferCleanup(func() {
		By("stopping manager")
		mgrCancel()
	})
})

func getMutatingWebhookConfigurations(testID string) []*admissionregistrationv1.MutatingWebhookConfiguration {
	webhookConfig := []*admissionregistrationv1.MutatingWebhookConfiguration{
		{
			TypeMeta: metav1.TypeMeta{
				APIVersion: admissionregistrationv1.SchemeGroupVersion.String(),
				Kind:       "MutatingWebhookConfiguration",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("gardener-resource-manager-%s", testID),
			},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				resourcemanager.GetPodTopologySpreadConstraintsMutatingWebhook(
					&metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: testID}}, nil, func(_ *corev1.Secret, path string) admissionregistrationv1.WebhookClientConfig {
						return admissionregistrationv1.WebhookClientConfig{
							Service: &admissionregistrationv1.ServiceReference{
								Path: &path,
							},
						}
					}),
			},
		},
	}

	return webhookConfig
}
