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
	"net"
	"time"

	"github.com/gardener/gardener/pkg/operation/common"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// WaitUntilNginxIngressServiceIsReady waits until the external load balancer of the nginx ingress controller has been created.
func (b *Botanist) WaitUntilNginxIngressServiceIsReady(ctx context.Context) error {
	const timeout = 10 * time.Minute

	loadBalancerIngress, err := kutil.WaitUntilLoadBalancerIsReady(ctx, b.Logger, b.ShootClientSet.Client(), metav1.NamespaceSystem, "addons-nginx-ingress-controller", timeout)
	if err != nil {
		return err
	}

	b.SetNginxIngressAddress(loadBalancerIngress, b.SeedClientSet.Client())
	return nil
}

// WaitUntilVpnShootServiceIsReady waits until the external load balancer of the VPN has been created.
func (b *Botanist) WaitUntilVpnShootServiceIsReady(ctx context.Context) error {
	const timeout = 10 * time.Minute

	_, err := kutil.WaitUntilLoadBalancerIsReady(ctx, b.Logger, b.ShootClientSet.Client(), metav1.NamespaceSystem, "vpn-shoot", timeout)
	return err
}

// WaitUntilTunnelConnectionExists waits until a port forward connection to the tunnel pod (vpn-shoot) in the kube-system
// namespace of the Shoot cluster can be established.
func (b *Botanist) WaitUntilTunnelConnectionExists(ctx context.Context) error {
	const timeout = 15 * time.Minute

	if err := retry.UntilTimeout(ctx, 5*time.Second, timeout, func(ctx context.Context) (bool, error) {
		return CheckTunnelConnection(ctx, b.Logger, b.ShootClientSet, common.VPNTunnel)
	}); err != nil {
		// If the classic VPN solution is used for the shoot cluster then let's try to fetch
		// the last events of the vpn-shoot service (potentially indicating an error with the load balancer service).
		if !b.Shoot.ReversedVPNEnabled {
			b.Logger.Error(err, "Error occurred while checking the tunnel connection")

			service := &corev1.Service{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Service",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vpn-shoot",
					Namespace: metav1.NamespaceSystem,
				},
			}

			eventsErrorMessage, err2 := kutil.FetchEventMessages(ctx, b.ShootClientSet.Client().Scheme(), b.ShootClientSet.Client(), service, corev1.EventTypeWarning, 2)
			if err2 != nil {
				return fmt.Errorf("'%v' occurred but could not fetch events for more information: %w", err, err2)
			}

			if eventsErrorMessage != "" {
				return fmt.Errorf("%s\n\n%s", err.Error(), eventsErrorMessage)
			}

			return err
		}

		return err
	}

	return nil
}

// WaitUntilNodesDeleted waits until no nodes exist in the shoot cluster anymore.
func (b *Botanist) WaitUntilNodesDeleted(ctx context.Context) error {
	return retry.Until(ctx, 5*time.Second, func(ctx context.Context) (done bool, err error) {
		nodesList := &corev1.NodeList{}
		if err := b.ShootClientSet.Client().List(ctx, nodesList); err != nil {
			return retry.SevereError(err)
		}

		if len(nodesList.Items) == 0 {
			return retry.Ok()
		}

		b.Logger.Info("Waiting until all nodes have been deleted in the shoot cluster", "numberOfNodes", len(nodesList.Items))
		return retry.MinorError(fmt.Errorf("not all nodes have been deleted in the shoot cluster"))
	})
}

// WaitUntilNoPodRunning waits until there is no running Pod in the shoot cluster.
func (b *Botanist) WaitUntilNoPodRunning(ctx context.Context) error {
	b.Logger.Info("Waiting until there are no running Pods in the shoot cluster")

	return retry.Until(ctx, 5*time.Second, func(ctx context.Context) (done bool, err error) {
		podList := &corev1.PodList{}
		if err := b.ShootClientSet.Client().List(ctx, podList); err != nil {
			return retry.SevereError(err)
		}

		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodRunning {
				b.Logger.Info("Waiting until there are no running pods in the shoot cluster (at least one pod still exists)", "pod", client.ObjectKeyFromObject(&pod))
				return retry.MinorError(fmt.Errorf("waiting until there are no running Pods in the shoot cluster... "+
					"there is still at least one running Pod in the shoot cluster: %q", client.ObjectKeyFromObject(&pod).String()))
			}
		}

		return retry.Ok()
	})
}

// WaitUntilEndpointsDoNotContainPodIPs waits until all endpoints in the shoot cluster to not contain any IPs from the Shoot's PodCIDR.
func (b *Botanist) WaitUntilEndpointsDoNotContainPodIPs(ctx context.Context) error {
	b.Logger.Info("Waiting until there are no Endpoints containing Pod IPs in the shoot cluster")

	var podsNetwork *net.IPNet
	if val := b.Shoot.GetInfo().Spec.Networking.Pods; val != nil {
		var err error
		_, podsNetwork, err = net.ParseCIDR(*val)
		if err != nil {
			return fmt.Errorf("unable to check if there are still Endpoints containing Pod IPs in the shoot cluster. Shoots's Pods network could not be parsed: %+v", err)
		}
	} else {
		return fmt.Errorf("unable to check if there are still Endpoints containing Pod IPs in the shoot cluster. Shoot's Pods network is empty")
	}

	return retry.Until(ctx, 5*time.Second, func(ctx context.Context) (done bool, err error) {
		endpointsList := &corev1.EndpointsList{}
		if err := b.ShootClientSet.Client().List(ctx, endpointsList); err != nil {
			return retry.SevereError(err)
		}

		serviceList := &corev1.ServiceList{}
		if err := b.ShootClientSet.Client().List(ctx, serviceList); err != nil {
			return retry.SevereError(err)
		}

		epsNotReconciledByKCM := sets.NewString()
		for _, service := range serviceList.Items {
			// if service.Spec.Selector is empty or nil, kube-controller-manager will not reconcile Endpoints for this Service
			if len(service.Spec.Selector) == 0 {
				epsNotReconciledByKCM.Insert(fmt.Sprintf("%s/%s", service.Namespace, service.Name))
			}
		}

		for _, endpoint := range endpointsList.Items {
			if epsNotReconciledByKCM.Has(fmt.Sprintf("%s/%s", endpoint.Namespace, endpoint.Name)) {
				continue
			}

			for _, subset := range endpoint.Subsets {
				for _, address := range subset.Addresses {
					if podsNetwork.Contains(net.ParseIP(address.IP)) {
						b.Logger.Info("Waiting until there are no endpoints containing pod IPs in the shoot cluster (at least one endpoint still exists)", "endpoint", client.ObjectKeyFromObject(&endpoint))
						return retry.MinorError(fmt.Errorf("waiting until there are no running Pods in the shoot cluster... "+
							"there is still at least one Endpoint containing pod IPs in the shoot cluster: %q", client.ObjectKeyFromObject(&endpoint).String()))
					}
				}
			}
		}

		return retry.Ok()
	})
}

// WaitUntilRequiredExtensionsReady waits until all the extensions required for a shoot reconciliation are ready
func (b *Botanist) WaitUntilRequiredExtensionsReady(ctx context.Context) error {
	return retry.UntilTimeout(ctx, 5*time.Second, time.Minute, func(ctx context.Context) (done bool, err error) {
		if err := b.RequiredExtensionsReady(ctx); err != nil {
			b.Logger.Error(err, "Waiting until all the required extension controllers are ready")
			return retry.MinorError(err)
		}
		return retry.Ok()
	})
}
