// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package steps

import (
	"context"
	"fmt"

	"go.ate.dev/substrate/cmd/ate-setup/internal/log"
)

// eksPrereqsDir holds the cluster objects an EKS install needs before
// PostgreSQL and the control plane bundle: the encrypted gp3 StorageClass and
// the RBAC for ate-local-gateway's Kubernetes group.
const eksPrereqsDir = installDir + "/eks/prereqs"

// eksConfigMap carries the AWS region to the object-store plugins (envFrom).
// Pods get EKS Pod Identity credentials but cannot read the region from
// instance metadata, so it is passed explicitly.
const eksConfigMap = "ate-eks-config"

// eksConfigMapManifest renders the region ConfigMap for namespace.
func eksConfigMapManifest(namespace, region string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  AWS_REGION: %q
`, eksConfigMap, namespace, region))
}

// applyEKSPrerequisites applies the region ConfigMap and eks/prereqs. It is a
// no-op for any other profile.
func (e *Env) applyEKSPrerequisites(ctx context.Context) error {
	if !e.Cfg.EKS {
		return nil
	}
	log.Step("apply_eks_prerequisites")
	if err := e.Kube.ApplyBytes(ctx, eksConfigMapManifest(e.Namespace(), e.Cfg.EKSRegion)); err != nil {
		return fmt.Errorf("applying %s: %w", eksConfigMap, err)
	}
	manifest, err := e.Kustomize(eksPrereqsDir)
	if err != nil {
		return err
	}
	return e.Kube.ApplyBytes(ctx, manifest)
}

// deleteEKSPrerequisites removes what applyEKSPrerequisites created. The
// StorageClass goes last only in effect: PostgreSQL's claim keeps its volume
// until the claim itself is deleted.
func (e *Env) deleteEKSPrerequisites(ctx context.Context) error {
	if !e.Cfg.EKS {
		return nil
	}
	manifest, err := e.Kustomize(eksPrereqsDir)
	if err != nil {
		return err
	}
	if err := e.Kube.DeleteBytes(ctx, manifest); err != nil {
		return err
	}
	return e.Kube.DeleteBytes(ctx, eksConfigMapManifest(e.Namespace(), e.Cfg.EKSRegion))
}
