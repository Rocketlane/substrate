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

package config

import (
	"errors"
	"strings"
	"testing"
)

func loadEKS(t *testing.T, extra map[string]string) (*Config, error) {
	t.Helper()
	loadEnv(t)
	t.Setenv("ATE_INSTALL_EKS", "true")
	t.Setenv("ATE_EKS_REGION", "us-east-1")
	t.Setenv("BUCKET_NAME", "snapshots-bucket")
	for k, v := range extra {
		t.Setenv(k, v)
	}
	return Load(Options{})
}

// The EKS profile derives S3 storage, the encrypted gp3 class and both
// architectures, and records them as values the install uses.
func TestEKSProfileDefaults(t *testing.T) {
	cfg, err := loadEKS(t, nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.EKS || cfg.Kind {
		t.Fatalf("EKS = %v, Kind = %v; want EKS only", cfg.EKS, cfg.Kind)
	}
	if cfg.EKSRegion != "us-east-1" {
		t.Errorf("EKSRegion = %q, want us-east-1", cfg.EKSRegion)
	}
	r := cfg.Resolved()
	for key, want := range map[string]string{
		"storage.snapshotLocation":     "s3://snapshots-bucket/",
		"ateapi.postgres.storageClass": eksPostgresStorageClass,
		"ko.defaultPlatforms":          "linux/amd64,linux/arm64",
	} {
		if got := r.String(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
		if v, _ := r.Value(key); v.From != OriginEKSProfile {
			t.Errorf("%s came from %v, want the --eks profile", key, v.From)
		}
	}
	if err := cfg.RequireTransferServiceAccount(); err != nil {
		t.Errorf("RequireTransferServiceAccount() = %v; S3 needs no transfer service account", err)
	}
}

// A GCP project from the developer environment is cleared, as for Kind.
func TestEKSProfileClearsGCPCoordinates(t *testing.T) {
	cfg, err := loadEKS(t, map[string]string{"PROJECT_ID": "some-project", "CLUSTER_LOCATION": "us-central1"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ProjectID != "" || cfg.ClusterLocation != "" {
		t.Errorf("ProjectID = %q, ClusterLocation = %q; want both cleared by the --eks profile", cfg.ProjectID, cfg.ClusterLocation)
	}
}

// A snapshot location from the ambient environment (a developer env that
// names a gs:// bucket) is replaced, as the Kind profile does; only a flag or
// configuration file overrides the profile.
func TestEKSProfileReplacesAmbientSnapshotLocation(t *testing.T) {
	cfg, err := loadEKS(t, map[string]string{"SNAPSHOT_LOCATION": "gs://dev-bucket/"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SnapshotLocation != "s3://snapshots-bucket/" {
		t.Errorf("SnapshotLocation = %q, want s3://snapshots-bucket/", cfg.SnapshotLocation)
	}
}

func TestEKSProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		check func(error) bool
	}{
		{"kind and eks", map[string]string{"ATE_INSTALL_KIND": "true"}, func(err error) bool {
			var c *ConflictError
			return errors.As(err, &c)
		}},
		{"region required", map[string]string{"ATE_EKS_REGION": ""}, func(err error) bool {
			var r *RequiredError
			return errors.As(err, &r) && strings.Contains(err.Error(), "eks")
		}},
		{"bucket required", map[string]string{"BUCKET_NAME": ""}, func(err error) bool {
			var r *RequiredError
			return errors.As(err, &r)
		}},
		{"agentgateway rejected", map[string]string{"ATE_ATENET_DATAPLANE": RouterAgentgateway}, func(err error) bool {
			var c *ConflictError
			return errors.As(err, &c) && strings.Contains(err.Error(), "envoy")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadEKS(t, tc.env)
			if err == nil || !tc.check(err) {
				t.Errorf("Load() error = %v, want the %s failure", err, tc.name)
			}
		})
	}
}
