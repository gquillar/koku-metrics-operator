package storage

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kokumetricscfgv1alpha1 "github.com/project-koku/koku-metrics-operator/api/v1alpha1"
)

var (
	tenGi = *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI)
	// DefaultPVC is a basic PVC
	DefaultPVC = kokumetricscfgv1alpha1.EmbeddedPersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
		},
		EmbeddedObjectMetadata: kokumetricscfgv1alpha1.EmbeddedObjectMetadata{
			Name: "koku-metrics-operator-data",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{"ReadWriteOnce"},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: tenGi,
				},
			},
		},
	}
)

type Storage struct {
	PVC    *corev1.PersistentVolumeClaim
	Client client.Client
	vol    *volume
}

type volume struct {
	index  int
	volume *corev1.Volume
}

func (s *Storage) getOrCreateVolume() error {
	ctx := context.Background()
	namespace := types.NamespacedName{
		Namespace: "koku-metrics-operator",
		Name:      s.PVC.Name}
	if err := s.Client.Get(ctx, namespace, s.PVC); err == nil {
		return nil
	}
	return s.Client.Create(ctx, s.PVC)
}

func (s *Storage) getVolume(vols []corev1.Volume, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) error {
	for i, v := range vols {
		if v.Name == "koku-metrics-operator-reports" {
			s.vol = &volume{index: i, volume: &v}
			if v.EmptyDir != nil {
				kmCfg.Status.Storage.VolumeType = v.EmptyDir.String()
			}
			if v.PersistentVolumeClaim != nil {
				kmCfg.Status.Storage.VolumeType = v.PersistentVolumeClaim.String()
			}
			return nil
		}
	}
	return fmt.Errorf("volume not found")
}

func (v *volume) isMounted() bool {
	return v.volume.PersistentVolumeClaim != nil
}

func (s *Storage) mountVolume(dep *appsv1.Deployment) (bool, error) {
	ctx := context.Background()
	s.vol.volume.EmptyDir = nil
	s.vol.volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: s.PVC.Name,
	}
	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Template.Spec.Volumes[s.vol.index] = *s.vol.volume

	if err := s.Client.Patch(ctx, dep, patch); err != nil {
		return false, fmt.Errorf("failed to Patch deployment: %v", err)
	}
	return true, nil
}

func ConvertPVC(s *Storage, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) (bool, error) {
	ctx := context.Background()

	deployment := &appsv1.Deployment{}
	namespace := types.NamespacedName{
		Namespace: "koku-metrics-operator",
		Name:      "koku-metrics-controller-manager"}
	if err := s.Client.Get(ctx, namespace, deployment); err != nil {
		return false, fmt.Errorf("unable to get deployment: %v", err)
	}
	deployCp := deployment.DeepCopy()

	if err := s.getVolume(deployCp.Spec.Template.Spec.Volumes, kmCfg); err != nil {
		return false, err
	}

	if s.vol.isMounted() && s.vol.volume.PersistentVolumeClaim.ClaimName == s.PVC.Name {
		kmCfg.Status.Storage.VolumeMounted = true
		return false, nil
	}

	if err := s.getOrCreateVolume(); err != nil {
		return false, fmt.Errorf("failed to get or create PVC: %v", err)
	}

	return s.mountVolume(deployCp)
}

// MakeVolumeClaimTemplate produces a template to create the PVC
func MakeVolumeClaimTemplate(e kokumetricscfgv1alpha1.EmbeddedPersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: e.APIVersion,
			Kind:       e.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        e.Name,
			Namespace:   "koku-metrics-operator",
			Labels:      e.Labels,
			Annotations: e.Annotations,
		},
		Spec: e.Spec,
	}
}