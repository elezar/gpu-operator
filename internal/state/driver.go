/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package state

import (
	"context"
	"fmt"
	"os"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/v1alpha1"
	"github.com/NVIDIA/gpu-operator/controllers/clusterinfo"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	"github.com/NVIDIA/gpu-operator/internal/image"
	"github.com/NVIDIA/gpu-operator/internal/render"
	"github.com/NVIDIA/gpu-operator/internal/utils"
)

const (
	nfdOSReleaseIDLabelKey = "feature.node.kubernetes.io/system-os_release.ID"
	nfdOSVersionIDLabelKey = "feature.node.kubernetes.io/system-os_release.VERSION_ID"
)

type stateDriver struct {
	stateSkel
}

var _ State = (*stateDriver)(nil)

type driverRuntimeSpec struct {
	Namespace                     string
	KubernetesVersion             string
	OpenshiftVersion              string
	OpenshiftDriverToolkitEnabled bool
	OpenshiftRHCOSVersions        []string
	OpenshiftDriverToolkitImages  map[string]string
}

type openshiftSpec struct {
	ToolkitImage string
	RHCOSVersion string
}

type additionalConfigs struct {
	VolumeMounts []corev1.VolumeMount
	Volumes      []corev1.Volume
}

type driverRenderData struct {
	Driver            *driverSpec
	Operator          *gpuv1.OperatorSpec
	Validator         *validatorSpec
	GDS               *gdsDriverSpec
	GPUDirectRDMA     *gpuv1.GPUDirectRDMASpec
	Runtime           *driverRuntimeSpec
	Openshift         *openshiftSpec
	AdditionalConfigs *additionalConfigs
}

func NewStateDriver(
	k8sClient client.Client,
	scheme *runtime.Scheme,
	manifestDir string) (State, error) {

	files, err := utils.GetFilesWithSuffix(manifestDir, render.ManifestFileSuffix...)
	if err != nil {
		return nil, fmt.Errorf("failed to get files from manifest directory: %v", err)
	}

	renderer := render.NewRenderer(files)
	state := &stateDriver{
		stateSkel: stateSkel{
			name:        "state-driver",
			description: "NVIDIA driver deployed in the cluster",
			client:      k8sClient,
			scheme:      scheme,
			renderer:    renderer,
		},
	}
	return state, nil
}

func (s *stateDriver) Sync(ctx context.Context, customResource interface{}, infoCatalog InfoCatalog) (SyncState, error) {
	cr, ok := customResource.(*nvidiav1alpha1.NVIDIADriver)
	if !ok {
		return SyncStateError, fmt.Errorf("NVIDIADriver CR not provided as input to Sync()")
	}

	info := infoCatalog.Get(InfoTypeClusterPolicyCR)
	if info == nil {
		return SyncStateError, fmt.Errorf("failed to get ClusterPolicy CR from info catalog")
	}
	clusterPolicy, ok := info.(gpuv1.ClusterPolicy)
	if !ok {
		return SyncStateError, fmt.Errorf("failed to get ClusterPolicy CR from info catalog")
	}

	info = infoCatalog.Get(InfoTypeClusterInfo)
	if info == nil {
		return SyncStateNotReady, fmt.Errorf("failed to get cluster info from info catalog")
	}
	clusterInfo := info.(clusterinfo.Interface)

	objs, err := s.getManifestObjects(ctx, cr, &clusterPolicy, clusterInfo)
	if err != nil {
		return SyncStateNotReady, fmt.Errorf("failed to create k8s objects from manifests: %v", err)
	}

	// Create objects if they don't exist, Update objects if they do exist
	err = s.createOrUpdateObjs(ctx, func(obj *unstructured.Unstructured) error {
		if err := controllerutil.SetControllerReference(cr, obj, s.scheme); err != nil {
			return fmt.Errorf("failed to set controller reference for object: %v", err)
		}
		return nil
	}, objs)
	if err != nil {
		return SyncStateNotReady, fmt.Errorf("failed to create/update objects: %v", err)
	}

	// Check objects status
	syncState, err := s.getSyncState(ctx, objs)
	if err != nil {
		return SyncStateNotReady, fmt.Errorf("failed to get sync state: %v", err)
	}
	return syncState, nil
}

func (s *stateDriver) GetWatchSources(mgr ctrlManager) map[string]SyncingSource {
	wr := make(map[string]SyncingSource)
	wr["DaemonSet"] = source.Kind(
		mgr.GetCache(),
		&appsv1.DaemonSet{},
	)
	return wr
}

func (s *stateDriver) getManifestObjects(ctx context.Context, cr *nvidiav1alpha1.NVIDIADriver, clusterPolicy *gpuv1.ClusterPolicy, clusterInfo clusterinfo.Interface) ([]*unstructured.Unstructured, error) {
	logger := log.FromContext(ctx)

	runtimeSpec, err := getRuntimeSpec(clusterInfo, cr.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to construct cluster runtime spec: %w", err)
	}

	driverSpec, err := getDriverSpec(cr)
	if err != nil {
		return nil, fmt.Errorf("failed to construct driver spec: %v", err)
	}

	validatorSpec, err := getValidatorSpec(&clusterPolicy.Spec.Validator)
	if err != nil {
		return nil, fmt.Errorf("failed to construct validator spec: %v", err)
	}

	gdsSpec, err := getGDSSpec(clusterPolicy.Spec.GPUDirectStorage)
	if err != nil {
		return nil, fmt.Errorf("failed to construct GDS spec: %v", err)
	}

	operatorSpec := &clusterPolicy.Spec.Operator
	gpuDirectRDMASpec := clusterPolicy.Spec.Driver.GPUDirectRDMA

	renderData := &driverRenderData{
		Driver:            driverSpec,
		Operator:          operatorSpec,
		Validator:         validatorSpec,
		GDS:               gdsSpec,
		GPUDirectRDMA:     gpuDirectRDMASpec,
		Runtime:           runtimeSpec,
		AdditionalConfigs: &additionalConfigs{},
	}

	var objs []*unstructured.Unstructured
	if runtimeSpec.OpenshiftDriverToolkitEnabled {
		for _, rhcosVersion := range runtimeSpec.OpenshiftRHCOSVersions {
			renderData.Openshift = &openshiftSpec{
				RHCOSVersion: rhcosVersion,
				ToolkitImage: runtimeSpec.OpenshiftDriverToolkitImages[rhcosVersion],
			}
			manifestObjs, err := s.renderManifestObjects(ctx, renderData)
			if err != nil {
				logger.Error(err, "error rendering manifests for openshift build", "RHCOSVersion", rhcosVersion)
				return nil, err
			}
			objs = append(objs, manifestObjs...)
		}
	} else {
		objs, err = s.renderManifestObjects(ctx, renderData)
	}

	return objs, nil
}

func (s *stateDriver) renderManifestObjects(ctx context.Context, renderData *driverRenderData) ([]*unstructured.Unstructured, error) {
	logger := log.FromContext(ctx)

	logger.V(consts.LogLevelDebug).Info("Rendering objects", "data:", renderData)

	objs, err := s.renderer.RenderObjects(
		&render.TemplatingData{
			Data: renderData,
			Funcs: template.FuncMap{
				"Deref": func(b *bool) bool {
					if b == nil {
						return false
					}
					return *b
				},
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to render kubernetes manifests: %v", err)
	}
	logger.V(consts.LogLevelDebug).Info("Rendered", "objects:", objs)

	return objs, nil
}

func getDriverName(cr *nvidiav1alpha1.NVIDIADriver) string {
	const (
		nameFormat = "nvidia-%s-driver-%s"
		// https://github.com/kubernetes/apimachinery/blob/v0.28.1/pkg/util/validation/validation.go#L35
		qualifiedNameMaxLength = 63
	)

	name := fmt.Sprintf(nameFormat, cr.Spec.DriverType, cr.Name)
	// truncate name if it exceeds the maximum length
	if len(name) > qualifiedNameMaxLength {
		name = name[:qualifiedNameMaxLength]
	}
	return name
}

func getDriverSpec(cr *nvidiav1alpha1.NVIDIADriver) (*driverSpec, error) {
	if cr == nil {
		return nil, fmt.Errorf("no NVIDIADriver CR provided")
	}

	nvidiaDriverName := getDriverName(cr)

	spec := &cr.Spec
	// TODO: construct image path differently for precompiled
	imagePath, err := spec.GetImagePath()
	if err != nil {
		return nil, fmt.Errorf("failed to construct image path for driver: %w", err)
	}

	managerImagePath, err := image.ImagePath(spec.Manager.Repository, spec.Manager.Image, spec.Manager.Version, "DRIVER_MANAGER_IMAGE")
	if err != nil {
		return nil, fmt.Errorf("failed to construct image path for driver manager: %w", err)
	}

	// If osVersion is set in the CR, add nodeSelectors (using NFD labels)
	// which ensure this driver only gets scheduled on nodes matching osVersion
	if spec.OSVersion != "" {
		id, versionID, err := spec.ParseOSVersion()
		if err != nil {
			return nil, fmt.Errorf("failed to parse osVersion: %w", err)
		}

		if spec.NodeSelector == nil {
			spec.NodeSelector = make(map[string]string)
		}

		spec.NodeSelector[nfdOSReleaseIDLabelKey] = id
		spec.NodeSelector[nfdOSVersionIDLabelKey] = versionID
	}

	return &driverSpec{
		Spec:             spec,
		Name:             nvidiaDriverName,
		ImagePath:        imagePath,
		ManagerImagePath: managerImagePath,
	}, nil
}

func getValidatorSpec(spec *gpuv1.ValidatorSpec) (*validatorSpec, error) {
	if spec == nil {
		return nil, fmt.Errorf("no validator spec provided")
	}
	imagePath, err := image.ImagePath(spec.Repository, spec.Image, spec.Version, "VALIDATOR_IMAGE")
	if err != nil {
		return nil, fmt.Errorf("failed to contruct image path for validator: %v", err)
	}

	return &validatorSpec{
		spec,
		imagePath,
	}, nil
}

func getGDSSpec(spec *gpuv1.GPUDirectStorageSpec) (*gdsDriverSpec, error) {
	if spec == nil {
		// note: GDS is optional in Clusterpolicy CRD
		return nil, nil
	}
	imagePath, err := image.ImagePath(spec.Repository, spec.Image, spec.Version, "GDS_IMAGE")
	if err != nil {
		return nil, fmt.Errorf("failed to contruct image path for validator: %v", err)
	}

	return &gdsDriverSpec{
		spec,
		imagePath,
	}, nil
}

func getRuntimeSpec(info clusterinfo.Interface, nodeSelector map[string]string) (*driverRuntimeSpec, error) {
	k8sVersion, err := info.GetKubernetesVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubernetes version: %v", err)
	}
	openshiftVersion, err := info.GetOpenshiftVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get openshift version: %v", err)
	}

	operatorNamespace := os.Getenv("OPERATOR_NAMESPACE")
	if operatorNamespace == "" {
		return nil, fmt.Errorf("OPERATOR_NAMESPACE environment variable not set")
	}

	rs := &driverRuntimeSpec{
		Namespace:         operatorNamespace,
		KubernetesVersion: k8sVersion,
		OpenshiftVersion:  openshiftVersion,
	}

	if openshiftVersion != "" {
		rhcosVersions, err := info.GetRHCOSVersions(nodeSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to list openshift versions: %w", err)
		}
		openshiftDTKMap := info.GetOpenshiftDriverToolkitImages()

		rs.OpenshiftDriverToolkitEnabled = len(rhcosVersions) > 0 && len(openshiftDTKMap) > 0
		rs.OpenshiftRHCOSVersions = rhcosVersions
		rs.OpenshiftDriverToolkitImages = openshiftDTKMap
	}

	return rs, nil
}
