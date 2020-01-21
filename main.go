package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v29/github"
	"golang.org/x/oauth2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"gopkg.in/yaml.v2"
)

const (
	authorName  = "bot"
	authorEmail = "fmhrit+bot@gmail.com"
)

func switchBranch(repo *git.Repository) (string, *git.Worktree, error) {
	branchName := fmt.Sprintf("update-kustomization-%d", time.Now().Unix())

	masterRef, err := repo.Reference("refs/remotes/origin/master", true)
	if err != nil {
		return "", nil, err
	}

	ref := plumbing.NewHashReference(plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", branchName)), masterRef.Hash())
	repo.Storer.SetReference(ref)

	tree, err := repo.Worktree()
	if err != nil {
		return "", nil, err
	}
	if err := tree.Checkout(&git.CheckoutOptions{Branch: ref.Name()}); err != nil {
		return "", nil, err
	}

	return branchName, tree, nil
}

func commit(tree *git.Worktree, path string) error {
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

func push(repo *git.Repository, branchName string) error {
	refSpec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branchName, branchName)
	return repo.Push(&git.PushOptions{
		Auth: &http.BasicAuth{
			Username: "octocat",
			Password: os.Getenv("GITHUB_TOKEN"),
		},
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refSpec)},
	})
}

func createPullRequest(branch, path string) error {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)

	description := fmt.Sprintf("Update %s", path)
	client := github.NewClient(tc)

	s := strings.SplitN(os.Getenv("GITHUB_REPO"), "/", 2)
	_, _, err := client.PullRequests.Create(context.Background(), s[0], s[1], &github.NewPullRequest{
		Title: github.String(fmt.Sprintf("Update %s", path)),
		Body:  github.String(description),
		Base:  github.String("master"),
		Head:  github.String(branch),
	})

	return err
}

func updateKustomization() error {
	repositoryRoot := ""
	inputFile := ""
	imageName := ""
	newTag := ""
	flag.StringVar(&repositoryRoot, "root", repositoryRoot, "git repository root")
	flag.StringVar(&inputFile, "input", inputFile, "kustomization.yaml path (relative path from repo root)")
	flag.StringVar(&imageName, "image-name", imageName, "Image name")
	flag.StringVar(&newTag, "image-tag", newTag, "New image tag")
	flag.Parse()

	if os.Getenv("GITHUB_TOKEN") == "" {
		return errors.New("not provided github personal access token")
	}
	if os.Getenv("GITHUB_REPO") == "" || !strings.Contains(os.Getenv("GITHUB_REPO"), "/") {
		return errors.New("not provided github repository name (e.g. octocat/example)")
	}

	repo, err := git.PlainOpen(filepath.Join(repositoryRoot))
	if err != nil {
		return err
	}
	branchName, tree, err := switchBranch(repo)
	if err != nil {
		return err
	}

	absPath := filepath.Join(repositoryRoot, inputFile)
	b, err := ioutil.ReadFile(absPath)
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
		if err := ioutil.WriteFile(absPath, outBuf, 0644); err != nil {
			return err
		}
	} else {
		return errors.New("did not changed")
	}

	if err := commit(tree, inputFile); err != nil {
		return err
	}

	if err := push(repo, branchName); err != nil {
		return err
	}

	if err := createPullRequest(branchName, inputFile); err != nil {
		return err
	}

	log.Print("Success create a pull request")
	return nil
}

func main() {
	if err := updateKustomization(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}
