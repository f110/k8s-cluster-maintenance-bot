package consumer

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v29/github"
	"github.com/sourcegraph/go-diff/diff"
	"golang.org/x/xerrors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/f110/k8s-cluster-maintenance-bot/pkg/config"
)

const (
	dnscontrolBuildRule    = ".bot/dnscontrol.yaml"
	defaultDNSControlImage = "registry.f110.dev/dnscontrol/dnscontrol"
)

var prMergedMessageRe = regexp.MustCompile(`^Merge pull request #(\d+) from`)

type DNSControlConsumer struct {
	Namespace   string
	HostAliases []config.HostAlias

	transport *ghinstallation.Transport
	debug     bool
}

func NewDNSControlConsumer(namespace string, conf *config.Config, debug bool) (*DNSControlConsumer, error) {
	t, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, conf.GitHubAppId, conf.GitHubInstallationId, conf.GitHubAppPrivateKeyFile)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	return &DNSControlConsumer{
		Namespace:   namespace,
		HostAliases: conf.HostAliases,
		transport:   t,
		debug:       debug,
	}, nil
}

func (c *DNSControlConsumer) Dispatch(e interface{}) {
	conf, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		clientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			errorLog(err)
		}
		conf = clientConfig
	}
	client, err := kubernetes.NewForConfig(conf)
	if err != nil {
		errorLog(err)
		return
	}

	switch v := e.(type) {
	case *github.PushEvent:
		c.dispatchPushEvent(v, client)
	case *github.PullRequestEvent:
		c.dispatchPullRequestEvent(v, client)
	default:
		log.Print("Unknown event")
	}

}

func (c *DNSControlConsumer) dispatchPushEvent(event *github.PushEvent, client *kubernetes.Clientset) {
	ctx := &dnsControlContext{eventContext: NewEventContextFromPushEvent(event)}
	if err := c.fetchRuleFile(ctx); err != nil {
		errorLog(err)
		return
	}

	s := strings.SplitN(event.GetRef(), "/", 3)
	branch := s[2]
	switch branch {
	case ctx.Rule.MasterBranch:
	default:
		return
	}

	if event.GetHeadCommit() == nil {
		log.Print("HeadCommit is empty")
		return
	}

	prNumber := extractPRNumberFromMergedMessage(event.GetHeadCommit().GetMessage())
	if prNumber == 0 {
		log.Printf("Failed parse commit message. could not extract pr number: %s", event.GetHeadCommit().GetMessage())
		return
	}

	ghClient := github.NewClient(&http.Client{Transport: c.transport})
	b, err := c.getCommitsDiff(ctx, ghClient, event.GetBefore(), event.GetAfter())
	if err != nil {
		errorLog(err)
		return
	}
	changed, err := changedFilesFromDiff(b)
	if err != nil {
		errorLog(err)
		return
	}

	ok := false
	for _, v := range changed {
		if strings.HasPrefix(v, ctx.Rule.Dir) {
			ok = true
			break
		}
	}
	if !ok {
		errorLog(xerrors.New("nothing change"))
		return
	}

	result, err := c.runExecute(ctx, client)
	if err != nil {
		errorLog(err)
		return
	}

	comment := "Applied:\n```\n" + result + "\n```\n"
	_, _, err = ghClient.Issues.CreateComment(context.Background(), ctx.Owner, ctx.Repo, prNumber, &github.IssueComment{Body: github.String(comment)})
	if err != nil {
		errorLog(err)
		return
	}
}

func (c *DNSControlConsumer) dispatchPullRequestEvent(event *github.PullRequestEvent, client *kubernetes.Clientset) {
	switch event.GetAction() {
	case "opened", "synchronized":
	default:
		return
	}

	ctx := &dnsControlContext{eventContext: NewEventContextFromPullRequest(event)}
	if err := c.fetchRuleFile(ctx); err != nil {
		errorLog(err)
		return
	}

	ghClient := github.NewClient(&http.Client{Transport: c.transport})
	res, _, err := ghClient.PullRequests.GetRaw(context.Background(), ctx.Owner, ctx.Repo, ctx.PullRequestNumber, github.RawOptions{Type: github.Diff})
	if err != nil {
		errorLog(err)
		return
	}

	changed, err := changedFilesFromDiff(res)
	if err != nil {
		errorLog(err)
		return
	}
	ctx.Changed = changed

	result, err := c.runPreview(ctx, client)
	if err != nil {
		errorLog(err)
		return
	}

	comment := "Preview:\n```\n" + result + "\n```\n"
	_, _, err = ghClient.Issues.CreateComment(context.Background(), ctx.Owner, ctx.Repo, ctx.PullRequestNumber, &github.IssueComment{Body: github.String(comment)})
	if err != nil {
		errorLog(err)
		return
	}
}

func (c *DNSControlConsumer) runExecute(ctx *dnsControlContext, client *kubernetes.Clientset) (string, error) {
	buildId := newBuildId()
	defer func() {
		if err := c.cleanup(client, buildId); err != nil {
			errorLog(err)
			return
		}
	}()

	pod := c.runPod(ctx, buildId, "push")
	_, err := client.CoreV1().Pods(c.Namespace).Create(pod)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	_, err = WaitForFinish(client, pod.Namespace, pod.Name)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	body, err := client.CoreV1().Pods(c.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "dnscontrol"}).DoRaw()
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	return string(body), nil
}

func (c *DNSControlConsumer) runPreview(ctx *dnsControlContext, client *kubernetes.Clientset) (string, error) {
	buildId := newBuildId()
	defer func() {
		if err := c.cleanup(client, buildId); err != nil {
			errorLog(err)
			return
		}
	}()

	pod := c.runPod(ctx, buildId, "preview")
	_, err := client.CoreV1().Pods(c.Namespace).Create(pod)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	_, err = WaitForFinish(client, pod.Namespace, pod.Name)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	body, err := client.CoreV1().Pods(c.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "dnscontrol"}).DoRaw()
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	return string(body), nil
}

func (c *DNSControlConsumer) fetchRuleFile(ctx *dnsControlContext) error {
	if contents, err := ctx.FetchRuleFile(c.transport, dnscontrolBuildRule); err != nil {
		return xerrors.Errorf(": %v", err)
	} else {
		rule, err := config.ParseDNSControlRule(contents)
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
		log.Printf("Rule: %v", rule)
		ctx.Rule = rule
	}

	return nil
}

func (c *DNSControlConsumer) getCommitsDiff(ctx *dnsControlContext, ghClient *github.Client, base, head string) (string, error) {
	compare, _, err := ghClient.Repositories.CompareCommits(context.Background(), ctx.Owner, ctx.Repo, base, head)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	req, err := ghClient.NewRequest(http.MethodGet, compare.GetDiffURL(), nil)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", xerrors.Errorf(": %v", err)
	}

	return string(b), nil
}

func (c *DNSControlConsumer) cleanup(client *kubernetes.Clientset, buildId string) error {
	podList, err := client.CoreV1().Pods(c.Namespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labelKeyJobId, buildId),
	})
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	for _, v := range podList.Items {
		err := client.CoreV1().Pods(c.Namespace).Delete(v.Name, nil)
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
	}

	return nil
}

func (c *DNSControlConsumer) runPod(ctx *dnsControlContext, buildId, command string) *corev1.Pod {
	mainImage := defaultDNSControlImage
	if ctx.Rule.Image != "" {
		mainImage = ctx.Rule.Image
	}
	hostAliases := make([]corev1.HostAlias, 0)
	for _, v := range c.HostAliases {
		hostAliases = append(hostAliases, corev1.HostAlias{Hostnames: v.Hostnames, IP: v.IP})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", ctx.Owner, ctx.Repo, buildId),
			Namespace: c.Namespace,
			Labels: map[string]string{
				labelKeyJobId:  buildId,
				labelKeyCtrlBy: "dnscontrol",
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: builderServiceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{
				{
					Name:  "pre-process",
					Image: buildSidecarImage,
					Args: []string{
						"--action=clone",
						"--work-dir=/work",
						fmt.Sprintf("--url=%s", ctx.CloneURL()),
						fmt.Sprintf("--commit=%s", ctx.Commit),
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workdir", MountPath: "/work"},
					},
				},
			},
			HostAliases: hostAliases,
			Containers: []corev1.Container{
				{
					Name:       "dnscontrol",
					Image:      mainImage,
					Args:       []string{command},
					WorkingDir: filepath.Join("/work", ctx.Rule.Dir),
					Env: []corev1.EnvVar{
						{Name: ctx.Rule.Secret.EnvName, ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: ctx.Rule.Secret.Name},
								Key:                  ctx.Rule.Secret.Key,
							},
						}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workdir", MountPath: "/work"},
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
			},
		},
	}
}

func changedFilesFromDiff(v string) ([]string, error) {
	diffs, err := diff.ParseMultiFileDiff([]byte(v))
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	files := make(map[string]struct{})
	for _, v := range diffs {
		s := strings.Split(v.NewName, "/")
		name := strings.Join(s[1:], "/")
		files[name] = struct{}{}
	}

	res := make([]string, 0, len(files))
	for k := range files {
		res = append(res, "/"+k)
	}

	return res, nil
}

func extractPRNumberFromMergedMessage(msg string) int {
	matched := prMergedMessageRe.FindStringSubmatch(msg)
	if len(matched) != 2 {
		return 0
	}
	num, err := strconv.ParseInt(matched[1], 10, 32)
	if err != nil {
		return 0
	}

	return int(num)
}
