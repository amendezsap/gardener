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

package seccompprofile_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("SeccompProfile tests", func() {
	var pod *corev1.Pod

	BeforeEach(func() {
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    testNamespace.Name,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "foo-container",
						Image: "foo",
					},
				},
			},
		}
	})

	AfterEach(func() {
		Expect(testClient.Delete(ctx, pod)).To(Succeed())
	})

	It("should not mutate the pod when the pod is explicitly specifying a seccomp profile", func() {
		profileType := corev1.SeccompProfileTypeUnconfined
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type: profileType,
			},
		}
		Expect(testClient.Create(ctx, pod)).To(Succeed())

		Expect(testClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.Spec.SecurityContext.SeccompProfile.Type).To(Equal(profileType))
	})

	It("should mutate the pod and assign default seccomp profile when seccomp profile is not specified", func() {
		Expect(testClient.Create(ctx, pod)).To(Succeed())

		Expect(testClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileType("RuntimeDefault")))
	})

	It("should not overwrite any values in security context during pod mutation", func() {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot:       pointer.Bool(false),
			RunAsUser:          pointer.Int64(3),
			SupplementalGroups: []int64{4, 5, 6},
		}
		Expect(testClient.Create(ctx, pod)).To(Succeed())

		Expect(testClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		Expect(pod.Spec.SecurityContext).To(Equal(&corev1.PodSecurityContext{
			RunAsNonRoot:       pointer.Bool(false),
			RunAsUser:          pointer.Int64(3),
			SupplementalGroups: []int64{4, 5, 6},
			SeccompProfile: &corev1.SeccompProfile{
				Type: "RuntimeDefault",
			},
		}))
	})
})
