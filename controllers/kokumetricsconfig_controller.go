/*


Copyright 2020 Red Hat, Inc.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/xorcare/pointer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kokumetricscfgv1alpha1 "github.com/project-koku/koku-metrics-operator/api/v1alpha1"
	"github.com/project-koku/koku-metrics-operator/archive"
	cv "github.com/project-koku/koku-metrics-operator/clusterversion"
	"github.com/project-koku/koku-metrics-operator/collector"
	"github.com/project-koku/koku-metrics-operator/crhchttp"
	"github.com/project-koku/koku-metrics-operator/dirconfig"
	"github.com/project-koku/koku-metrics-operator/packaging"
	"github.com/project-koku/koku-metrics-operator/sources"
)

var (
	GitCommit string

	openShiftConfigNamespace = "openshift-config"
	pullSecretName           = "pull-secret"
	pullSecretDataKey        = ".dockerconfigjson"
	pullSecretAuthKey        = "cloud.openshift.com"
	authSecretUserKey        = "username"
	authSecretPasswordKey    = "password"
	promCompareFormat        = "2006-01-02T15"

	dirCfg     *dirconfig.DirectoryConfig = new(dirconfig.DirectoryConfig)
	sourceSpec *kokumetricscfgv1alpha1.CloudDotRedHatSourceSpec
)

// KokuMetricsConfigReconciler reconciles a KokuMetricsConfig object
type KokuMetricsConfigReconciler struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	cvClientBuilder cv.ClusterVersionBuilder
	promCollector   *collector.PromCollector
}

type serializedAuthMap struct {
	Auths map[string]serializedAuth `json:"auths"`
}
type serializedAuth struct {
	Auth string `json:"auth"`
}

// StringReflectSpec Determine if the string Status item reflects the Spec item if not empty, otherwise take the default value.
func StringReflectSpec(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, specItem *string, statusItem *string, defaultVal string) (string, bool) {
	// Update statusItem if needed
	changed := false
	if *statusItem == "" || !reflect.DeepEqual(*specItem, *statusItem) {
		// If data is specified in the spec it should be used
		changed = true
		if *specItem != "" {
			*statusItem = *specItem
		} else if defaultVal != "" {
			*statusItem = defaultVal
		} else {
			*statusItem = *specItem
		}
	}
	return *statusItem, changed
}

// ReflectSpec Determine if the Status item reflects the Spec item if not empty, otherwise set a default value if applicable.
func ReflectSpec(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) {

	StringReflectSpec(r, kmCfg, &kmCfg.Spec.APIURL, &kmCfg.Status.APIURL, kokumetricscfgv1alpha1.DefaultAPIURL)
	StringReflectSpec(r, kmCfg, &kmCfg.Spec.Authentication.AuthenticationSecretName, &kmCfg.Status.Authentication.AuthenticationSecretName, "")

	if !reflect.DeepEqual(kmCfg.Spec.Authentication.AuthType, kmCfg.Status.Authentication.AuthType) {
		kmCfg.Status.Authentication.AuthType = kmCfg.Spec.Authentication.AuthType
	}
	kmCfg.Status.Upload.ValidateCert = kmCfg.Spec.Upload.ValidateCert

	StringReflectSpec(r, kmCfg, &kmCfg.Spec.Upload.IngressAPIPath, &kmCfg.Status.Upload.IngressAPIPath, kokumetricscfgv1alpha1.DefaultIngressPath)
	kmCfg.Status.Upload.UploadToggle = kmCfg.Spec.Upload.UploadToggle

	// set the default max file size for packaging
	kmCfg.Status.Packaging.MaxSize = &kmCfg.Spec.Packaging.MaxSize

	// set the upload wait to whatever is in the spec, if the spec is defined
	if kmCfg.Spec.Upload.UploadWait != nil {
		kmCfg.Status.Upload.UploadWait = kmCfg.Spec.Upload.UploadWait
	}

	// if the status is nil, generate an upload wait
	if kmCfg.Status.Upload.UploadWait == nil {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		uploadWait := r.Int63() % 35
		kmCfg.Status.Upload.UploadWait = &uploadWait
	}

	if !reflect.DeepEqual(kmCfg.Spec.Upload.UploadCycle, kmCfg.Status.Upload.UploadCycle) {
		kmCfg.Status.Upload.UploadCycle = kmCfg.Spec.Upload.UploadCycle
	}

	StringReflectSpec(r, kmCfg, &kmCfg.Spec.Source.SourcesAPIPath, &kmCfg.Status.Source.SourcesAPIPath, kokumetricscfgv1alpha1.DefaultSourcesPath)
	StringReflectSpec(r, kmCfg, &kmCfg.Spec.Source.SourceName, &kmCfg.Status.Source.SourceName, "")

	kmCfg.Status.Source.CreateSource = kmCfg.Spec.Source.CreateSource

	if !reflect.DeepEqual(kmCfg.Spec.Source.CheckCycle, kmCfg.Status.Source.CheckCycle) {
		kmCfg.Status.Source.CheckCycle = kmCfg.Spec.Source.CheckCycle
	}

	StringReflectSpec(r, kmCfg, &kmCfg.Spec.PrometheusConfig.SvcAddress, &kmCfg.Status.Prometheus.SvcAddress, kokumetricscfgv1alpha1.DefaultPrometheusSvcAddress)
	kmCfg.Status.Prometheus.SkipTLSVerification = kmCfg.Spec.PrometheusConfig.SkipTLSVerification
}

// GetClusterID Collects the cluster identifier from the Cluster Version custom resource object
func GetClusterID(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) error {
	log := r.Log.WithValues("KokuMetricsConfig", "GetClusterID")
	// Get current ClusterVersion
	cvClient := r.cvClientBuilder.New(r)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return err
	}
	log.Info("cluster version found", "ClusterVersion", clusterVersion.Spec)
	if clusterVersion.Spec.ClusterID != "" {
		kmCfg.Status.ClusterID = string(clusterVersion.Spec.ClusterID)
	}
	return nil
}

// GetPullSecretToken Obtain the bearer token string from the pull secret in the openshift-config namespace
func GetPullSecretToken(r *KokuMetricsConfigReconciler, authConfig *crhchttp.AuthConfig) error {
	ctx := context.Background()
	log := r.Log.WithValues("KokuMetricsConfig", "GetPullSecretToken")
	secret := &corev1.Secret{}
	namespace := types.NamespacedName{
		Namespace: openShiftConfigNamespace,
		Name:      pullSecretName}
	err := r.Get(ctx, namespace, secret)
	if err != nil {
		switch {
		case errors.IsNotFound(err):
			log.Error(err, "pull-secret does not exist")
		case errors.IsForbidden(err):
			log.Error(err, "operator does not have permission to check pull-secret")
		default:
			log.Error(err, "could not check pull-secret")
		}
		return err
	}

	tokenFound := false
	encodedPullSecret := secret.Data[pullSecretDataKey]
	if len(encodedPullSecret) <= 0 {
		return fmt.Errorf("cluster authorization secret did not have data")
	}
	var pullSecret serializedAuthMap
	if err := json.Unmarshal(encodedPullSecret, &pullSecret); err != nil {
		log.Error(err, "unable to unmarshal cluster pull-secret")
		return err
	}
	if auth, ok := pullSecret.Auths[pullSecretAuthKey]; ok {
		token := strings.TrimSpace(auth.Auth)
		if strings.Contains(token, "\n") || strings.Contains(token, "\r") {
			return fmt.Errorf("cluster authorization token is not valid: contains newlines")
		}
		if len(token) > 0 {
			log.Info("found cloud.openshift.com token")
			authConfig.BearerTokenString = token
			tokenFound = true
		} else {
			return fmt.Errorf("cluster authorization token is not found")
		}
	} else {
		return fmt.Errorf("cluster authorization token was not found in secret data")
	}
	if !tokenFound {
		return fmt.Errorf("cluster authorization token is not found")
	}
	return nil
}

// GetAuthSecret Obtain the username and password from the authentication secret provided in the current namespace
func GetAuthSecret(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, authConfig *crhchttp.AuthConfig, reqNamespace types.NamespacedName) error {
	ctx := context.Background()
	log := r.Log.WithValues("KokuMetricsConfig", "GetAuthSecret")

	log.Info("secret namespace", "namespace", reqNamespace.Namespace)
	secret := &corev1.Secret{}
	namespace := types.NamespacedName{
		Namespace: reqNamespace.Namespace,
		Name:      kmCfg.Status.Authentication.AuthenticationSecretName}
	err := r.Get(ctx, namespace, secret)
	if err != nil {
		switch {
		case errors.IsNotFound(err):
			log.Error(err, "secret does not exist")
		case errors.IsForbidden(err):
			log.Error(err, "operator does not have permission to check secret")
		default:
			log.Error(err, "could not check secret")
		}
		return err
	}

	if val, ok := secret.Data[authSecretUserKey]; ok {
		authConfig.BasicAuthUser = string(val)
	} else {
		log.Info("secret not found with expected user data")
		err = fmt.Errorf("secret not found with expected user data")
		return err
	}

	if val, ok := secret.Data[authSecretPasswordKey]; ok {
		authConfig.BasicAuthPassword = string(val)
	} else {
		log.Info("secret not found with expected password data")
		err = fmt.Errorf("secret not found with expected password data")
		return err
	}
	return nil
}

func checkCycle(logger logr.Logger, cycle int64, lastExecution metav1.Time, action string) bool {
	log := logger.WithValues("KokuMetricsConfig", "checkCycle")
	if lastExecution.IsZero() {
		log.Info(fmt.Sprintf("there have been no prior successful %ss to cloud.redhat.com", action))
		return true
	}

	duration := time.Since(lastExecution.Time.UTC())
	minutes := int64(duration.Minutes())
	log.Info(fmt.Sprintf("it has been %d minute(s) since the last successful %s", minutes, action))
	if minutes >= cycle {
		log.Info(fmt.Sprintf("executing %s to cloud.redhat.com", action))
		return true
	}
	log.Info(fmt.Sprintf("not time to execute the %s", action))
	return false

}

func setClusterID(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) error {
	if kmCfg.Status.ClusterID == "" {
		r.cvClientBuilder = cv.NewBuilder()
		err := GetClusterID(r, kmCfg)
		return err
	}
	return nil
}

func setAuthentication(r *KokuMetricsConfigReconciler, authConfig *crhchttp.AuthConfig, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, reqNamespace types.NamespacedName) error {
	log := r.Log.WithValues("KokuMetricsConfig", "setAuthentication")
	if kmCfg.Status.Authentication.AuthType == kokumetricscfgv1alpha1.Token {
		// Get token from pull secret
		err := GetPullSecretToken(r, authConfig)
		if err != nil {
			log.Error(nil, "failed to obtain cluster authentication token")
			kmCfg.Status.Authentication.AuthenticationCredentialsFound = pointer.Bool(false)
		} else {
			kmCfg.Status.Authentication.AuthenticationCredentialsFound = pointer.Bool(true)
		}
		return err
	} else if kmCfg.Spec.Authentication.AuthenticationSecretName != "" {
		// Get user and password from auth secret in namespace
		err := GetAuthSecret(r, kmCfg, authConfig, reqNamespace)
		if err != nil {
			log.Error(nil, "failed to obtain authentication secret credentials")
			kmCfg.Status.Authentication.AuthenticationCredentialsFound = pointer.Bool(false)
		} else {
			kmCfg.Status.Authentication.AuthenticationCredentialsFound = pointer.Bool(true)
		}
		return err
	} else {
		// No authentication secret name set when using basic auth
		kmCfg.Status.Authentication.AuthenticationCredentialsFound = pointer.Bool(false)
		err := fmt.Errorf("no authentication secret name set when using basic auth")
		return err
	}
}

func setOperatorCommit(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) {
	log := r.Log.WithName("setOperatorCommit")
	if GitCommit == "" {
		commit, exists := os.LookupEnv("GIT_COMMIT")
		if exists {
			msg := fmt.Sprintf("using git commit from environment: %s", commit)
			log.Info(msg)
			GitCommit = commit
		}
	}
	kmCfg.Status.OperatorCommit = GitCommit
}

func checkSource(r *KokuMetricsConfigReconciler, authConfig *crhchttp.AuthConfig, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) {
	// check if the Source Spec has changed
	updated := false
	if sourceSpec != nil {
		updated = !reflect.DeepEqual(*sourceSpec, kmCfg.Spec.Source)
	}
	sourceSpec = kmCfg.Spec.Source.DeepCopy()

	sSpec := &sources.SourceSpec{
		APIURL: kmCfg.Status.APIURL,
		Auth:   authConfig,
		Spec:   kmCfg.Status.Source,
		Log:    r.Log,
	}
	log := r.Log.WithValues("KokuMetricsConfig", "checkSource")
	if sSpec.Spec.SourceName != "" && (updated || checkCycle(r.Log, *sSpec.Spec.CheckCycle, sSpec.Spec.LastSourceCheckTime, "source check")) {
		client := crhchttp.GetClient(authConfig)
		kmCfg.Status.Source.SourceError = ""
		defined, lastCheck, err := sources.SourceGetOrCreate(sSpec, client)
		if err != nil {
			kmCfg.Status.Source.SourceError = err.Error()
			log.Info("source get or create message", "error", err)
		}
		kmCfg.Status.Source.SourceDefined = &defined
		kmCfg.Status.Source.LastSourceCheckTime = lastCheck
	}
}

func packageFiles(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, dirCfg *dirconfig.DirectoryConfig) *packaging.FilePackager {
	log := r.Log.WithValues("KokuMetricsConfig", "packageFiles")

	// if its time to upload/package
	if !checkCycle(r.Log, *kmCfg.Status.Upload.UploadCycle, kmCfg.Status.Upload.LastSuccessfulUploadTime, "package") {
		return nil
	}

	// Package and split the payload if necessary
	packager := packaging.FilePackager{
		KMCfg:   kmCfg,
		DirCfg:  dirCfg,
		Log:     r.Log,
		MaxSize: *kmCfg.Status.Packaging.MaxSize,
	}
	kmCfg.Status.Packaging.PackagingError = ""
	if err := packager.PackageReports(); err != nil {
		log.Error(err, "PackageReports failed")
		// update the CR packaging error status
		kmCfg.Status.Packaging.PackagingError = err.Error()
	}
	return &packager
}

func uploadFiles(r *KokuMetricsConfigReconciler, authConfig *crhchttp.AuthConfig, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, dirCfg *dirconfig.DirectoryConfig, packager *packaging.FilePackager) error {
	log := r.Log.WithValues("KokuMetricsConfig", "uploadFiles")

	// if its time to upload/package
	if !*kmCfg.Spec.Upload.UploadToggle {
		log.Info("operator is configured to not upload reports to cloud.redhat.com")
		return nil
	}
	if !checkCycle(r.Log, *kmCfg.Status.Upload.UploadCycle, kmCfg.Status.Upload.LastSuccessfulUploadTime, "upload") {
		return nil
	}

	uploadFiles, err := packager.ReadUploadDir()
	if err != nil {
		log.Error(err, "failed to read upload directory")
		return err
	}

	if len(uploadFiles) <= 0 {
		log.Info("no files to upload")
		return nil
	}

	log.Info("files ready for upload: " + strings.Join(uploadFiles, ", "))
	log.Info("pausing for " + fmt.Sprintf("%d", *kmCfg.Status.Upload.UploadWait) + " seconds before uploading")
	time.Sleep(time.Duration(*kmCfg.Status.Upload.UploadWait) * time.Second)
	for _, file := range uploadFiles {
		if !strings.Contains(file, "tar.gz") {
			continue
		}
		log.Info(fmt.Sprintf("uploading file: %s", file))
		// grab the body and the multipart file header
		body, contentType, err := crhchttp.GetMultiPartBodyAndHeaders(filepath.Join(dirCfg.Upload.Path, file))
		if err != nil {
			log.Error(err, "failed to set multipart body and headers")
			return err
		}
		ingressURL := kmCfg.Status.APIURL + kmCfg.Status.Upload.IngressAPIPath
		uploadStatus, uploadTime, err := crhchttp.Upload(authConfig, contentType, "POST", ingressURL, body)
		kmCfg.Status.Upload.UploadError = ""
		if err != nil {
			log.Error(err, "upload failed")
			kmCfg.Status.Upload.UploadError = err.Error()
		}
		kmCfg.Status.Upload.LastUploadStatus = uploadStatus
		kmCfg.Status.Upload.LastUploadTime = uploadTime
		if strings.Contains(uploadStatus, "202") {
			kmCfg.Status.Upload.LastSuccessfulUploadTime = uploadTime
			// remove the tar.gz after a successful upload
			log.Info("removing tar file since upload was successful")
			if err := os.Remove(filepath.Join(dirCfg.Upload.Path, file)); err != nil {
				log.Error(err, "error removing tar file")
			}
		}
	}
	return nil
}

func collectPromStats(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, dirCfg *dirconfig.DirectoryConfig) {
	log := r.Log.WithValues("KokuMetricsConfig", "collectPromStats")
	if r.promCollector == nil {
		r.promCollector = &collector.PromCollector{
			Client: r.Client,
			Log:    r.Log,
		}
	}
	r.promCollector.TimeSeries = nil

	if err := r.promCollector.GetPromConn(kmCfg); err != nil {
		log.Error(err, "failed to get prometheus connection")
		return
	}
	timeUTC := metav1.Now().UTC()
	t := metav1.Time{Time: timeUTC}
	timeRange := promv1.Range{
		Start: time.Date(t.Year(), t.Month(), t.Day(), t.Hour()-1, 0, 0, 0, t.Location()),
		End:   time.Date(t.Year(), t.Month(), t.Day(), t.Hour()-1, 59, 59, 0, t.Location()),
		Step:  time.Minute,
	}
	r.promCollector.TimeSeries = &timeRange

	if kmCfg.Status.Prometheus.LastQuerySuccessTime.UTC().Format(promCompareFormat) == t.Format(promCompareFormat) {
		log.Info("reports already generated for range", "start", timeRange.Start, "end", timeRange.End)
		return
	}
	kmCfg.Status.Prometheus.LastQueryStartTime = t
	log.Info("generating reports for range", "start", timeRange.Start, "end", timeRange.End)
	if err := collector.GenerateReports(kmCfg, dirCfg, r.promCollector); err != nil {
		kmCfg.Status.Reports.DataCollected = false
		kmCfg.Status.Reports.DataCollectionMessage = fmt.Sprintf("error: %v", err)
		log.Error(err, "failed to generate reports")
		return
	}
	log.Info("reports generated for range", "start", timeRange.Start, "end", timeRange.End)
	kmCfg.Status.Prometheus.LastQuerySuccessTime = t

}

func getOrCreateVolume(r *KokuMetricsConfigReconciler, pvc *corev1.PersistentVolumeClaim) error {
	ctx := context.Background()
	namespace := types.NamespacedName{
		Namespace: "koku-metrics-operator",
		Name:      pvc.Name}
	if err := r.Get(ctx, namespace, pvc); err == nil {
		return nil
	}
	return r.Client.Create(ctx, pvc)
}

func getVolume(vols []corev1.Volume, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig) (int, *corev1.Volume, error) {
	for i, v := range vols {
		if v.Name == "koku-metrics-operator-reports" {
			if v.EmptyDir != nil {
				kmCfg.Status.Storage.VolumeType = v.EmptyDir.String()
			}
			if v.PersistentVolumeClaim != nil {
				kmCfg.Status.Storage.VolumeType = v.PersistentVolumeClaim.String()
			}
			return i, &v, nil
		}
	}
	return -1, nil, fmt.Errorf("volume not found")
}

func isMounted(vol corev1.Volume) bool {
	return vol.PersistentVolumeClaim != nil
}

func mountVolume(r *KokuMetricsConfigReconciler, dep *appsv1.Deployment, volIndex int, vol corev1.Volume, claimName string) (bool, error) {
	ctx := context.Background()
	vol.EmptyDir = nil
	vol.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: claimName,
	}
	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Template.Spec.Volumes[volIndex] = vol

	if err := r.Patch(ctx, dep, patch); err != nil {
		return false, fmt.Errorf("failed to Patch deployment: %v", err)
	}
	return true, nil
}

func convertPVC(r *KokuMetricsConfigReconciler, kmCfg *kokumetricscfgv1alpha1.KokuMetricsConfig, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	ctx := context.Background()

	deployment := &appsv1.Deployment{}
	namespace := types.NamespacedName{
		Namespace: "koku-metrics-operator",
		Name:      "koku-metrics-controller-manager"}
	if err := r.Get(ctx, namespace, deployment); err != nil {
		return false, fmt.Errorf("unable to get deployment: %v", err)
	}
	deployCp := deployment.DeepCopy()

	i, vol, err := getVolume(deployCp.Spec.Template.Spec.Volumes, kmCfg)
	if err != nil {
		return false, err
	}

	if isMounted(*vol) && vol.PersistentVolumeClaim.ClaimName == pvc.Name {
		kmCfg.Status.Storage.VolumeMounted = true
		return false, nil
	}

	if err := getOrCreateVolume(r, pvc); err != nil {
		return false, fmt.Errorf("failed to get or create PVC: %v", err)
	}

	return mountVolume(r, deployCp, i, *vol, pvc.Name)
}

// +kubebuilder:rbac:groups=koku-metrics-cfg.openshift.io,resources=kokumetricsconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=koku-metrics-cfg.openshift.io,resources=kokumetricsconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=proxies;networks,verbs=get;list
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews;tokenreviews,verbs=create
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets;serviceaccounts,verbs=list;watch
// +kubebuilder:rbac:groups=core,namespace=koku-metrics-operator,resources=pods;services;services/finalizers;endpoints;persistentvolumeclaims;events;configmaps;secrets,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=apps,namespace=koku-metrics-operator,resources=deployments,verbs=get;list;patch;watch

// Reconcile Process the KokuMetricsConfig custom resource based on changes or requeue
func (r *KokuMetricsConfigReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	os.Setenv("TZ", "UTC")
	ctx := context.Background()
	log := r.Log.WithValues("KokuMetricsConfig", req.NamespacedName)

	// fetch the KokuMetricsConfig instance
	kmCfgOriginal := &kokumetricscfgv1alpha1.KokuMetricsConfig{}

	if err := r.Get(ctx, req.NamespacedName, kmCfgOriginal); err != nil {
		log.Info(fmt.Sprintf("unable to fetch KokuMetricsConfigCR: %v", err))
		// we'll ignore not-found errors, since they cannot be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	kmCfg := kmCfgOriginal.DeepCopy()
	log.Info("reconciling custom resource", "KokuMetricsConfig", kmCfg)

	// reflect the spec values into status
	ReflectSpec(r, kmCfg)

	pvcTemplate := kmCfg.Spec.VolumeClaimTemplate
	if pvcTemplate == nil {
		pvcTemplate = &archive.DefaultPVC
	}
	pvc := archive.MakeVolumeClaimTemplate(*pvcTemplate)
	mountEstablished, err := convertPVC(r, kmCfg, pvc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to mount on PVC: %v", err)
	}
	if mountEstablished {
		return ctrl.Result{}, nil
	}

	kmCfg.Status.Storage.PersistentVolumeClaim = pvcTemplate

	if strings.Contains(kmCfg.Status.Storage.VolumeType, "EmptyDir") {
		kmCfg.Status.Storage.VolumeMounted = false
		if err := r.Status().Update(ctx, kmCfg); err != nil {
			log.Error(err, "failed to update KokuMetricsConfig status")
		}
		return ctrl.Result{}, fmt.Errorf("PVC not mounted")
	}

	// set the cluster ID & return if there are errors
	if err := setClusterID(r, kmCfg); err != nil {
		log.Error(err, "failed to obtain clusterID")
		if err := r.Status().Update(ctx, kmCfg); err != nil {
			log.Error(err, "failed to update KokuMetricsConfig status")
		}
		return ctrl.Result{}, err
	}

	log.Info("using the following inputs", "KokuMetricsConfigConfig", kmCfg.Status)

	// set the Operator git commit and reflect it in the upload status & return if there are errors
	setOperatorCommit(r, kmCfg)

	authConfig := &crhchttp.AuthConfig{
		Log:            r.Log,
		ValidateCert:   *kmCfg.Status.Upload.ValidateCert,
		Authentication: kmCfg.Status.Authentication.AuthType,
		OperatorCommit: kmCfg.Status.OperatorCommit,
		ClusterID:      kmCfg.Status.ClusterID,
		Client:         r.Client,
	}

	// obtain credentials token/basic & return if there are authentication credential errors
	if err := setAuthentication(r, authConfig, kmCfg, req.NamespacedName); err != nil {
		if err := r.Status().Update(ctx, kmCfg); err != nil {
			log.Error(err, "failed to update KokuMetricsConfig status")
		}
		return ctrl.Result{}, err
	}

	// Check if source is defined and update the status to confirmed/created
	checkSource(r, authConfig, kmCfg)

	// Get or create the directory configuration
	log.Info("getting directory configuration")
	if dirCfg == nil || !dirCfg.Parent.Exists() {
		if err := dirCfg.GetDirectoryConfig(); err != nil {
			log.Error(err, "failed to get directory configuration")
			return ctrl.Result{}, err // without this directory, it is pointless to continue
		}
	}

	var result = ctrl.Result{RequeueAfter: time.Minute * 5}
	var errors []error

	// attempt to collect prometheus stats and create reports
	collectPromStats(r, kmCfg, dirCfg)

	packager := packageFiles(r, kmCfg, dirCfg)

	// attempt package and upload, if errors occur return
	if err := uploadFiles(r, authConfig, kmCfg, dirCfg, packager); err != nil {
		result = ctrl.Result{}
		errors = append(errors, err)
	}

	if err := r.Status().Update(ctx, kmCfg); err != nil {
		log.Error(err, "failed to update KokuMetricsConfig status")
		result = ctrl.Result{}
		errors = append(errors, err)
	}

	// Requeue for processing after 5 minutes
	return result, concatErrs(errors...)
}

// SetupWithManager Setup reconciliation with manager object
func (r *KokuMetricsConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kokumetricscfgv1alpha1.KokuMetricsConfig{}).
		Complete(r)
}

// concatErrs combines all the errors into one error
func concatErrs(errors ...error) error {
	var err error
	var errstrings []string
	for _, e := range errors {
		errstrings = append(errstrings, e.Error())
	}
	if len(errstrings) > 0 {
		err = fmt.Errorf(strings.Join(errstrings, "\n"))
	}
	return err
}
