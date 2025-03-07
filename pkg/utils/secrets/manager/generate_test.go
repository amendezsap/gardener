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

package manager

import (
	"context"
	"strconv"
	"time"

	secretutils "github.com/gardener/gardener/pkg/utils/secrets"
	"github.com/gardener/gardener/pkg/utils/test"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesscheme "k8s.io/client-go/kubernetes/scheme"
	testclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = BeforeSuite(func() {
	DeferCleanup(test.WithVar(&secretutils.GenerateRandomString, secretutils.FakeGenerateRandomString))
	DeferCleanup(test.WithVar(&secretutils.GenerateKey, secretutils.FakeGenerateKey))
})

var _ = Describe("Generate", func() {
	var (
		ctx       = context.TODO()
		namespace = "shoot--foo--bar"
		identity  = "test"

		m          *manager
		fakeClient client.Client
		fakeClock  = testclock.NewFakeClock(time.Time{})
	)

	BeforeEach(func() {
		fakeClient = fakeclient.NewClientBuilder().WithScheme(kubernetesscheme.Scheme).Build()

		mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{})
		Expect(err).NotTo(HaveOccurred())
		m = mgr.(*manager)
	})

	Describe("#Generate", func() {
		name := "config"

		Context("for non-certificate secrets", func() {
			var config *secretutils.BasicAuthSecretConfig

			BeforeEach(func() {
				config = &secretutils.BasicAuthSecretConfig{
					Name:           name,
					Format:         secretutils.BasicAuthFormatNormal,
					Username:       "foo",
					PasswordLength: 3,
				}
			})

			It("should generate a new secret", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(secret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should maintain the lifetime labels (w/o validity)", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					Not(HaveKey("valid-until-time")),
				))
			})

			It("should maintain the lifetime labels (w/ validity)", func() {
				By("generating new secret")
				validity := time.Hour
				secret, err := m.Generate(ctx, config, Validity(validity))
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())

				issuedAtTime := fakeClock.Now()

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(issuedAtTime.Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(issuedAtTime.Add(validity).Unix(), 10)),
				))

				By("fast-forward time")
				fakeClock.Step(30 * time.Minute)

				By("generating the same secret again w/ same validity")
				secret2, err := m.Generate(ctx, config, Validity(validity))
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret2 := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret2), foundSecret2)).To(Succeed())

				By("verifying labels (validity should not have been changed)")
				Expect(foundSecret2.Labels).To(And(
					HaveKeyWithValue("issued-at-time", secret.Labels["issued-at-time"]),
					HaveKeyWithValue("valid-until-time", secret.Labels["valid-until-time"]),
				))

				By("generating the same secret again w/ new validity")
				validity = 2 * time.Hour
				secret3, err := m.Generate(ctx, config, Validity(validity))
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret3 := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret3), foundSecret3)).To(Succeed())

				By("verifying labels (validity should have been recomputed)")
				Expect(foundSecret3.Labels).To(And(
					HaveKeyWithValue("issued-at-time", secret.Labels["issued-at-time"]),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(issuedAtTime.Add(validity).Unix(), 10)),
				))

				By("generating the same secret again w/o validity option this time")
				secret4, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret4 := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret4), foundSecret4)).To(Succeed())

				By("verifying labels (validity should have been recomputed)")
				Expect(foundSecret4.Labels).To(And(
					HaveKeyWithValue("issued-at-time", secret.Labels["issued-at-time"]),
					Not(HaveKey("valid-until-time")),
				))
			})

			It("should generate a new secret when the config changes", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("changing secret config and generate again")
				config.PasswordLength = 4
				newSecret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should generate a new secret when the last rotation initiation time changes", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("changing last rotation initiation time and generate again")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{name: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newSecret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should store the old secret if rotation strategy is KeepOld", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("changing secret config and generate again with KeepOld strategy")
				config.PasswordLength = 4
				newSecret, err := m.Generate(ctx, config, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old.obj).To(Equal(withoutTypeMeta(secret)))
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should not store the old secret even if rotation strategy is KeepOld when old secrets shall be ignored", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("changing secret config and generate again with KeepOld strategy and ignore old secrets option")
				config.PasswordLength = 4
				newSecret, err := m.Generate(ctx, config, Rotate(KeepOld), IgnoreOldSecrets())
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should drop the old secret if rotation strategy is KeepOld after IgnoreOldSecretsAfter has passed", func() {
				By("generating secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("changing secret config and generating again")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				config.PasswordLength = 4
				newSecret, err := m.Generate(ctx, config, Rotate(KeepOld), IgnoreOldSecretsAfter(time.Minute))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store contains both old and new secret")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old.obj).To(Equal(withoutTypeMeta(secret)))
				Expect(secretInfos.bundle).To(BeNil())

				By("generating secret again after given duration")
				fakeClock.Step(time.Minute)
				mgr, err = New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newSecret, err = m.Generate(ctx, config, Rotate(KeepOld), IgnoreOldSecretsAfter(time.Minute))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("verifying internal store contains only new secret")
				secretInfos, found = m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})

			It("should reconcile the secret", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("marking secret as mutable")
				patch := client.MergeFrom(secret.DeepCopy())
				secret.Immutable = nil
				// ensure that label with empty value is added by another call to Generate
				delete(secret.Labels, "last-rotation-initiation-time")
				Expect(fakeClient.Patch(ctx, secret, patch)).To(Succeed())

				By("changing options and generate again")
				secret, err = m.Generate(ctx, config, Persist())
				Expect(err).NotTo(HaveOccurred())

				By("verifying labels got reconciled")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("persist", "true"),
					// ensure that label with empty value is added by another call to Generate
					HaveKeyWithValue("last-rotation-initiation-time", ""),
				))
				Expect(foundSecret.Immutable).To(PointTo(BeTrue()))
			})
		})

		Context("for CA certificate secrets", func() {
			var (
				config     *secretutils.CertificateSecretConfig
				commonName = "my-ca-common-name"
			)

			BeforeEach(func() {
				config = &secretutils.CertificateSecretConfig{
					Name:       name,
					CommonName: commonName,
					CertType:   secretutils.CACert,
				}
			})

			It("should generate a new CA secret and a corresponding bundle", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				Expect(secret.Name).To(Equal(name + "-54620669"))
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("finding created bundle secret")
				secretList := &corev1.SecretList{}
				Expect(fakeClient.List(ctx, secretList, client.InNamespace(namespace), client.MatchingLabels{
					"managed-by":       "secrets-manager",
					"manager-identity": "test",
					"bundle-for":       name,
				})).To(Succeed())
				Expect(secretList.Items).To(HaveLen(1))

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(secret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle.obj).To(Equal(withTypeMeta(&secretList.Items[0])))
			})

			It("should maintain the lifetime labels (w/o custom validity)", func() {
				DeferCleanup(test.WithVar(&secretutils.Clock, fakeClock))

				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(fakeClock.Now().AddDate(10, 0, 0).Unix(), 10)),
				))
			})

			It("should maintain the lifetime labels (w/ custom validity which is ignored for certificates)", func() {
				DeferCleanup(test.WithVar(&secretutils.Clock, fakeClock))

				By("generating new secret")
				secret, err := m.Generate(ctx, config, Validity(time.Hour))
				Expect(err).NotTo(HaveOccurred())

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(fakeClock.Now().AddDate(10, 0, 0).Unix(), 10)),
				))
			})

			It("should generate a new CA secret and use the secret name as common name", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				cert, err := secretutils.LoadCertificate("", secret.Data["ca.key"], secret.Data["ca.crt"])
				Expect(err).NotTo(HaveOccurred())
				Expect(cert.Certificate.Subject.CommonName).To(Equal(secret.Name))
			})

			It("should generate a new CA secret and ignore the config checksum for its name", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config, IgnoreConfigChecksumForCASecretName())
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)
				Expect(secret.Name).To(Equal(name))
			})

			It("should rotate a CA secret and add old and new to the corresponding bundle", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("storing old bundle secret")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				oldBundleSecret := secretInfos.bundle.obj

				By("changing secret config and generate again")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{name: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newSecret, err := m.Generate(ctx, config, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newSecret)

				By("finding created bundle secret")
				secretList := &corev1.SecretList{}
				Expect(fakeClient.List(ctx, secretList, client.InNamespace(namespace), client.MatchingLabels{
					"managed-by":       "secrets-manager",
					"manager-identity": "test",
					"bundle-for":       name,
				})).To(Succeed())
				Expect(secretList.Items).To(HaveLen(2))

				By("verifying internal store reflects changes")
				secretInfos, found = m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(newSecret))
				Expect(secretInfos.old.obj).To(Equal(withoutTypeMeta(secret)))
				Expect(secretInfos.bundle.obj).NotTo(PointTo(Equal(oldBundleSecret)))
			})
		})

		Context("for certificate secrets", func() {
			var (
				caName, serverName, clientName       = "ca", "server", "client"
				caConfig, serverConfig, clientConfig *secretutils.CertificateSecretConfig
			)

			BeforeEach(func() {
				caConfig = &secretutils.CertificateSecretConfig{
					Name:       caName,
					CommonName: caName,
					CertType:   secretutils.CACert,
				}
				serverConfig = &secretutils.CertificateSecretConfig{
					Name:                        serverName,
					CommonName:                  serverName,
					CertType:                    secretutils.ServerCert,
					SkipPublishingCACertificate: true,
				}
				clientConfig = &secretutils.CertificateSecretConfig{
					Name:                        clientName,
					CommonName:                  clientName,
					CertType:                    secretutils.ClientCert,
					SkipPublishingCACertificate: true,
				}
			})

			It("should maintain the lifetime labels (w/o custom validity)", func() {
				DeferCleanup(test.WithVar(&secretutils.Clock, fakeClock))

				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new server secret")
				serverSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(serverSecret), foundSecret)).To(Succeed())

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(fakeClock.Now().AddDate(10, 0, 0).Unix(), 10)),
				))
			})

			It("should maintain the lifetime labels (w/ custom validity which is ignored for certificates)", func() {
				DeferCleanup(test.WithVar(&secretutils.Clock, fakeClock))

				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new server secret")
				serverSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName), Validity(time.Hour))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("reading created secret from system")
				foundSecret := &corev1.Secret{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(serverSecret), foundSecret)).To(Succeed())

				By("verifying labels")
				Expect(foundSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(fakeClock.Now().AddDate(10, 0, 0).Unix(), 10)),
				))
			})

			It("should keep the same server cert even when the CA rotates", func() {
				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new server secret")
				serverSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("rotating CA")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{name: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newCASecret, err := m.Generate(ctx, caConfig, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newCASecret)

				By("get or generate server secret")
				newServerSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newServerSecret)

				By("verifying server secret is still the same")
				Expect(newServerSecret).To(Equal(withTypeMeta(serverSecret)))
			})

			It("should regenerate the server cert when the CA rotates and the 'UseCurrentCA' option is set", func() {
				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new server secret")
				serverSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName, UseCurrentCA))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("rotating CA")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{caName: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newCASecret, err := m.Generate(ctx, caConfig, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newCASecret)

				By("get or generate server secret")
				newServerSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName, UseCurrentCA))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newServerSecret)

				By("verifying server secret is changed")
				Expect(newServerSecret).NotTo(Equal(serverSecret))
			})

			It("should not regenerate the client cert when the CA rotates and the 'UseOldCA' option is set", func() {
				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new client secret")
				clientSecret, err := m.Generate(ctx, clientConfig, SignedByCA(caName, UseOldCA))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, clientSecret)

				By("rotating CA")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{caName: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newCASecret, err := m.Generate(ctx, caConfig, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newCASecret)

				By("get or generate client secret")
				newClientSecret, err := m.Generate(ctx, clientConfig, SignedByCA(caName, UseOldCA))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newClientSecret)

				By("verifying client secret is not changed")
				Expect(newClientSecret).To(Equal(clientSecret))
			})

			It("should regenerate the client cert when the CA rotates", func() {
				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new client secret")
				clientSecret, err := m.Generate(ctx, clientConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, clientSecret)

				By("rotating CA")
				mgr, err := New(ctx, logr.Discard(), fakeClock, fakeClient, namespace, identity, Config{SecretNamesToTimes: map[string]time.Time{caName: time.Now()}})
				Expect(err).NotTo(HaveOccurred())
				m = mgr.(*manager)

				newCASecret, err := m.Generate(ctx, caConfig, Rotate(KeepOld))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newCASecret)

				By("get or generate client secret")
				newClientSecret, err := m.Generate(ctx, clientConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, newClientSecret)

				By("verifying client secret is changed")
				Expect(newClientSecret).NotTo(Equal(clientSecret))
			})

			It("should also accept ControlPlaneSecretConfigs", func() {
				DeferCleanup(test.WithVar(&secretutils.Clock, fakeClock))

				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new control plane secret")
				serverConfig.Validity = pointer.Duration(1337 * time.Minute)
				controlPlaneSecretConfig := &secretutils.ControlPlaneSecretConfig{
					Name:                    "control-plane-secret",
					CertificateSecretConfig: serverConfig,
					KubeConfigRequests: []secretutils.KubeConfigRequest{{
						ClusterName:   namespace,
						APIServerHost: "some-host",
					}},
				}

				serverSecret, err := m.Generate(ctx, controlPlaneSecretConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("verifying labels")
				Expect(serverSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					HaveKeyWithValue("valid-until-time", strconv.FormatInt(fakeClock.Now().Add(*serverConfig.Validity).Unix(), 10)),
				))
			})

			It("should correctly maintain lifetime labels for ControlPlaneSecretConfigs w/o certificate secret configs", func() {
				By("generating new control plane secret")
				cpSecret, err := m.Generate(ctx, &secretutils.ControlPlaneSecretConfig{Name: "control-plane-secret"})
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, cpSecret)

				By("verifying labels")
				Expect(cpSecret.Labels).To(And(
					HaveKeyWithValue("issued-at-time", strconv.FormatInt(fakeClock.Now().Unix(), 10)),
					Not(HaveKey("valid-until-time")),
				))
			})

			It("should generate a new server and client secret and keep the common name", func() {
				By("generating new CA secret")
				caSecret, err := m.Generate(ctx, caConfig)
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, caSecret)

				By("generating new server secret")
				serverSecret, err := m.Generate(ctx, serverConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, serverSecret)

				By("verifying server certificate common name")
				serverCert, err := secretutils.LoadCertificate("", serverSecret.Data["tls.key"], serverSecret.Data["tls.crt"])
				Expect(err).NotTo(HaveOccurred())
				Expect(serverCert.Certificate.Subject.CommonName).To(Equal(serverConfig.CommonName))

				By("generating new client secret")
				clientSecret, err := m.Generate(ctx, clientConfig, SignedByCA(caName))
				Expect(err).NotTo(HaveOccurred())
				expectSecretWasCreated(ctx, fakeClient, clientSecret)

				By("verifying client certificate common name")
				clientCert, err := secretutils.LoadCertificate("", clientSecret.Data["tls.key"], clientSecret.Data["tls.crt"])
				Expect(err).NotTo(HaveOccurred())
				Expect(clientCert.Certificate.Subject.CommonName).To(Equal(clientConfig.CommonName))
			})
		})

		Context("for RSA Private Key secrets", func() {
			var config *secretutils.RSASecretConfig

			BeforeEach(func() {
				config = &secretutils.RSASecretConfig{
					Name: name,
					Bits: 2048,
				}
			})

			It("should generate a new RSA private key secret and a corresponding bundle", func() {
				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				Expect(secret.Name).To(Equal(name + "-16163da7"))
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("finding created bundle secret")
				secretList := &corev1.SecretList{}
				Expect(fakeClient.List(ctx, secretList, client.InNamespace(namespace), client.MatchingLabels{
					"managed-by":       "secrets-manager",
					"manager-identity": "test",
					"bundle-for":       name,
				})).To(Succeed())
				Expect(secretList.Items).To(HaveLen(1))

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(secret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle.obj).To(Equal(withTypeMeta(&secretList.Items[0])))
			})

			It("should generate a new RSA private key secret but no bundle since it's used for SSH", func() {
				config.UsedForSSH = true

				By("generating new secret")
				secret, err := m.Generate(ctx, config)
				Expect(err).NotTo(HaveOccurred())
				Expect(secret.Name).To(Equal(name + "-fc4f9932"))
				expectSecretWasCreated(ctx, fakeClient, secret)

				By("verifying internal store reflects changes")
				secretInfos, found := m.getFromStore(name)
				Expect(found).To(BeTrue())
				Expect(secretInfos.current.obj).To(Equal(secret))
				Expect(secretInfos.old).To(BeNil())
				Expect(secretInfos.bundle).To(BeNil())
			})
		})

		Context("backwards compatibility", func() {
			Context("etcd encryption key", func() {
				var (
					oldKey    = []byte("old-key")
					oldSecret = []byte("old-secret")
					config    *secretutils.ETCDEncryptionKeySecretConfig
				)

				BeforeEach(func() {
					config = &secretutils.ETCDEncryptionKeySecretConfig{
						Name:         "kube-apiserver-etcd-encryption-key",
						SecretLength: 32,
					}
				})

				It("should generate a new encryption key secret if old secret does not exist", func() {
					By("generating secret")
					secret, err := m.Generate(ctx, config)
					Expect(err).NotTo(HaveOccurred())

					By("verifying new key and secret were generated")
					Expect(secret.Data["key"]).NotTo(Equal(oldKey))
					Expect(secret.Data["secret"]).NotTo(Equal(oldSecret))
				})

				It("should keep the existing encryption key and secret if old secret still exists", func() {
					oldEncryptionConfiguration := `apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
- providers:
  - aescbc:
      keys:
      - name: ` + string(oldKey) + `
        secret: ` + string(oldSecret) + `
  - identity: {}
  resources:
  - secrets
`

					By("creating existing secret with old encryption configuration")
					existingSecret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-encryption-secret",
							Namespace: namespace,
						},
						Type: corev1.SecretTypeOpaque,
						Data: map[string][]byte{"encryption-configuration.yaml": []byte(oldEncryptionConfiguration)},
					}
					Expect(fakeClient.Create(ctx, existingSecret)).To(Succeed())

					By("generating secret")
					secret, err := m.Generate(ctx, config)
					Expect(err).NotTo(HaveOccurred())

					By("verifying old key and secret were kept")
					Expect(secret.Data["key"]).To(Equal(oldKey))
					Expect(secret.Data["secret"]).To(Equal(oldSecret))
				})
			})

			Context("service account key", func() {
				var (
					oldData = map[string][]byte{"id_rsa": []byte("some-old-key")}
					config  *secretutils.RSASecretConfig
				)

				BeforeEach(func() {
					config = &secretutils.RSASecretConfig{
						Name: "service-account-key",
						Bits: 4096,
					}
				})

				It("should generate a new key if old secret does not exist", func() {
					By("generating secret")
					secret, err := m.Generate(ctx, config)
					Expect(err).NotTo(HaveOccurred())

					By("verifying new key was generated")
					Expect(secret.Data).NotTo(Equal(oldData))
				})

				It("should keep the existing key if old secret still exists", func() {
					By("creating existing secret with old key")
					existingSecret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "service-account-key",
							Namespace: namespace,
						},
						Type: corev1.SecretTypeOpaque,
						Data: oldData,
					}
					Expect(fakeClient.Create(ctx, existingSecret)).To(Succeed())

					By("generating secret")
					secret, err := m.Generate(ctx, config)
					Expect(err).NotTo(HaveOccurred())

					By("verifying old password was kept")
					Expect(secret.Data).To(Equal(oldData))
				})
			})
		})
	})
})

func expectSecretWasCreated(ctx context.Context, fakeClient client.Client, secret *corev1.Secret) {
	foundSecret := &corev1.Secret{}
	Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(secret), foundSecret)).To(Succeed())

	Expect(foundSecret).To(Equal(withTypeMeta(secret)))
}

func withTypeMeta(obj *corev1.Secret) *corev1.Secret {
	secret := obj.DeepCopy()
	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	return secret
}

func withoutTypeMeta(obj *corev1.Secret) *corev1.Secret {
	secret := obj.DeepCopy()
	secret.TypeMeta = metav1.TypeMeta{}
	return secret
}
