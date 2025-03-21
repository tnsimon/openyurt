/*
Copyright 2024 The OpenYurt Authors.

Licensed under the Apache License, Version 2.0 (the License);
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an AS IS BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openyurtio/openyurt/pkg/apis/iot/v1beta1"
)

// Default satisfies the defaulting webhook interface.
func (webhook *PlatformAdminHandler) Default(ctx context.Context, obj runtime.Object) error {
	platformAdmin, ok := obj.(*v1beta1.PlatformAdmin)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a PlatformAdmin but got a %T", obj))
	}

	v1beta1.SetDefaultsPlatformAdmin(platformAdmin)

	if platformAdmin.Spec.Version == "" {
		platformAdmin.Spec.Version = webhook.Manifests.LatestVersion
	}

	if platformAdmin.Spec.Platform == "" {
		platformAdmin.Spec.Platform = v1beta1.PlatformAdminPlatformEdgeX
	}

	return nil
}
