package main

import (
	"fmt"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	azdoforge "github.com/yteraoka/kibitz/internal/forge/azuredevops"
	githubforge "github.com/yteraoka/kibitz/internal/forge/github"
	gitlabforge "github.com/yteraoka/kibitz/internal/forge/gitlab"
)

// reactionPermissions is everything a GitHub token minted by the server can
// do. A reaction on a comment in the conversation needs issues, one on a line
// of the diff needs pull requests, and nothing else is asked for — in
// particular not contents, so a token taken from the server cannot push.
var reactionPermissions = map[string]string{
	"issues":        "write",
	"pull_requests": "write",
}

// newReactors builds a reactor for each platform the server has a credential
// for. A platform without one simply gets no reaction.
func newReactors(cfg config.Reactions) (map[event.Platform]forge.Reactor, error) {
	reactors := make(map[event.Platform]forge.Reactor)
	if !cfg.Enabled {
		return reactors, nil
	}

	if cfg.GitHub.AppID != 0 {
		client, err := githubforge.New(githubforge.Config{
			AppID:          cfg.GitHub.AppID,
			InstallationID: cfg.GitHub.InstallationID,
			PrivateKey:     cfg.GitHub.PrivateKey.Reveal(),
			BaseURL:        cfg.GitHub.BaseURL,
			Permissions:    reactionPermissions,
		})
		if err != nil {
			return nil, fmt.Errorf("github reactions: %w", err)
		}
		reactors[event.PlatformGitHub] = client
	}
	if cfg.GitLab.Configured() {
		client, err := gitlabforge.New(gitlabforge.Config{
			BaseURL: cfg.GitLab.BaseURL,
			Token:   cfg.GitLab.Token.Reveal(),
		})
		if err != nil {
			return nil, fmt.Errorf("gitlab reactions: %w", err)
		}
		reactors[event.PlatformGitLab] = client
	}
	if cfg.AzureDevOps.Configured() {
		client, err := azdoforge.New(azdoforge.Config{
			OrganizationURL: cfg.AzureDevOps.OrganizationURL,
			Token:           cfg.AzureDevOps.Token.Reveal(),
			TokenIsBearer:   cfg.AzureDevOps.TokenIsBearer,
		})
		if err != nil {
			return nil, fmt.Errorf("azure devops reactions: %w", err)
		}
		reactors[event.PlatformAzureDevOps] = client
	}
	return reactors, nil
}
