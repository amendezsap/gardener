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

package csrapprover

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	certificatesclientv1 "k8s.io/client-go/kubernetes/typed/certificates/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/resourcemanager/apis/config"
	"github.com/gardener/gardener/pkg/utils"
)

// Reconciler reconciles CertificateSigningRequest objects.
type Reconciler struct {
	SourceClient       client.Client
	TargetClient       client.Client
	CertificatesClient certificatesclientv1.CertificateSigningRequestInterface
	Config             config.KubeletCSRApproverControllerConfig
	SourceNamespace    string
}

// Reconcile performs the main reconciliation logic.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.TargetClient.Get(ctx, request.NamespacedName, csr); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Object is gone, stop reconciling")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("error retrieving object from store: %w", err)
	}

	var (
		isInFinalState bool
		finalState     string
	)

	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved || c.Type == certificatesv1.CertificateDenied {
			isInFinalState = true
			finalState = string(c.Type)
		}
	}

	if len(csr.Status.Certificate) != 0 || isInFinalState {
		log.Info("Ignoring CSR, as it is in final state", "finalState", finalState)
		return reconcile.Result{}, nil
	}

	x509cr, err := utils.DecodeCertificateRequest(csr.Spec.Request)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to parse csr: %w", err)
	}

	reason, allowed, err := r.mustApprove(ctx, csr, x509cr)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed when checking for approval conditions: %w", err)
	}

	if allowed {
		log.Info("Auto-approving CSR", "reason", reason)
		csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Reason:  "RequestApproved",
			Message: fmt.Sprintf("Approving kubelet server certificate CSR (%s)", reason),
		})
	} else {
		log.Info("Denying CSR", "reason", reason)
		csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateDenied,
			Status:  corev1.ConditionTrue,
			Reason:  "RequestDenied",
			Message: fmt.Sprintf("Denying kubelet server certificate CSR (%s)", reason),
		})
	}

	_, err = r.CertificatesClient.UpdateApproval(ctx, csr.Name, csr, kubernetes.DefaultUpdateOptions())
	return reconcile.Result{}, err
}

func (r *Reconciler) mustApprove(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, x509cr *x509.CertificateRequest) (string, bool, error) {
	if prefix := "system:node:"; !strings.HasPrefix(csr.Spec.Username, prefix) {
		return fmt.Sprintf("username %q is not prefixed with %q", csr.Spec.Username, prefix), false, nil
	}

	if len(x509cr.DNSNames)+len(x509cr.IPAddresses) == 0 {
		return "no DNS names or IP addresses in the SANs found", false, nil
	}

	if x509cr.Subject.CommonName != csr.Spec.Username {
		return "common name in CSR does not match username", false, nil
	}

	if len(x509cr.Subject.Organization) != 1 || !utils.ValueExists(user.NodesGroup, x509cr.Subject.Organization) {
		return "organization in CSR does not match nodes group", false, nil
	}

	nodeName := strings.TrimPrefix(x509cr.Subject.CommonName, "system:node:")

	node := &corev1.Node{}
	if err := r.TargetClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("could not find node object with name %q", node.Name), false, nil
		}
		return "", false, err
	}

	machineList := &machinev1alpha1.MachineList{}
	if err := r.SourceClient.List(ctx, machineList, client.InNamespace(r.SourceNamespace), client.MatchingLabels{"node": node.Name}); err != nil {
		return "", false, err
	}

	if length := len(machineList.Items); length != 1 {
		return fmt.Sprintf("Expected exactly one machine in namespace %q for node %q but found %d", r.SourceNamespace, node.Name, length), false, nil
	}

	var (
		hostNames   []string
		ipAddresses []string
	)

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeHostName {
			hostNames = append(hostNames, address.Address)
		}
		if address.Type == corev1.NodeInternalIP {
			ipAddresses = append(ipAddresses, address.Address)
		}
	}

	if !sets.NewString(hostNames...).Equal(sets.NewString(x509cr.DNSNames...)) {
		return "DNS names in CSR do not match Hostname addresses in node object", false, nil
	}

	var ipAddressesInCSR []string
	for _, ip := range x509cr.IPAddresses {
		ipAddressesInCSR = append(ipAddressesInCSR, ip.String())
	}

	if !sets.NewString(ipAddresses...).Equal(sets.NewString(ipAddressesInCSR...)) {
		return "IP addresses in CSR do not match InternalIP addresses in node object", false, nil
	}

	return "all checks passed", true, nil
}
