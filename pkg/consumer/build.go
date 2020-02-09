package consumer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v29/github"
	"golang.org/x/xerrors"
	"gopkg.in/src-d/go-git.v4"
	gitConfig "gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	gogitHttp "gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	goyaml "gopkg.in/yaml.v2"
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
	artifactHost           = "storage-hl-svc.default.svc.cluster.local:9000"
	artifactBucket         = "build-artifacts"
	storageTokenSecretName = "storage-token"

	labelKeyJobId = "k8s-cluster-maintenance-bot.f110.dev/job-id"

	authorName  = "bot"
	authorEmail = "fmhrit+bot@gmail.com"
)

var (
	errBuildFailure = xerrors.New("build failed")
)

var letters = "abcdefghijklmnopqrstuvwxyz1234567890"

type BazelBuild struct {
	Namespace      string
	Rule           *config.Rule
	AppId          int64
	InstallationId int64

	transport  *ghinstallation.Transport
	url        string
	workingDir string
	debug      bool
}

func errorLog(err error) {
	fmt.Fprintf(os.Stderr, "%+v\n", err)
}

func NewBuildConsumer(namespace string, rule *config.Rule, appId, installationId int64, privateKeyFile string, debug bool) (*BazelBuild, error) {
	var u string
	if rule.Private {
		u = fmt.Sprintf("git@github.com:%s.git", rule.Repo)
	} else {
		u = fmt.Sprintf("https://github.com/%s.git", rule.Repo)
	}

	t, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, appId, installationId, privateKeyFile)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	return &BazelBuild{Namespace: namespace, Rule: rule, AppId: appId, InstallationId: installationId, debug: debug, url: u, transport: t}, nil
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
	defer func() {
		if err := b.cleanup(client, buildId); err != nil {
			errorLog(err)
			return
		}
	}()

	err = b.buildRepository(client, buildId)
	if err != nil && err != errBuildFailure {
		errorLog(err)
		return
	}

	if b.Rule.PostProcess != nil {
		if err := b.postProcess(buildId); err != nil {
			errorLog(err)
			return
		}
	}
}

func (b *BazelBuild) cleanup(client *kubernetes.Clientset, buildId string) error {
	if b.debug {
		return nil
	}

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

	failed := false
Watch:
	for e := range watchCh.ResultChan() {
		switch e.Type {
		case watch.Modified:
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				break Watch
			case corev1.PodFailed:
				failed = true
				break Watch
			}
		}
	}

	if failed {
		return errBuildFailure
	}

	return nil
}

func (b *BazelBuild) postProcess(buildId string) error {
	artifactDir, err := b.downloadArtifact(b.Rule.Name, buildId)
	if artifactDir != "" {
		defer os.RemoveAll(artifactDir)
	}
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	s := strings.SplitN(b.Rule.PostProcess.Repo, "/", 2)
	r, err := newGitRepo(b.transport, s[0], s[1], b.Rule.PostProcess.Image)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	defer r.Close()

	artifactPath := filepath.Join(artifactDir, filepath.Base(b.Rule.Artifacts[0]))
	if err := r.UpdateKustomization(b.Rule.Name, artifactPath, b.Rule.PostProcess.Paths); err != nil {
		return xerrors.Errorf(": %v", err)
	}

	return nil
}

func (b *BazelBuild) downloadArtifact(buildName, buildId string) (string, error) {
	cfg := &aws.Config{
		Endpoint:         aws.String(artifactHost),
		Region:           aws.String("us-east-1"),
		DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
		Credentials:      credentials.NewEnvCredentials(),
	}
	sess := session.Must(session.NewSession(cfg))
	s3Client := s3manager.NewDownloaderWithClient(s3.New(sess))

	tmpFile, err := ioutil.TempFile("", "")
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = s3Client.Download(tmpFile, &s3.GetObjectInput{
		Bucket: aws.String(artifactBucket),
		Key:    aws.String(fmt.Sprintf("%s-%s.tar", buildName, buildId)),
	})
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	tmpFile.Seek(0, 0)
	t := tar.NewReader(tmpFile)
	for {
		hdr, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", xerrors.Errorf(": %v", err)
		}
		f, err := os.Create(filepath.Join(dir, hdr.Name))
		if err != nil {
			return "", xerrors.Errorf(": %v", err)
		}
		if _, err := io.Copy(f, t); err != nil {
			return "", xerrors.Errorf(": %v", err)
		}
	}

	return dir, nil
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
					WorkingDir: "/work",
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

type gitRepo struct {
	dir      string
	owner    string
	repoName string
	image    string

	repo      *git.Repository
	transport *ghinstallation.Transport
}

func newGitRepo(transport *ghinstallation.Transport, owner, repo, image string) (*gitRepo, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	t, err := transport.Token(context.Background())
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	u := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	log.Printf("git clone %s", u)
	r, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:   u,
		Depth: 1,
		Auth:  &gogitHttp.BasicAuth{Username: "octocast", Password: t},
	})
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	log.Printf("New git repo: %s/%s in %s with image name: %s", owner, repo, dir, image)
	return &gitRepo{dir: dir, owner: owner, repoName: repo, image: image, repo: r, transport: transport}, nil
}

func (g *gitRepo) switchBranch() (string, *git.Worktree, error) {
	branchName := fmt.Sprintf("update-kustomization-%d", time.Now().Unix())

	masterRef, err := g.repo.Reference("refs/remotes/origin/master", true)
	if err != nil {
		return "", nil, err
	}

	ref := plumbing.NewHashReference(plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", branchName)), masterRef.Hash())
	if err := g.repo.Storer.SetReference(ref); err != nil {
		return "", nil, err
	}

	tree, err := g.repo.Worktree()
	if err != nil {
		return "", nil, err
	}
	if err := tree.Checkout(&git.CheckoutOptions{Branch: ref.Name()}); err != nil {
		return "", nil, err
	}

	return branchName, tree, nil
}

func (g *gitRepo) commit(tree *git.Worktree, path string) error {
	if _, err := tree.Add(path); err != nil {
		return err
	}
	st, err := tree.Status()
	if err != nil {
		return err
	}
	if st.IsClean() {
		return errors.New("changeset is empty")
	}
	_, err = tree.Commit(fmt.Sprintf("Update %s", path), &git.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return err
	}

	return nil
}

func (g *gitRepo) push(branchName string) error {
	token, err := g.transport.Token(context.Background())
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	refSpec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName)
	log.Printf("git push origin %s with %s", refSpec, token)
	return g.repo.Push(&git.PushOptions{
		Auth:       &gogitHttp.BasicAuth{Username: "octocat", Password: token},
		RemoteName: "origin",
		RefSpecs:   []gitConfig.RefSpec{gitConfig.RefSpec(refSpec)},
	})
}

func (g *gitRepo) createPullRequest(name, branch string, editedFiles []string) error {
	client := github.NewClient(&http.Client{Transport: g.transport})

	desc := "Change file(s):\n"
	for _, v := range editedFiles {
		desc += v + "\n"
	}
	_, _, err := client.PullRequests.Create(context.Background(), g.owner, g.repoName, &github.NewPullRequest{
		Title: github.String(fmt.Sprintf("Update %s", name)),
		Body:  github.String(desc),
		Base:  github.String("master"),
		Head:  github.String(branch),
	})

	return err
}

func (g *gitRepo) UpdateKustomization(name, artifactPath string, paths []string) error {
	buf, err := ioutil.ReadFile(artifactPath)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	if !bytes.HasPrefix(buf, []byte("sha256:")) {
		return xerrors.New("artifact file does not contain an image hash")
	}
	newImageHash := strings.TrimSuffix(string(buf), "\n")

	branchName, tree, err := g.switchBranch()
	if err != nil {
		return err
	}

	editFiles := make([]string, 0)
	for _, in := range paths {
		absPath := filepath.Join(g.dir, in)
		log.Printf("Read: %s", absPath)
		b, err := ioutil.ReadFile(absPath)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return errors.New("file is empty")
		}

		k := make(map[string]interface{})
		if err := goyaml.Unmarshal(b, k); err != nil {
			return err
		}
		log.Printf("File body: %v", k)

		changed := false
		if v, ok := k["images"]; ok {
			value := v.([]interface{})
			for _, i := range value {
				image := i.(map[interface{}]interface{})
				if n, ok := image["name"]; ok {
					name := n.(string)
					log.Printf("Found image: %s", name)
					if name == g.image {
						image["digest"] = newImageHash
						editFiles = append(editFiles, in)
						changed = true
					}
				}
			}
		}

		if changed {
			outBuf, err := goyaml.Marshal(k)
			if err != nil {
				return err
			}
			log.Printf("Write: %s", absPath)
			if err := ioutil.WriteFile(absPath, outBuf, 0644); err != nil {
				return err
			}

			if err := g.commit(tree, in); err != nil {
				return err
			}
		}
	}

	if len(editFiles) == 0 {
		log.Print("Skip creating a pull request because not have any change")
		return nil
	}

	if err := g.push(branchName); err != nil {
		return err
	}

	if err := g.createPullRequest(name, branchName, editFiles); err != nil {
		return err
	}

	log.Print("Success create a pull request")
	return nil
}

func (g *gitRepo) Close() {
	os.RemoveAll(g.dir)
}
