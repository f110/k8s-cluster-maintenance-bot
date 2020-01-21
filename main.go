package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"

	"gopkg.in/yaml.v2"
)

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

	k := make(map[string]interface{})
	if err := yaml.Unmarshal(b, k); err != nil {
		return err
	}

	changed := false
	if v, ok := k["images"]; ok {
		value := v.([]interface{})
		for _, i := range value {
			image := i.(map[interface{}]interface{})
			if n, ok := image["name"]; ok {
				name := n.(string)
				if name == imageName {
					image["newTag"] = newTag
					changed = true
				}
			}
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
