package config

import (
	"io/ioutil"

	"golang.org/x/xerrors"
	"sigs.k8s.io/yaml"
)

type Config struct {
	WebhookListener string `json:"webhook_listener"`
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
	Type     string `json:"type"`
	ImageTag string `json:"image_tag"`
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
