// Package remote implements cluster remote load tests.
// ref. https://github.com/kubernetes/perf-tests
// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	eks_tester "github.com/aws/aws-k8s-tester/eks/tester"
	"github.com/aws/aws-k8s-tester/eksconfig"
	"github.com/aws/aws-k8s-tester/pkg/aws/cw"
	aws_ecr "github.com/aws/aws-k8s-tester/pkg/aws/ecr"
	aws_s3 "github.com/aws/aws-k8s-tester/pkg/aws/s3"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"github.com/aws/aws-k8s-tester/pkg/metrics"
	"github.com/aws/aws-k8s-tester/pkg/timeutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/exec"
)

// Config defines stresser configuration.
// ref. https://github.com/kubernetes/perf-tests
type Config struct {
	Logger    *zap.Logger
	Stopc     chan struct{}
	EKSConfig *eksconfig.Config
	K8SClient k8s_client.EKS
	S3API     s3iface.S3API
	CWAPI     cloudwatchiface.CloudWatchAPI
	ECRAPI    ecriface.ECRAPI
}

// TODO: use kubemark
// nodelease.NewController, kubemark.GetHollowKubeletConfig

var pkgName = reflect.TypeOf(tester{}).PkgPath()

func (ts *tester) Name() string { return pkgName }

func New(cfg Config) eks_tester.Tester {
	cfg.Logger.Info("creating tester", zap.String("tester", pkgName))
	return &tester{cfg: cfg}
}

type tester struct {
	cfg      Config
	ecrImage string
}

func (ts *tester) Create() (err error) {
	if !ts.cfg.EKSConfig.IsEnabledAddOnStresserRemote() {
		ts.cfg.Logger.Info("skipping tester.Create", zap.String("tester", pkgName))
		return nil
	}
	if ts.cfg.EKSConfig.AddOnStresserRemote.Created {
		ts.cfg.Logger.Info("skipping tester.Create", zap.String("tester", pkgName))
		return nil
	}

	ts.cfg.Logger.Info("starting tester.Create", zap.String("tester", pkgName))
	ts.cfg.EKSConfig.AddOnStresserRemote.Created = true
	ts.cfg.EKSConfig.Sync()
	createStart := time.Now()
	defer func() {
		createEnd := time.Now()
		ts.cfg.EKSConfig.AddOnStresserRemote.TimeFrameCreate = timeutil.NewTimeFrame(createStart, createEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	if ts.ecrImage, err = aws_ecr.Check(
		ts.cfg.Logger,
		ts.cfg.ECRAPI,
		ts.cfg.EKSConfig.AddOnStresserRemote.RepositoryAccountID,
		ts.cfg.EKSConfig.AddOnStresserRemote.RepositoryName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RepositoryImageTag,
	); err != nil {
		return err
	}
	if err = k8s_client.CreateNamespace(
		ts.cfg.Logger,
		ts.cfg.K8SClient.KubernetesClientSet(),
		ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
	); err != nil {
		return err
	}
	if err = ts.createServiceAccount(); err != nil {
		return err
	}
	if err = ts.createALBRBACClusterRole(); err != nil {
		return err
	}
	if err = ts.createALBRBACClusterRoleBinding(); err != nil {
		return err
	}
	if err = ts.createConfigMap(); err != nil {
		return err
	}

	if err = ts.createJob(); err != nil {
		return err
	}
	var pods []v1.Pod
	_, pods, err = k8s_client.WaitForJobCompletes(
		ts.cfg.Logger,
		ts.cfg.Stopc,
		ts.cfg.K8SClient,
		3*time.Minute,
		10*time.Second,
		3*time.Minute+time.Duration(ts.cfg.EKSConfig.AddOnStresserRemote.Completes)*30*time.Second,
		ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
		stresserJobName,
		ts.cfg.EKSConfig.AddOnStresserRemote.Completes,
	)
	if err != nil {
		return err
	}
	println()
	for _, item := range pods {
		fmt.Printf("Job Pod %q: %q\n", item.Name, item.Status.Phase)
	}
	println()

	if err = ts.checkResults(); err == nil {
		return err
	}
	if err = ts.publishResults(); err != nil {
		return err
	}

	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) Delete() error {
	if !ts.cfg.EKSConfig.IsEnabledAddOnStresserRemote() {
		ts.cfg.Logger.Info("skipping tester.Delete", zap.String("tester", pkgName))
		return nil
	}
	if !ts.cfg.EKSConfig.AddOnStresserRemote.Created {
		ts.cfg.Logger.Info("skipping tester.Delete", zap.String("tester", pkgName))
		return nil
	}

	ts.cfg.Logger.Info("starting tester.Delete", zap.String("tester", pkgName))
	deleteStart := time.Now()
	defer func() {
		deleteEnd := time.Now()
		ts.cfg.EKSConfig.AddOnStresserRemote.TimeFrameDelete = timeutil.NewTimeFrame(deleteStart, deleteEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	var errs []string

	if err := ts.deleteJob(); err != nil {
		errs = append(errs, err.Error())
	}
	time.Sleep(2 * time.Minute)

	if err := ts.deleteConfigMap(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ts.deleteALBRBACClusterRoleBinding(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ts.deleteALBRBACClusterRole(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ts.deleteServiceAccount(); err != nil {
		errs = append(errs, err.Error())
	}

	getAllArgs := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=" + ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
		"get",
		"all",
	}
	getAllCmd := strings.Join(getAllArgs, " ")

	if err := k8s_client.DeleteNamespaceAndWait(
		ts.cfg.Logger,
		ts.cfg.K8SClient.KubernetesClientSet(),
		ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
		k8s_client.DefaultNamespaceDeletionInterval,
		k8s_client.DefaultNamespaceDeletionTimeout,
		k8s_client.WithQueryFunc(func() {
			println()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			output, err := exec.New().CommandContext(ctx, getAllArgs[0], getAllArgs[1:]...).CombinedOutput()
			cancel()
			out := strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl get all' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", getAllCmd, out)
			}
		}),
		k8s_client.WithForceDelete(true),
	); err != nil {
		// TODO: error...
		// errs = append(errs, fmt.Sprintf("failed to delete stresser namespace (%v)", err))
		ts.cfg.Logger.Warn("failed to delete namespace", zap.Error(err))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}

	ts.cfg.EKSConfig.AddOnStresserRemote.Created = false
	return ts.cfg.EKSConfig.Sync()
}

const (
	stresserServiceAccountName          = "stresser-remote-service-account"
	stresserRBACRoleName                = "stresser-remote-rbac-role"
	stresserRBACClusterRoleBindingName  = "stresser-remote-rbac-role-binding"
	stresserKubeConfigConfigMapName     = "stresser-remote-kubeconfig-configmap"
	stresserKubeConfigConfigMapFileName = "stresser-remote-kubeconfig-configmap.yaml"
	stresserAppName                     = "stresser-remote-app"
	stresserJobName                     = "stresser-remote-job"
)

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) createServiceAccount() error {
	ts.cfg.Logger.Info("creating stresser ServiceAccount")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ServiceAccounts(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Create(
			ctx,
			&v1.ServiceAccount{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "ServiceAccount",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      stresserServiceAccountName,
					Namespace: ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": stresserAppName,
					},
				},
			},
			metav1.CreateOptions{},
		)
	cancel()
	if err != nil {
		return fmt.Errorf("failed to create stresser ServiceAccount (%v)", err)
	}

	ts.cfg.Logger.Info("created stresser ServiceAccount")
	return ts.cfg.EKSConfig.Sync()
}

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) deleteServiceAccount() error {
	ts.cfg.Logger.Info("deleting stresser ServiceAccount")
	foreground := metav1.DeletePropagationForeground
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ServiceAccounts(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Delete(
			ctx,
			stresserServiceAccountName,
			metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	cancel()
	if err != nil && !api_errors.IsNotFound(err) {
		ts.cfg.Logger.Warn("failed to delete", zap.Error(err))
		return fmt.Errorf("failed to delete stresser ServiceAccount (%v)", err)
	}
	ts.cfg.Logger.Info("deleted stresser ServiceAccount", zap.Error(err))

	return ts.cfg.EKSConfig.Sync()
}

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) createALBRBACClusterRole() error {
	ts.cfg.Logger.Info("creating stresser RBAC ClusterRole")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoles().
		Create(
			ctx,
			&rbacv1.ClusterRole{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "ClusterRole",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      stresserRBACRoleName,
					Namespace: "default",
					Labels: map[string]string{
						"app.kubernetes.io/name": stresserAppName,
					},
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{
							"*",
						},
						Resources: []string{
							"leases",
							"nodes",
							"pods",
							"secrets",
							"services",
							"namespaces",
							"stresser",
							"endpoints",
							"events",
							"ingresses",
							"ingresses/status",
							"services",
							"jobs",
							"cronjobs",
						},
						Verbs: []string{
							"create",
							"get",
							"list",
							"update",
							"watch",
							"patch",
						},
					},
				},
			},
			metav1.CreateOptions{},
		)
	cancel()
	if err != nil {
		return fmt.Errorf("failed to create stresser RBAC ClusterRole (%v)", err)
	}

	ts.cfg.Logger.Info("created stresser RBAC ClusterRole")
	return ts.cfg.EKSConfig.Sync()
}

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) deleteALBRBACClusterRole() error {
	ts.cfg.Logger.Info("deleting stresser RBAC ClusterRole")
	foreground := metav1.DeletePropagationForeground
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoles().
		Delete(
			ctx,
			stresserRBACRoleName,
			metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	cancel()
	if err != nil && !api_errors.IsNotFound(err) {
		ts.cfg.Logger.Warn("failed to delete", zap.Error(err))
		return fmt.Errorf("failed to delete stresser RBAC ClusterRole (%v)", err)
	}

	ts.cfg.Logger.Info("deleted stresser RBAC ClusterRole", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) createALBRBACClusterRoleBinding() error {
	ts.cfg.Logger.Info("creating stresser RBAC ClusterRoleBinding")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoleBindings().
		Create(
			ctx,
			&rbacv1.ClusterRoleBinding{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "ClusterRoleBinding",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      stresserRBACClusterRoleBindingName,
					Namespace: "default",
					Labels: map[string]string{
						"app.kubernetes.io/name": stresserAppName,
					},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     stresserRBACRoleName,
				},
				Subjects: []rbacv1.Subject{
					{
						APIGroup:  "",
						Kind:      "ServiceAccount",
						Name:      stresserServiceAccountName,
						Namespace: ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
					},
					{ // https://kubernetes.io/docs/reference/access-authn-authz/rbac/
						APIGroup: "rbac.authorization.k8s.io",
						Kind:     "User",
						Name:     "system:node",
					},
				},
			},
			metav1.CreateOptions{},
		)
	cancel()
	if err != nil {
		return fmt.Errorf("failed to create stresser RBAC ClusterRoleBinding (%v)", err)
	}

	ts.cfg.Logger.Info("created stresser RBAC ClusterRoleBinding")
	return ts.cfg.EKSConfig.Sync()
}

// ref. https://github.com/kubernetes/client-go/tree/master/examples/in-cluster-client-configuration
// ref. https://kubernetes.io/docs/reference/access-authn-authz/rbac/
func (ts *tester) deleteALBRBACClusterRoleBinding() error {
	ts.cfg.Logger.Info("deleting stresser RBAC ClusterRoleBinding")
	foreground := metav1.DeletePropagationForeground
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoleBindings().
		Delete(
			ctx,
			stresserRBACClusterRoleBindingName,
			metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	cancel()
	if err != nil && !api_errors.IsNotFound(err) {
		ts.cfg.Logger.Warn("failed to delete", zap.Error(err))
		return fmt.Errorf("failed to delete stresser RBAC ClusterRoleBinding (%v)", err)
	}

	ts.cfg.Logger.Info("deleted stresser RBAC ClusterRoleBinding", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) createConfigMap() error {
	ts.cfg.Logger.Info("creating config map")

	b, err := ioutil.ReadFile(ts.cfg.EKSConfig.KubeConfigPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err = ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ConfigMaps(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Create(
			ctx,
			&v1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      stresserKubeConfigConfigMapName,
					Namespace: ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
					Labels: map[string]string{
						"name": stresserKubeConfigConfigMapName,
					},
				},
				Data: map[string]string{
					stresserKubeConfigConfigMapFileName: string(b),
				},
			},
			metav1.CreateOptions{},
		)
	cancel()
	if err != nil {
		return err
	}

	ts.cfg.Logger.Info("created config map")
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) deleteConfigMap() error {
	ts.cfg.Logger.Info("deleting config map")
	foreground := metav1.DeletePropagationForeground
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ConfigMaps(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Delete(
			ctx,
			stresserKubeConfigConfigMapName,
			metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	cancel()
	if err != nil {
		return err
	}
	ts.cfg.Logger.Info("deleted config map")
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) createJob() (err error) {
	obj, b, err := ts.createObject()
	if err != nil {
		return err
	}

	ts.cfg.Logger.Info("creating Job",
		zap.String("name", stresserJobName),
		zap.String("object-size", humanize.Bytes(uint64(len(b)))),
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, err = ts.cfg.K8SClient.KubernetesClientSet().
		BatchV1().
		Jobs(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Create(ctx, &obj, metav1.CreateOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("failed to create Job (%v)", err)
	}

	ts.cfg.Logger.Info("created Job")
	return nil
}

func (ts *tester) createObject() (batchv1.Job, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	nss, err := ts.cfg.K8SClient.KubernetesClientSet().CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	cancel()
	if err != nil {
		ts.cfg.Logger.Warn("list namespaces failed", zap.Error(err))
		return batchv1.Job{}, "", err
	}
	ns := make([]string, 0, len(nss.Items))
	for _, nv := range nss.Items {
		ns = append(ns, nv.GetName())
	}

	// "/opt/"+stresserKubeConfigConfigMapFileName,
	// do not specify "kubeconfig", and use in-cluster config via "pkg/k8s-client"
	// otherwise, error "namespaces is forbidden: User "system:node:ip-192-168-84..."
	// ref. https://github.com/kubernetes/client-go/blob/master/examples/in-cluster-client-configuration/main.go
	testerCmd := fmt.Sprintf(`/aws-k8s-tester eks create stresser --partition=%s --region=%s --s3-bucket-name=%s --clients=%d --client-qps=%f --client-burst=%d --client-timeout=%s --object-size=%d --list-limit=%d --duration=%s --namespace-write=%s --namespaces-read=%s --requests-raw-writes-json-s3-dir=%s --requests-summary-writes-json-s3-dir=%s --requests-summary-writes-table-s3-dir=%s --requests-raw-reads-json-s3-dir=%s --requests-summary-reads-json-s3-dir=%s --requests-summary-reads-table-s3-dir=%s --writes-output-name-prefix=%s --reads-output-name-prefix=%s`,
		ts.cfg.EKSConfig.Partition,
		ts.cfg.EKSConfig.Region,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.Clients,
		ts.cfg.EKSConfig.ClientQPS,
		ts.cfg.EKSConfig.ClientBurst,
		ts.cfg.EKSConfig.ClientTimeout,
		ts.cfg.EKSConfig.AddOnStresserRemote.ObjectSize,
		ts.cfg.EKSConfig.AddOnStresserRemote.ListLimit,
		ts.cfg.EKSConfig.AddOnStresserRemote.Duration,
		ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
		strings.Join(ns, ","),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONS3Key),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONS3Key),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesTableS3Key),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONS3Key),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONS3Key),
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsTableS3Key),
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesOutputNamePrefix,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsOutputNamePrefix,
	)

	dirOrCreate := v1.HostPathDirectoryOrCreate
	podSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/name": stresserAppName,
			},
		},
		Spec: v1.PodSpec{
			ServiceAccountName: stresserServiceAccountName,

			// spec.template.spec.restartPolicy: Unsupported value: "Always": supported values: "OnFailure", "Never"
			RestartPolicy: v1.RestartPolicyOnFailure,
			// TODO: set resource limits
			Containers: []v1.Container{
				{
					Name:            stresserAppName,
					Image:           ts.ecrImage,
					ImagePullPolicy: v1.PullAlways,

					Command: []string{
						"/bin/sh",
						"-ec",
						testerCmd,
					},

					// grant access "/dev/kmsg"
					SecurityContext: &v1.SecurityContext{
						Privileged: aws.Bool(true),
					},

					// ref. https://kubernetes.io/docs/concepts/cluster-administration/logging/
					VolumeMounts: []v1.VolumeMount{
						{ // to execute
							Name:      stresserKubeConfigConfigMapName,
							MountPath: "/opt",
						},
						{ // to write
							Name:      "varlog",
							MountPath: "/var/log",
							ReadOnly:  false,
						},
					},
				},
			},

			// ref. https://kubernetes.io/docs/concepts/cluster-administration/logging/
			Volumes: []v1.Volume{
				{ // to execute
					Name: stresserKubeConfigConfigMapName,
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: v1.LocalObjectReference{
								Name: stresserKubeConfigConfigMapName,
							},
							DefaultMode: aws.Int32(0777),
						},
					},
				},
				{ // to write
					Name: "varlog",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/var/log",
							Type: &dirOrCreate,
						},
					},
				},
			},

			NodeSelector: map[string]string{
				// do not deploy in fake nodes, obviously
				"NodeType": "regular",
			},
		},
	}

	jobObj := batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      stresserJobName,
			Namespace: ts.cfg.EKSConfig.AddOnStresserRemote.Namespace,
		},
		Spec: batchv1.JobSpec{
			Completions: aws.Int32(int32(ts.cfg.EKSConfig.AddOnStresserRemote.Completes)),
			Parallelism: aws.Int32(int32(ts.cfg.EKSConfig.AddOnStresserRemote.Parallels)),
			Template:    podSpec,
			// TODO: 'TTLSecondsAfterFinished' is still alpha
			// https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/
		},
	}
	b, err := yaml.Marshal(jobObj)
	return jobObj, string(b), err
}

func (ts *tester) deleteJob() (err error) {
	foreground := metav1.DeletePropagationForeground
	ts.cfg.Logger.Info("deleting Job", zap.String("name", stresserJobName))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err = ts.cfg.
		K8SClient.KubernetesClientSet().
		BatchV1().
		Jobs(ts.cfg.EKSConfig.AddOnStresserRemote.Namespace).
		Delete(
			ctx,
			stresserJobName,
			metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	cancel()
	if err == nil {
		ts.cfg.Logger.Info("deleted Job", zap.String("name", stresserJobName))
	} else {
		ts.cfg.Logger.Warn("failed to delete Job", zap.Error(err))
	}
	return err
}

// 1. if previous summary exists, download and compare
// 2. upload new summary and overwrite the previous s3 key
func (ts *tester) checkResults() (err error) {
	curTS := time.Now().UTC().Format(time.RFC3339Nano)
	ts.cfg.Logger.Info("checking results", zap.String("timestamp", curTS))

	writesSummary := metrics.RequestsSummary{TestID: curTS}
	curWriteLatencies := make(metrics.Durations, 0, 20000)
	writesDirRaw := ""
	writesDirSummary := ""

	readsSummary := metrics.RequestsSummary{TestID: curTS}
	curReadLatencies := make(metrics.Durations, 0, 20000)
	readsDirRaw := ""
	readsDirSummary := ""

	writesDirRaw, err = aws_s3.DownloadDir(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONS3Key),
	)
	if err == nil {
		ts.cfg.Logger.Info("reading writes results raw",
			zap.String("writes-dir", writesDirRaw),
			zap.String("s3-dir", path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONS3Key)),
		)
		cnt := 0
		err = filepath.Walk(writesDirRaw, func(fpath string, info os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if info.IsDir() {
				return nil
			}
			cnt++
			switch {
			case strings.HasSuffix(fpath, "-writes-raw.json"):
				b, err := ioutil.ReadFile(fpath)
				if err != nil {
					return fmt.Errorf("failed to open %q (%v)", fpath, err)
				}
				var r metrics.Durations
				if err = json.Unmarshal(b, &r); err != nil {
					return fmt.Errorf("failed to unmarshal %q (%s, %v)", fpath, string(b), err)
				}
				curWriteLatencies = append(curWriteLatencies, r...)
			}
			return nil
		})
		if err != nil || cnt == 0 {
			ts.cfg.Logger.Warn("failed to read writes results", zap.Int("file-count", cnt), zap.Error(err))
			os.RemoveAll(writesDirRaw)
			writesDirRaw = ""
		}
	}
	writesDirSummary, err = aws_s3.DownloadDir(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONS3Key),
	)
	if err == nil {
		ts.cfg.Logger.Info("reading writes results summary",
			zap.String("writes-dir", writesDirSummary),
			zap.String("s3-dir", path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONS3Key)),
		)
		cnt := 0
		err = filepath.Walk(writesDirSummary, func(fpath string, info os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if info.IsDir() {
				return nil
			}
			cnt++
			switch {
			case strings.HasSuffix(fpath, "-writes-summary.json"):
				b, err := ioutil.ReadFile(fpath)
				if err != nil {
					return fmt.Errorf("failed to open %q (%v)", fpath, err)
				}
				var r metrics.RequestsSummary
				if err = json.Unmarshal(b, &r); err != nil {
					return fmt.Errorf("failed to unmarshal %q (%s, %v)", fpath, string(b), err)
				}
				writesSummary.SuccessTotal += r.SuccessTotal
				writesSummary.FailureTotal += r.FailureTotal
				if writesSummary.LatencyHistogram == nil || len(writesSummary.LatencyHistogram) == 0 {
					writesSummary.LatencyHistogram = r.LatencyHistogram
				} else {
					writesSummary.LatencyHistogram, err = metrics.MergeHistograms(writesSummary.LatencyHistogram, r.LatencyHistogram)
					if err != nil {
						return fmt.Errorf("failed to merge histograms (%v)", err)
					}
				}
			}
			return nil
		})
		if err != nil || cnt == 0 {
			ts.cfg.Logger.Warn("failed to read writes results", zap.Int("file-count", cnt), zap.Error(err))
			os.RemoveAll(writesDirSummary)
			writesDirSummary = ""
		}
	}

	readsDirRaw, err = aws_s3.DownloadDir(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONS3Key),
	)
	if err == nil {
		ts.cfg.Logger.Info("reading reads results raw",
			zap.String("reads-dir", readsDirRaw),
			zap.String("s3-dir", path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONS3Key)),
		)
		cnt := 0
		err = filepath.Walk(readsDirRaw, func(fpath string, info os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if info.IsDir() {
				return nil
			}
			cnt++
			switch {
			case strings.HasSuffix(fpath, "-reads-raw.json"):
				b, err := ioutil.ReadFile(fpath)
				if err != nil {
					return fmt.Errorf("failed to open %q (%v)", fpath, err)
				}
				var r metrics.Durations
				if err = json.Unmarshal(b, &r); err != nil {
					return fmt.Errorf("failed to unmarshal %q (%s, %v)", fpath, string(b), err)
				}
				curReadLatencies = append(curReadLatencies, r...)
			}
			return nil
		})
		if err != nil || cnt == 0 {
			ts.cfg.Logger.Warn("failed to read reads results", zap.Int("file-count", cnt), zap.Error(err))
			os.RemoveAll(readsDirRaw)
			readsDirRaw = ""
		}
	}
	readsDirSummary, err = aws_s3.DownloadDir(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONS3Key),
	)
	if err == nil {
		ts.cfg.Logger.Info("reading reads results summary",
			zap.String("reads-dir", readsDirSummary),
			zap.String("s3-dir", path.Dir(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONS3Key)),
		)
		cnt := 0
		err = filepath.Walk(readsDirSummary, func(fpath string, info os.FileInfo, werr error) error {
			if werr != nil {
				return werr
			}
			if info.IsDir() {
				return nil
			}
			cnt++
			switch {
			case strings.HasSuffix(fpath, "-reads-summary.json"):
				b, err := ioutil.ReadFile(fpath)
				if err != nil {
					return fmt.Errorf("failed to open %q (%v)", fpath, err)
				}
				var r metrics.RequestsSummary
				if err = json.Unmarshal(b, &r); err != nil {
					return fmt.Errorf("failed to unmarshal %q (%s, %v)", fpath, string(b), err)
				}
				readsSummary.SuccessTotal += r.SuccessTotal
				readsSummary.FailureTotal += r.FailureTotal
				if readsSummary.LatencyHistogram == nil || len(readsSummary.LatencyHistogram) == 0 {
					readsSummary.LatencyHistogram = r.LatencyHistogram
				} else {
					readsSummary.LatencyHistogram, err = metrics.MergeHistograms(readsSummary.LatencyHistogram, r.LatencyHistogram)
					if err != nil {
						return fmt.Errorf("failed to merge histograms (%v)", err)
					}
				}
			}
			return nil
		})
		if err != nil || cnt == 0 {
			ts.cfg.Logger.Warn("failed to read reads results", zap.Int("file-count", cnt), zap.Error(err))
			os.RemoveAll(readsDirSummary)
			readsDirSummary = ""
		}
	}

	sortStart := time.Now()
	ts.cfg.Logger.Info("sorting write latencies", zap.Int("data", len(curWriteLatencies)))
	sort.Sort(curWriteLatencies)
	ts.cfg.Logger.Info("sorted write latencies", zap.String("took", time.Since(sortStart).String()))
	writesSummary.LantencyP50 = curWriteLatencies.PickLantencyP50()
	writesSummary.LantencyP90 = curWriteLatencies.PickLantencyP90()
	writesSummary.LantencyP99 = curWriteLatencies.PickLantencyP99()
	writesSummary.LantencyP999 = curWriteLatencies.PickLantencyP999()
	writesSummary.LantencyP9999 = curWriteLatencies.PickLantencyP9999()
	ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites = writesSummary
	ts.cfg.EKSConfig.Sync()

	sortStart = time.Now()
	ts.cfg.Logger.Info("sorting read latencies", zap.Int("data", len(curReadLatencies)))
	sort.Sort(curReadLatencies)
	ts.cfg.Logger.Info("sorted read latencies", zap.String("took", time.Since(sortStart).String()))
	readsSummary.LantencyP50 = curReadLatencies.PickLantencyP50()
	readsSummary.LantencyP90 = curReadLatencies.PickLantencyP90()
	readsSummary.LantencyP99 = curReadLatencies.PickLantencyP99()
	readsSummary.LantencyP999 = curReadLatencies.PickLantencyP999()
	readsSummary.LantencyP9999 = curReadLatencies.PickLantencyP9999()
	ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads = readsSummary
	ts.cfg.EKSConfig.Sync()

	wb, err := json.Marshal(curWriteLatencies)
	if err != nil {
		ts.cfg.Logger.Warn("failed to encode JSON", zap.Error(err))
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONPath, wb, 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONPath,
	); err != nil {
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONPath, []byte(writesSummary.JSON()), 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONPath,
	); err != nil {
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesTablePath, []byte(writesSummary.Table()), 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesTableS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesTablePath,
	); err != nil {
		return err
	}
	fmt.Printf("\n\nRequestsSummaryWrites:\n%s\n", writesSummary.Table())

	rb, err := json.Marshal(curReadLatencies)
	if err != nil {
		ts.cfg.Logger.Warn("failed to encode JSON", zap.Error(err))
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONPath, rb, 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONPath,
	); err != nil {
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONPath, []byte(readsSummary.JSON()), 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONPath,
	); err != nil {
		return err
	}
	if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsTablePath, []byte(readsSummary.Table()), 0600); err != nil {
		ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsTableS3Key,
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsTablePath,
	); err != nil {
		return err
	}
	fmt.Printf("\n\nRequestsSummaryReads:\n%s\n", readsSummary.Table())

	s3Objects := make([]*s3.Object, 0)
	if ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareS3Dir != "" {
		s3Objects, err = aws_s3.ListInDescendingLastModified(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			path.Clean(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareS3Dir)+"/",
		)
	}
	canCompare := len(s3Objects) > 0 && err == nil
	if canCompare {
		reqSummaryS3Key := aws.StringValue(s3Objects[0].Key)
		durRawS3Key := path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareS3Dir, path.Base(reqSummaryS3Key))

		var prevSummary metrics.RequestsSummary
		prevSummary, err = metrics.DownloadRequestsSummaryFromS3(ts.cfg.Logger, ts.cfg.S3API, ts.cfg.EKSConfig.S3BucketName, reqSummaryS3Key)
		if err != nil {
			ts.cfg.Logger.Warn("failed to download results", zap.Error(err))
			return err
		}
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompare, err = metrics.CompareRequestsSummary(prevSummary, ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites)
		if err != nil {
			ts.cfg.Logger.Warn("failed to compare results", zap.Error(err))
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompare.JSON()), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareJSONS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareJSONPath,
		); err != nil {
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompare.Table()), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareTableS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareTablePath,
		); err != nil {
			return err
		}
		fmt.Printf("\n\nRequestsSummaryWritesCompare:\n%s\n", ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompare.Table())

		var prevDurations metrics.Durations
		prevDurations, err = metrics.DownloadDurationsFromS3(ts.cfg.Logger, ts.cfg.S3API, ts.cfg.EKSConfig.S3BucketName, durRawS3Key)
		if err != nil {
			ts.cfg.Logger.Warn("failed to download results", zap.Error(err))
			return err
		}
		prevDurationsWithLabels := metrics.LabelDurations(prevDurations, prevSummary.TestID)
		curDurationsWithLabels := metrics.LabelDurations(curWriteLatencies, ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.TestID)
		allDurationsWithLabels := append(prevDurationsWithLabels, curDurationsWithLabels...)
		sortStart := time.Now()
		ts.cfg.Logger.Info("sorting before and after durations with label",
			zap.Int("before-data-points", len(prevDurationsWithLabels)),
			zap.Int("after-data-points", len(curDurationsWithLabels)),
			zap.Int("total-points", len(allDurationsWithLabels)),
		)
		sort.Sort(allDurationsWithLabels)
		ts.cfg.Logger.Info("sorted before and after durations with label",
			zap.Int("before-data-points", len(prevDurationsWithLabels)),
			zap.Int("after-data-points", len(curDurationsWithLabels)),
			zap.Int("total-points", len(allDurationsWithLabels)),
			zap.String("took", time.Since(sortStart).String()),
		)
		allDataJSON, err := json.Marshal(allDurationsWithLabels)
		if err != nil {
			ts.cfg.Logger.Warn("failed to marshal results", zap.Error(err))
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllJSONPath, []byte(allDataJSON), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllJSONS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllJSONPath,
		); err != nil {
			return err
		}
		if err = allDurationsWithLabels.CSV(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllCSVPath); err != nil {
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllCSVS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareAllCSVPath,
		); err != nil {
			return err
		}
	} else {
		ts.cfg.Logger.Warn("previous writes summary not found; skipping comparison", zap.Error(err))
	}
	ts.cfg.Logger.Info("uploading new writes summary to s3 bucket to overwrite the previous")
	if fileutil.Exist(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONPath) {
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesCompareS3Dir, curTS),
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawWritesJSONPath,
		); err != nil {
			return err
		}
	}
	if fileutil.Exist(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONPath) {
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesCompareS3Dir, curTS),
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWritesJSONPath,
		); err != nil {
			return err
		}
	}

	s3Objects = make([]*s3.Object, 0)
	if ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareS3Dir != "" {
		s3Objects, err = aws_s3.ListInDescendingLastModified(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			path.Clean(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareS3Dir)+"/",
		)
	}
	canCompare = len(s3Objects) > 0 && err == nil
	if canCompare {
		reqSummaryS3Key := aws.StringValue(s3Objects[0].Key)
		durRawS3Key := path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareS3Dir, path.Base(reqSummaryS3Key))

		var prevSummary metrics.RequestsSummary
		prevSummary, err = metrics.DownloadRequestsSummaryFromS3(ts.cfg.Logger, ts.cfg.S3API, ts.cfg.EKSConfig.S3BucketName, reqSummaryS3Key)
		if err != nil {
			ts.cfg.Logger.Warn("failed to download results", zap.Error(err))
			return err
		}
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompare, err = metrics.CompareRequestsSummary(prevSummary, ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads)
		if err != nil {
			ts.cfg.Logger.Warn("failed to compare results", zap.Error(err))
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompare.JSON()), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareJSONS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareJSONPath,
		); err != nil {
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompare.Table()), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareTableS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareTablePath,
		); err != nil {
			return err
		}
		fmt.Printf("\n\nRequestsSummaryReadsCompare:\n%s\n", ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompare.Table())

		var prevDurations metrics.Durations
		prevDurations, err = metrics.DownloadDurationsFromS3(ts.cfg.Logger, ts.cfg.S3API, ts.cfg.EKSConfig.S3BucketName, durRawS3Key)
		if err != nil {
			ts.cfg.Logger.Warn("failed to download results", zap.Error(err))
			return err
		}
		prevDurationsWithLabels := metrics.LabelDurations(prevDurations, prevSummary.TestID)
		curDurationsWithLabels := metrics.LabelDurations(curWriteLatencies, ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.TestID)
		allDurationsWithLabels := append(prevDurationsWithLabels, curDurationsWithLabels...)
		sortStart := time.Now()
		ts.cfg.Logger.Info("sorting before and after durations with label",
			zap.Int("before-data-points", len(prevDurationsWithLabels)),
			zap.Int("after-data-points", len(curDurationsWithLabels)),
			zap.Int("total-points", len(allDurationsWithLabels)),
		)
		sort.Sort(allDurationsWithLabels)
		ts.cfg.Logger.Info("sorted before and after durations with label",
			zap.Int("before-data-points", len(prevDurationsWithLabels)),
			zap.Int("after-data-points", len(curDurationsWithLabels)),
			zap.Int("total-points", len(allDurationsWithLabels)),
			zap.String("took", time.Since(sortStart).String()),
		)
		allDataJSON, err := json.Marshal(allDurationsWithLabels)
		if err != nil {
			ts.cfg.Logger.Warn("failed to marshal results", zap.Error(err))
			return err
		}
		if err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllJSONPath, []byte(allDataJSON), 0600); err != nil {
			ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllJSONS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllJSONPath,
		); err != nil {
			return err
		}
		if err = allDurationsWithLabels.CSV(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllCSVPath); err != nil {
			return err
		}
		if err = aws_s3.Upload(
			ts.cfg.Logger,
			ts.cfg.S3API,
			ts.cfg.EKSConfig.S3BucketName,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllCSVS3Key,
			ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareAllCSVPath,
		); err != nil {
			return err
		}
	} else {
		ts.cfg.Logger.Warn("previous writes summary not found; skipping comparison", zap.Error(err))
	}
	ts.cfg.Logger.Info("uploading new reads summary to s3 bucket to overwrite the previous")
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsCompareS3Dir, curTS),
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsRawReadsJSONPath,
	); err != nil {
		return err
	}
	if err = aws_s3.Upload(
		ts.cfg.Logger,
		ts.cfg.S3API,
		ts.cfg.EKSConfig.S3BucketName,
		path.Join(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsCompareS3Dir, curTS),
		ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReadsJSONPath,
	); err != nil {
		return err
	}

	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) publishResults() (err error) {
	tv := aws.Time(time.Now().UTC())
	datums := make([]*cloudwatch.MetricDatum, 0)
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-writes-latency-p50"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.LantencyP50.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-writes-latency-p90"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.LantencyP90.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-writes-latency-p99"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.LantencyP99.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-writes-latency-p999"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.LantencyP999.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-writes-latency-p9999"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryWrites.LantencyP9999.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-reads-latency-p50"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.LantencyP50.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-reads-latency-p90"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.LantencyP90.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-reads-latency-p99"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.LantencyP99.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-reads-latency-p999"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.LantencyP999.Milliseconds())),
	})
	datums = append(datums, &cloudwatch.MetricDatum{
		Timestamp:  tv,
		MetricName: aws.String("add-on-stresser-remote-reads-latency-p9999"),
		Unit:       aws.String(cloudwatch.StandardUnitMilliseconds),
		Value:      aws.Float64(float64(ts.cfg.EKSConfig.AddOnStresserRemote.RequestsSummaryReads.LantencyP9999.Milliseconds())),
	})
	return cw.PutData(ts.cfg.Logger, ts.cfg.CWAPI, ts.cfg.EKSConfig.CWNamespace, 20, datums...)
}
