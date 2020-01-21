package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"

	"gopkg.in/yaml.v2"
)

type kustomization struct {
	Images []*image `yaml:"images"`
}

type image struct {
	Name   string `yaml:"name"`
	NewTag string `yaml:"newTag"`
}

func updateRepository() error {
	inputFile := ""
	imageName := ""
	newTag := ""
	flag.StringVar(&inputFile, "input", inputFile, "kustomization.yaml path")
	flag.StringVar(&imageName, "image-name", imageName, "Image name")
	flag.StringVar(&newTag, "image-tag", newTag, "New image tag")
	flag.Parse()

	b, err := ioutil.ReadFile(inputFile)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errors.New("file is empty")
	}

	k := &kustomization{}
	if err := yaml.Unmarshal(b, k); err != nil {
		return err
	}

	changed := false
	for _, v := range k.Images {
		if v.Name == imageName {
			v.NewTag = newTag
			changed = true
		}
	}

	if changed {
		outBuf, err := yaml.Marshal(k)
		if err != nil {
			return err
		}
		if err := ioutil.WriteFile(inputFile, outBuf, 0644); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	if err := updateRepository(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}
