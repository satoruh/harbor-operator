/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/satoruh/harbor-operator/api/v1alpha1"
	"github.com/satoruh/harbor-operator/internal/harbor"
)

// defaultSyncPeriod is the fallback interval: the connection could not be
// read, or it carries no syncPeriod because it was built in code rather
// than defaulted by the CRD.
const defaultSyncPeriod = 10 * time.Minute

func syncPeriodOf(conn *harborv1alpha1.HarborConnection) time.Duration {
	if conn.Spec.SyncPeriod != nil {
		return conn.Spec.SyncPeriod.Duration
	}
	return defaultSyncPeriod
}

// newHarborClient builds a client from a connection, reading its credentials
// and CA bundle from namespace only.
func newHarborClient(
	ctx context.Context,
	c client.Reader,
	namespace string,
	newClient func(harbor.Options) (harbor.Client, error),
	conn *harborv1alpha1.HarborConnection,
) (harbor.Client, error) {
	username, password, err := credentials(ctx, c, namespace, conn)
	if err != nil {
		return nil, err
	}

	bundle, err := caBundle(ctx, c, namespace, conn)
	if err != nil {
		return nil, err
	}
	if conn.Spec.InsecureSkipVerify && conn.Spec.CABundleRef != nil {
		logf.FromContext(ctx).Info("insecureSkipVerify makes caBundleRef pointless; certificates are not verified",
			"connection", conn.Name)
	}

	hc, err := newClient(harbor.Options{
		BaseURL:            conn.Spec.BaseURL,
		Username:           username,
		Password:           password,
		CABundle:           bundle,
		InsecureSkipVerify: conn.Spec.InsecureSkipVerify,
	})
	if err != nil {
		return nil, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "%v", err)
	}
	return hc, nil
}

func credentials(ctx context.Context, c client.Reader, namespace string, conn *harborv1alpha1.HarborConnection) (string, string, error) {
	ref := conn.Spec.CredentialsRef
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}

	var secret corev1.Secret
	if err := c.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s not found", key)
		}
		return "", "", failed(harborv1alpha1.ReasonInternalError, "read Secret %s: %v", key, err)
	}

	// The label is the Secret owner's opt-in. It cannot be expressed as CRD
	// validation because it lives on the referenced object, so the check has
	// to happen here.
	if secret.Labels[harborv1alpha1.CredentialsLabel] != harborv1alpha1.CredentialsLabelEnabled {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid,
			"Secret %s does not carry %s=%s", key,
			harborv1alpha1.CredentialsLabel, harborv1alpha1.CredentialsLabelEnabled)
	}

	// The CRD defaults these, but an object built in code carries whatever it
	// was given.
	usernameKey := valueOr(ref.UsernameKey, "username")
	passwordKey := valueOr(ref.PasswordKey, "password")

	username, ok := secret.Data[usernameKey]
	if !ok {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s has no key %q", key, usernameKey)
	}
	password, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", reasoned(harborv1alpha1.ReasonCredentialsInvalid, "Secret %s has no key %q", key, passwordKey)
	}
	return string(username), string(password), nil
}

func caBundle(ctx context.Context, c client.Reader, namespace string, conn *harborv1alpha1.HarborConnection) ([]byte, error) {
	ref := conn.Spec.CABundleRef
	if ref == nil {
		return nil, nil
	}
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "ConfigMap %s not found", key)
		}
		return nil, failed(harborv1alpha1.ReasonInternalError, "read ConfigMap %s: %v", key, err)
	}

	dataKey := valueOr(ref.Key, "ca.crt")
	bundle, ok := cm.Data[dataKey]
	if !ok {
		return nil, reasoned(harborv1alpha1.ReasonConnectionUnavailable, "ConfigMap %s has no key %q", key, dataKey)
	}
	return []byte(bundle), nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// allows reports whether conn lets a HarborProject in namespace manage the
// Harbor project called name.
func allows(conn *harborv1alpha1.HarborConnection, namespace, name string) bool {
	for _, rule := range conn.Spec.AllowedProjects {
		if rule.Namespace != "*" && rule.Namespace != namespace {
			continue
		}
		for _, pattern := range rule.Names {
			if matchName(pattern, name) {
				return true
			}
		}
	}
	return false
}

// matchName matches name against a pattern in which "*" stands for any run of
// characters. Nothing else is special, unlike in path.Match.
func matchName(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}

	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(name, part)
		if i < 0 {
			return false
		}
		name = name[i+len(part):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}
