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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// setCondition upserts a metav1.Condition into a conditions slice.
// If a condition with the same Type already exists, it is replaced only if
// the Status or Message changed (to avoid unnecessary status updates).
func setCondition(conditions *[]metav1.Condition, desired metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == desired.Type {
			if c.Status == desired.Status && c.Message == desired.Message {
				return // no change
			}
			(*conditions)[i] = desired
			return
		}
	}
	*conditions = append(*conditions, desired)
}
