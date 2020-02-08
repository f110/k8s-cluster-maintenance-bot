package consumer

import (
	"fmt"
	"math/rand"
	"os"
	"time"

	"golang.org/x/xerrors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/f110/k8s-cluster-maintenance-bot/pkg/config"
)

const (
	builderServiceAccount  = "build"
	buildSidecarImage      = "registry.f110.dev/k8s-cluster-maintenance-bot/sidecar"
	bazelImage             = "l.gcr.io/google/bazel:2.0.0"
	artifactHost           = "storage-hl.svc.default.svc.cluster.local:9000"
	artifactBucket         = "build-artifacts"
	storageTokenSecretName = "storage-token"

	labelKeyJobId = "k8s-cluster-maintenance-bot.f110.dev/job-id"
)

var letters = "abcdefghijklmnopqrstuvwxyz1234567890"

type BazelBuild struct {
	Namespace string
	Rule      *config.Rule

	url        string
	workingDir string
}

func errorLog(err error) {
	fmt.Fprintf(os.Stderr, "%+v\n", err)
}

func NewBuildConsumer(namespace string, rule *config.Rule) *BazelBuild {
	var u string
	if rule.Private {
		u = fmt.Sprintf("git@github.com:%s.git", rule.Repo)
	} else {
		u = fmt.Sprintf("https://github.com/%s.git", rule.Repo)
	}

	return &BazelBuild{Namespace: namespace, Rule: rule, url: u}
}

func (b *BazelBuild) Build(_ interface{}) {
	conf, err := rest.InClusterConfig()
	if err != nil {
		errorLog(err)
		return
	}
	client, err := kubernetes.NewForConfig(conf)
	if err != nil {
		errorLog(err)
		return
	}

	buildId := newBuildId()
	if err := b.buildRepository(client, buildId); err != nil {
		errorLog(err)
		return
	}

	if err := b.cleanup(client, buildId); err != nil {
		errorLog(err)
		return
	}
}

func (b *BazelBuild) cleanup(client *kubernetes.Clientset, buildId string) error {
	podList, err := client.CoreV1().Pods(b.Namespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labelKeyJobId, buildId),
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	for _, v := range podList.Items {
		err := client.CoreV1().Pods(b.Namespace).Delete(v.Name, nil)
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
	}

	return nil
}

func (b *BazelBuild) buildRepository(client *kubernetes.Clientset, buildId string) error {
	buildPod := b.buildPod(buildId)
	_, err := client.CoreV1().Pods(b.Namespace).Create(buildPod)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	watchCh, err := client.CoreV1().Pods(b.Namespace).Watch(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", buildPod.Name),
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	defer watchCh.Stop()

Watch:
	for e := range watchCh.ResultChan() {
		switch e.Type {
		case watch.Modified:
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if pod.Status.Phase != corev1.PodSucceeded {
				continue
			}

			break Watch
		}
	}

	return nil
}

func (b *BazelBuild) buildPod(buildId string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", b.Rule.Name, buildId),
			Namespace: b.Namespace,
			Labels: map[string]string{
				labelKeyJobId: buildId,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: builderServiceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{
				{
					Name:  "pre-process",
					Image: buildSidecarImage,
					Args:  []string{"--action=clone", "--work-dir=/work", fmt.Sprintf("--url=%s", b.url)},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workdir", MountPath: "/work"},
					},
				},
			},
			HostAliases: []corev1.HostAlias{
				{
					Hostnames: []string{"registry.f110.dev", "registry.storage.x.f110.dev"},
					IP:        "192.168.100.132",
				},
			},
			Containers: []corev1.Container{
				{
					Name:       "main",
					Image:      bazelImage,
					Args:       []string{"--output_user_root=/out", "run", b.Rule.Target},
					WorkingDir: "/work",
					Env: []corev1.EnvVar{
						{Name: "DOCKER_CONFIG", Value: "/home/bazel/.docker"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workdir", MountPath: "/work"},
						{Name: "outdir", MountPath: "/out"},
						{
							Name:      "docker-config",
							MountPath: "/home/bazel/.docker/config.json",
							SubPath:   ".dockerconfigjson",
						},
					},
				},
				{
					Name:  "post-process",
					Image: buildSidecarImage,
					Args: []string{
						"--action=wait",
						fmt.Sprintf("--artifact-host=%s", artifactHost),
						fmt.Sprintf("--artifact-bucket=%s", artifactBucket),
						fmt.Sprintf("--artifact-path=%s", b.Rule.Artifacts[0]),
					},
					Env: []corev1.EnvVar{
						{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.name",
							},
						}},
						{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.namespace",
							},
						}},
						{Name: "AWS_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: storageTokenSecretName,
								},
								Key: "accesskey",
							},
						}},
						{Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: storageTokenSecretName,
								},
								Key: "secretkey",
							},
						}},
						{Name: "JOB_NAME", Value: b.Rule.Name},
						{Name: "JOB_ID", Value: buildId},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workdir", MountPath: "/work"},
						{Name: "outdir", MountPath: "/out"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workdir",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "outdir",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "docker-config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "docker-config",
						},
					},
				},
			},
		},
	}
}

func newBuildId() string {
	buf := make([]byte, 8)

	rand.Seed(time.Now().UnixNano())
	for i := range buf {
		buf[i] = letters[rand.Intn(len(letters))]
	}

	return string(buf)
}
