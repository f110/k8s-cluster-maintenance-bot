package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"golang.org/x/xerrors"

	"github.com/f110/k8s-cluster-maintenance-bot/pkg/config"
	"github.com/f110/k8s-cluster-maintenance-bot/pkg/consumer"
	"github.com/f110/k8s-cluster-maintenance-bot/pkg/webhook"
)

func producer(args []string) error {
	confFile := ""
	buildRuleFile := ""
	debug := false
	fs := pflag.NewFlagSet("maintenance-bot", pflag.ContinueOnError)
	fs.StringVarP(&confFile, "conf", "c", confFile, "Config file")
	fs.StringVar(&buildRuleFile, "build-rule", buildRuleFile, "Build rule")
	fs.BoolVarP(&debug, "debug", "D", debug, "Debug")
	if err := fs.Parse(args); err != nil {
		return xerrors.Errorf(": %v", err)
	}

	conf, err := config.ReadConfig(confFile)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	buildRule, err := config.ReadBuildRule(buildRuleFile)
	if err != nil {
		return xerrors.Errorf(": %v", err)
	}

	webhookListener := webhook.NewWebhookListener(conf.WebhookListener)

	for _, r := range buildRule.Rules {
		builder, err := consumer.NewBuildConsumer(conf.BuildNamespace, r, conf.GitHubAppId, conf.GitHubInstallationId, conf.GitHubAppPrivateKeyFile, debug)
		if err != nil {
			return xerrors.Errorf(": %v", err)
		}
		s := strings.SplitN(r.Repo, "/", 2)
		if strings.HasSuffix(s[1], ".git") {
			s[1] = strings.TrimSuffix(s[1], ".git")
		}
		webhookListener.SubscribePushEvent(s[0], s[1], builder.Build)
	}

	if err := webhookListener.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			return nil
		}

		return xerrors.Errorf(": %v", err)
	}

	return nil
}

func main() {
	if err := producer(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}
