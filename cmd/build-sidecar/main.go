package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/spf13/pflag"
	"golang.org/x/xerrors"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	ActionClone             = "clone"
	ActionWait              = "wait"
	ActionDownloadArtifacts = "download-artifacts"

	MainProcessContainerName = "main"

	ContainerImage = "quay.io/f110/k8s-cluster-maintenance-bot-build-sidecar"
)

func actionClone(dir, repo, commit string) error {
	r, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL:   repo,
		Depth: 1,
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	if commit != "" {
		tree, err := r.Worktree()
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
		if err := tree.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(commit)}); err != nil {
			return xerrors.Errorf(": %v", err)
		}
	}

	return nil
}

func actionWait(artifactHost, artifactBucket, artifactPath string) error {
	conf, err := rest.InClusterConfig()
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	client, err := kubernetes.NewForConfig(conf)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	w, err := client.CoreV1().Pods(os.Getenv("POD_NAMESPACE")).Watch(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", os.Getenv("POD_NAME")),
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
Watch:
	for e := range w.ResultChan() {
		switch e.Type {
		case watch.Modified:
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				return xerrors.New("failure type assert to corev1.Pod")
			}

			for _, v := range pod.Status.ContainerStatuses {
				if v.Name != MainProcessContainerName {
					continue
				}

				if v.Ready == true {
					continue
				}
				if v.State.Terminated == nil {
					continue
				}
				if v.State.Terminated.Reason != "Completed" {
					return xerrors.Errorf("main container is terminated by unexpected reason: %s", v.State.Terminated.Reason)
				}

				break Watch
			}
		case watch.Error:
			return nil
		}
	}
	w.Stop()

	if artifactPath != "" {
		cfg := &aws.Config{
			Endpoint:         aws.String(artifactHost),
			Region:           aws.String("us-east-1"),
			DisableSSL:       aws.Bool(true),
			S3ForcePathStyle: aws.Bool(true),
			Credentials:      credentials.NewEnvCredentials(),
		}
		sess := session.Must(session.NewSession(cfg))
		s3Client := s3manager.NewUploaderWithClient(s3.New(sess))

		buf := new(bytes.Buffer)
		if s, err := os.Stat(artifactPath); os.IsNotExist(err) {
			return xerrors.Errorf(": %v", err)
		} else if !s.IsDir() {
			t := tar.NewWriter(buf)
			hdr := &tar.Header{
				Name: fmt.Sprintf("./%s", filepath.Base(artifactPath)),
				Mode: 0644,
				Size: s.Size(),
			}
			if err := t.WriteHeader(hdr); err != nil {
				return xerrors.Errorf(": %v", err)
			}
			f, err := ioutil.ReadFile(artifactPath)
			if err != nil {
				return xerrors.Errorf(": %v", err)
			}
			if _, err := t.Write(f); err != nil {
				return xerrors.Errorf(": %v", err)
			}
			if err := t.Close(); err != nil {
				return xerrors.Errorf(": %v", err)
			}
		}
		_, err = s3Client.Upload(&s3manager.UploadInput{
			Bucket: aws.String(artifactBucket),
			Key:    aws.String(fmt.Sprintf("%s-%s.tar", os.Getenv("JOB_NAME"), os.Getenv("JOB_ID"))),
			Body:   buf,
		})

		return err
	}

	pod, err := client.CoreV1().Pods(os.Getenv("POD_NAMESPACE")).Get(os.Getenv("POD_NAME"), metav1.GetOptions{})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}
	stillRunning := false
	for _, v := range pod.Status.ContainerStatuses {
		if v.Name == "main" {
			continue
		}
		if strings.HasPrefix(v.Image, ContainerImage) {
			continue
		}

		if v.State.Running != nil {
			stillRunning = true
		}
	}
	if stillRunning {
		log.Print("Force shutdown")
		if err := client.CoreV1().Pods(os.Getenv("POD_NAMESPACE")).Delete(os.Getenv("POD_NAME"), &metav1.DeleteOptions{}); err != nil {
			return xerrors.Errorf(": %v", err)
		}
	}

	return nil
}

func actionDownloadArtifacts(artifactHost, artifactBucket, artifactPath string) error {
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
		return xerrors.Errorf(": %v", err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = s3Client.Download(tmpFile, &s3.GetObjectInput{
		Bucket: aws.String(artifactBucket),
		Key:    aws.String(fmt.Sprintf("%s-%s", os.Getenv("JOB_NAME"), os.Getenv("JOB_ID"))),
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	if err := os.MkdirAll(artifactPath, 0755); err != nil {
		return xerrors.Errorf(": %v", err)
	}
	tmpFile.Seek(0, 0)
	t := tar.NewReader(tmpFile)
	for {
		hdr, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
		f, err := os.Create(filepath.Join(artifactPath, hdr.Name))
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
		if _, err := io.Copy(f, t); err != nil {
			return xerrors.Errorf(": %v", err)
		}
	}

	return nil
}

func buildSidecar(args []string) error {
	action := ""
	repo := ""
	commit := ""
	workingDir := ""
	artifactHost := ""
	artifactBucket := ""
	artifactPath := ""
	fs := pflag.NewFlagSet("build-sidecar", pflag.ContinueOnError)
	fs.StringVarP(&action, "action", "a", action, "Action")
	fs.StringVarP(&workingDir, "work-dir", "w", workingDir, "Working directory")
	fs.StringVar(&repo, "url", repo, "Repository url (e.g. git@github.com:octocat/example.git)")
	fs.StringVarP(&commit, "commit", "b", "", "Specify commit")
	fs.StringVar(&artifactHost, "artifact-host", artifactHost, "Artifact storage endpoint")
	fs.StringVar(&artifactBucket, "artifact-bucket", artifactBucket, "Artifact storage bucket name")
	fs.StringVar(&artifactPath, "artifact-path", artifactPath, "File path for storing")
	if err := fs.Parse(args); err != nil {
		return xerrors.Errorf(": %v", err)
	}

	switch action {
	case ActionClone:
		return actionClone(workingDir, repo, commit)
	case ActionWait:
		return actionWait(artifactHost, artifactBucket, artifactPath)
	case ActionDownloadArtifacts:
		return actionDownloadArtifacts(artifactHost, artifactBucket, artifactPath)
	default:
		return xerrors.Errorf("unknown action: %v", action)
	}
}

func main() {
	if err := buildSidecar(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}
