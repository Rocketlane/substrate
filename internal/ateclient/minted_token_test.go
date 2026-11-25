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

package ateclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// tokenClientset returns a fake clientset whose TokenRequests return
// token-1, token-2, ... with the given lifetime, or fail while *failing is set.
func tokenClientset(lifetime time.Duration, failing *bool) (*fake.Clientset, *int) {
	cs := fake.NewClientset()
	minted := 0
	cs.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		if *failing {
			return true, nil, errors.New("apiserver unavailable")
		}
		minted++
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
			Token:               fmt.Sprintf("token-%d", minted),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(lifetime)),
		}}, nil
	})
	return cs, &minted
}

func TestMintedTokenIsReusedUntilHalfItsLifetime(t *testing.T) {
	failing := false
	cs, minted := tokenClientset(time.Hour, &failing)
	c := &mintedTokenCreds{clientset: cs}

	for range 3 {
		md, err := c.GetRequestMetadata(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := md["authorization"]; got != "Bearer token-1" {
			t.Fatalf("authorization = %q, want the first token", got)
		}
	}
	if *minted != 1 {
		t.Fatalf("minted %d tokens, want 1", *minted)
	}

	c.refreshAt = time.Now().Add(-time.Second) // half the lifetime has passed
	if tok, _ := c.current(context.Background()); tok != "token-2" {
		t.Fatalf("token = %q, want a new one after half its lifetime", tok)
	}
}

func TestMintedTokenKeepsAValidTokenWhenRenewalFails(t *testing.T) {
	failing := false
	cs, _ := tokenClientset(time.Hour, &failing)
	c := &mintedTokenCreds{clientset: cs}
	if _, err := c.current(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing = true
	c.refreshAt = time.Now().Add(-time.Second)
	if tok, err := c.current(context.Background()); err != nil || tok != "token-1" {
		t.Fatalf("got %q, %v; want the still-valid token-1", tok, err)
	}
	if !c.refreshAt.After(time.Now()) {
		t.Fatal("a failed renewal should schedule another attempt")
	}

	c.refreshAt = time.Now().Add(-time.Second)
	c.expiresAt = time.Now().Add(-time.Second) // and now it has expired
	if _, err := c.current(context.Background()); err == nil {
		t.Fatal("want an error once the token has expired and renewal fails")
	}
}
