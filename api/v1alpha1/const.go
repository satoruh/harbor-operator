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

	// ReasonConnected means Harbor answered and accepted the credentials.
	ReasonConnected = "Connected"

	// ReasonCredentialsRejected means the credentials were read but Harbor
	// did not accept them.
	ReasonCredentialsRejected = "CredentialsRejected"

	// ReasonConnectionUnavailable means the referenced HarborConnection is
	// missing or cannot be turned into a usable client.
	ReasonConnectionUnavailable = "ConnectionUnavailable"

	// ReasonNotAllowed means the HarborConnection does not let this namespace
	// manage a project of this name.
	ReasonNotAllowed = "NotAllowed"

	// ReasonCredentialsInvalid means the credentials Secret is missing, lacks
	// the opt-in label, or does not carry the expected keys.
	ReasonCredentialsInvalid = "CredentialsInvalid"

	// ReasonProjectInaccessible means the project exists but the credentials
	// may not see it or act on it. A deleted project is not reported this way:
	// Harbor answers 403 for either, so the controller tries to create it and
	// only a refusal of the name as taken ends up here.
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

	// ReasonInternalError means the controller could not read a Kubernetes
	// object it depends on, for a reason other than the object being absent:
	// the API server failed or refused. Harbor was not involved.
	ReasonInternalError = "InternalError"

	// ReasonHarborError means the Harbor API failed in a way that may resolve
	// on its own.
	ReasonHarborError = "HarborError"
)

// Event reasons. DeletionBlocked is recorded as an event too.
const (
	// EventProjectCreated is recorded when the controller creates the Harbor
	// project for a resource that had none.
	EventProjectCreated = "ProjectCreated"

	// EventProjectRecreated is recorded when the controller creates the
	// project again after the one the resource was bound to disappeared from
	// Harbor.
	EventProjectRecreated = "ProjectRecreated"

	// EventProjectAdopted is recorded when the resource binds to a project it
	// found in Harbor rather than created.
	EventProjectAdopted = "ProjectAdopted"

	// EventDeletionUnconfirmed is recorded when a resource is released
	// without Harbor confirming that its project is gone: Harbor answered 403
	// and does not list the project, which is what it says both for a deleted
	// project and for one the credentials may not see.
	EventDeletionUnconfirmed = "DeletionUnconfirmed"
)
