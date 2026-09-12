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
	// ReasonSynced means the Harbor project matches the spec.
	ReasonSynced = "Synced"

	// ReasonConnectionUnavailable means the referenced HarborConnection is
	// missing or cannot be turned into a usable client.
	ReasonConnectionUnavailable = "ConnectionUnavailable"

	// ReasonCredentialsInvalid means the credentials Secret is missing, lacks
	// the opt-in label, or does not carry the expected keys.
	ReasonCredentialsInvalid = "CredentialsInvalid"

	// ReasonProjectInaccessible means Harbor will not show the project. It
	// covers both a deleted project and a permission problem: Harbor answers
	// 403 for either and does not distinguish them.
	ReasonProjectInaccessible = "ProjectInaccessible"

	// ReasonAdoptionRefused means a project with the same name already exists
	// and spec.adoptExisting is not set.
	ReasonAdoptionRefused = "AdoptionRefused"

	// ReasonInvalidProjectName means Harbor rejected the name. Retrying does
	// not help; the spec has to change.
	ReasonInvalidProjectName = "InvalidProjectName"

	// ReasonDeletionBlocked means the project still holds repositories, so it
	// cannot be deleted. The resource stays until they are gone.
	ReasonDeletionBlocked = "DeletionBlocked"

	// ReasonHarborError means the Harbor API failed in a way that may resolve
	// on its own.
	ReasonHarborError = "HarborError"
)
