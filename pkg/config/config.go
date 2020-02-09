package config

import (
	"io/ioutil"
	"os"

	"golang.org/x/xerrors"
	"sigs.k8s.io/yaml"
)

type Config struct {
	WebhookListener         string `json:"webhook_listener"`
	BuildNamespace          string `json:"build_namespace"`
	GitHubTokenFile         string `json:"github_token_file"`
	GitHubAppId             int64  `json:"app_id"`
	GitHubInstallationId    int64  `json:"installation_id"`
	GitHubAppPrivateKeyFile string `json:"app_private_key_file"`

	GitHubToken string `json:"-"`
}

func ReadConfig(p string) (*Config, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	conf := &Config{}
	if err := yaml.Unmarshal(b, conf); err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	if conf.BuildNamespace == "" {
		conf.BuildNamespace = os.Getenv("POD_NAMESPACE")
	}
	if conf.BuildNamespace == "" {
		return nil, xerrors.New("config: build namespace is mandatory")
	}
	if conf.GitHubTokenFile != "" {
		b, err := ioutil.ReadFile(conf.GitHubTokenFile)
		if err != nil {
			return nil, xerrors.Errorf(": %v", err)
		}
		conf.GitHubToken = string(b)
	}

	return conf, nil
}

type BuildRule struct {
	Rules []*Rule `json:"rules"`
}

type Rule struct {
	Name        string       `json:"name"`
	Repo        string       `json:"repo"`
	Private     bool         `json:"private"`
	Target      string       `json:"target"`
	Artifacts   []string     `json:"artifacts"`
	PostProcess *PostProcess `json:"post_process"`
}

type PostProcess struct {
	Repo  string   `json:"repo"`
	Image string   `json:"image"`
	Paths []string `json:"paths"`
}

func ReadBuildRule(p string) (*BuildRule, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	conf := &BuildRule{}
	if err := yaml.Unmarshal(b, conf); err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}

	return conf, nil
}
