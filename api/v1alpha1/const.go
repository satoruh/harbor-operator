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

package v1alpha1

const (
	// HarborProjectFinalizer is added to HarborProject resources to ensure the
	// corresponding Harbor project is handled before the CR is removed.
	// Must be a qualified name (<domain>/<name>).
	HarborProjectFinalizer = "harborproject.harbor.satoruh.org/finalizer"

	// CredentialsLabel must be set to "true" on a Secret before it can be
	// referenced as Harbor credentials. This is an explicit opt-in by the
	// Secret owner.
	CredentialsLabel = "harbor.satoruh.org/credentials"

	// CredentialsLabelEnabled is the value CredentialsLabel must carry.
	CredentialsLabelEnabled = "true"
)

// Condition types used by this API group
const (
	// ConditionReady indicates the resource has been reconciled successfully
	// and its observed state matches the desired state.
	ConditionReady = "Ready"
)

// Condition reasons
const (
	ReasonSynced = "Synced"
)
