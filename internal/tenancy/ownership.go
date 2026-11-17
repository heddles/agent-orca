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

package tenancy

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Who owns an object, and who changed it last. These annotations are the UI
// API's record of its own writes. app.kubernetes.io/managed-by cannot carry
// this: controllers set it on the objects they create, so it cannot tell an
// object made in the UI from an object made by a controller.
const (
	// AnnotationOwner says which writer owns the object. The UI API sets it to
	// OwnerUI on objects it creates, drops any value a request body carries, and
	// changes or deletes only objects that have it.
	AnnotationOwner = "agentorca.io/owner"
	// OwnerUI marks an object created through the UI API.
	OwnerUI = "ui"

	// AnnotationLastModifiedBy is the user ID (or, for a caller with no user, a
	// machine:<tenant> name) of the last UI API write.
	AnnotationLastModifiedBy = "agentorca.io/last-modified-by"
	// AnnotationLastModifiedAt is the RFC 3339 time of the last UI API write.
	AnnotationLastModifiedAt = "agentorca.io/last-modified-at"

	// FieldManagerUI is the server-side apply field manager of UI API writes.
	FieldManagerUI = "agent-orca-ui"
)

// OwnerOf returns the object's agentorca.io/owner annotation, "" when unset.
func OwnerOf(obj metav1.Object) string {
	return obj.GetAnnotations()[AnnotationOwner]
}

// StripOwnerAnnotations removes the ownership and actor annotations from an
// object read from a request body, so a caller cannot claim ownership or forge
// who made a change.
func StripOwnerAnnotations(obj metav1.Object) {
	a := obj.GetAnnotations()
	if a == nil {
		return
	}
	delete(a, AnnotationOwner)
	delete(a, AnnotationLastModifiedBy)
	delete(a, AnnotationLastModifiedAt)
	obj.SetAnnotations(a)
}

// StampUIWrite marks obj as owned by the UI and records who changed it and
// when. Call it after StripOwnerAnnotations.
func StampUIWrite(obj metav1.Object, actor string, at time.Time) {
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[AnnotationOwner] = OwnerUI
	a[AnnotationLastModifiedBy] = actor
	a[AnnotationLastModifiedAt] = at.UTC().Format(time.RFC3339)
	obj.SetAnnotations(a)
}
