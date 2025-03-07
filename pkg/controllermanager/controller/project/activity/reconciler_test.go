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

package activity

import (
	"context"
	"errors"
	"reflect"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	mockclient "github.com/gardener/gardener/pkg/mock/controller-runtime/client"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	testclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Project Activity", func() {
	var (
		projectName   string
		namespaceName string

		project *gardencorev1beta1.Project

		reconciler reconcile.Reconciler
		request    reconcile.Request

		fakeClock *testclock.FakeClock

		ctrl                   *gomock.Controller
		k8sGardenRuntimeClient *mockclient.MockClient
		ctx                    context.Context
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		k8sGardenRuntimeClient = mockclient.NewMockClient(ctrl)
		ctx = context.TODO()

		projectName = "name"
		namespaceName = "namespace"

		project = &gardencorev1beta1.Project{
			ObjectMeta: metav1.ObjectMeta{
				Name: projectName,
			},
			Status: gardencorev1beta1.ProjectStatus{
				LastActivityTimestamp: &metav1.Time{Time: time.Date(1, 1, 1, 1, 1, 1, 1, time.UTC)},
			},
			Spec: gardencorev1beta1.ProjectSpec{
				Namespace: &namespaceName,
			},
		}
	})

	AfterEach(func() {
		ctrl.Finish()
	})

	Describe("Reconciler", func() {
		BeforeEach(func() {
			fakeClock = testclock.NewFakeClock(time.Now())
			reconciler = &Reconciler{
				Client: k8sGardenRuntimeClient,
				Clock:  fakeClock,
			}

			k8sGardenRuntimeClient.EXPECT().Get(
				ctx,
				gomock.Any(),
				gomock.AssignableToTypeOf(&gardencorev1beta1.Project{}),
			).DoAndReturn(func(_ context.Context, namespacedName client.ObjectKey, obj *gardencorev1beta1.Project, _ ...client.GetOption) error {
				if reflect.DeepEqual(namespacedName.Namespace, namespaceName) {
					project.DeepCopyInto(obj)
					return nil
				}
				return errors.New("error retrieving object from store")
			})

			k8sGardenRuntimeClient.EXPECT().Patch(ctx, gomock.AssignableToTypeOf(&gardencorev1beta1.Project{}), gomock.Any()).DoAndReturn(
				func(_ context.Context, prj *gardencorev1beta1.Project, _ client.Patch, _ ...client.PatchOption) error {
					*project = *prj
					return nil
				},
			).AnyTimes()
			k8sGardenRuntimeClient.EXPECT().Status().Return(k8sGardenRuntimeClient).AnyTimes()
		})

		Context("#Reconcile", func() {
			It("should update the lastActivityTimestamp to now", func() {
				request = reconcile.Request{NamespacedName: types.NamespacedName{Name: project.Name, Namespace: namespaceName}}
				_, err := reconciler.Reconcile(ctx, request)
				Expect(err).ToNot(HaveOccurred())

				now := &metav1.Time{Time: fakeClock.Now()}
				Expect(project.Status.LastActivityTimestamp).To(Equal(now))
			})

			It("should fail reconcile because the project can't be retrieved", func() {
				request = reconcile.Request{NamespacedName: types.NamespacedName{Name: project.Name, Namespace: namespaceName + "other"}}
				_, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(HaveOccurred())
			})
		})
	})
})
