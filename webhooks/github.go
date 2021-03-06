package webhooks

import (
	"errors"
	"fmt"
	"github.com/Lavoaster/cloudsmith-sync/cloudsmith"
	"github.com/Lavoaster/cloudsmith-sync/composer"
	"github.com/Lavoaster/cloudsmith-sync/config"
	"github.com/Lavoaster/cloudsmith-sync/git"
	"gopkg.in/go-playground/webhooks.v5/github"
	git2 "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"net/http"
	"strconv"
	"strings"
)

var Hook *github.Webhook
var Client *cloudsmith.Client
var Config *config.Config

func HandleGithubWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := Hook.Parse(r, github.PushEvent, github.PingEvent)
	if err != nil {
		if err == github.ErrMissingGithubEventHeader || err == github.ErrMissingHubSignatureHeader {
			w.WriteHeader(400)
			w.Write([]byte(err.Error()))
			return
		}

		if err == github.ErrHMACVerificationFailed {
			w.WriteHeader(403)
			w.Write([]byte(err.Error()))
			return
		}

		if err == github.ErrEventNotFound {
			w.WriteHeader(422)
			w.Write([]byte(err.Error()))
			return
		}

		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
	}

	switch payload.(type) {
	case github.PingPayload:
		push := payload.(github.PingPayload)

		w.WriteHeader(201)
		w.Write([]byte("pong (" + strconv.Itoa(push.HookID) + ")"))

	case github.PushPayload:
		push := payload.(github.PushPayload)
		repoCfg, err := Config.GetRepository(push.Repository.SSHURL)

		if err != nil {
			w.WriteHeader(422)
			w.Write([]byte("repository not configured"))
			return
		}

		repoDir, err := git.GitUrlToDirectory(repoCfg.Url)
		repoPath := Config.GetRepoPath(repoDir)

		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		repo, err := git.CloneOrOpenAndUpdate(repoCfg.Url, repoPath)

		worktree, err := repo.Worktree()

		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		ref, err := repo.Reference(plumbing.ReferenceName(push.Ref), true)

		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		branchName := strings.TrimPrefix(push.Ref, "refs/heads/")
		tag := strings.TrimPrefix(push.Ref, "refs/tags/")
		isBranch := tag == push.Ref

		if isBranch {
			_, err := git.CheckoutBranch(repo, worktree, ref)

			if err != nil {
				w.WriteHeader(500)
				w.Write([]byte(err.Error()))
				return
			}
		} else {
			_, err := git.CheckoutTag(repo, worktree, ref)

			if err != nil {
				w.WriteHeader(500)
				w.Write([]byte(err.Error()))
				return
			}
		}

		composerData, err := composer.LoadFile(repoPath)

		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		packageName := composerData["name"].(string)

		version, normalisedVersion, err := composer.DeriveVersion(ref.Name().String(), isBranch)

		if err != nil {
			w.WriteHeader(200)
			w.Write([]byte(fmt.Sprintf("Skipping %s@%s due to %s...\n", packageName, branchName, err)))
			return
		}

		Client.DeletePackageIfExists(Config.Owner, Config.TargetRepository, packageName, version)

		if push.Deleted {
			w.WriteHeader(204)
			return
		}

		err = processPackage(
			Client,
			&repoCfg,
			repoPath,
			ref.Name().Short(),
			packageName,
			version,
			normalisedVersion,
			ref.Hash().String(),
		)

		worktree.Reset(&git2.ResetOptions{
			Mode: git2.HardReset,
		})

		if err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		w.WriteHeader(204)
	}
}

func processPackage(
	client *cloudsmith.Client,
	repoCfg *config.Repository,
	repoPath, branchOrTagName, packageName, version, normalisedVersion, commitRef string,
) error {
	var source *composer.Source

	if repoCfg.PublishSource {
		source = &composer.Source{
			Url:       repoCfg.Url,
			Type:      "git",
			Reference: commitRef,
		}
	}

	// Mutate composer.json file
	err := composer.MutateComposerFile(repoPath, version, normalisedVersion, source)
	if err != nil {
		return err
	}

	// Extract Info from the composer file
	packageNameParts := strings.Split(packageName, "/")
	namespace := packageNameParts[0]
	name := packageNameParts[1]

	artifactName := fmt.Sprintf("%v-%v-%v.zip", namespace, name, commitRef)
	artifactPath := Config.GetArtifactPath(artifactName)

	// Create archive file
	err = git.CreateArtifactFromRepository(repoPath, artifactPath)

	if err != nil {
		return err
	}

	//Upload archive to cloudsmith
	_, err = client.UploadComposerPackage(Config.Owner, Config.TargetRepository, artifactPath)

	if err != nil {
		return errors.New(fmt.Sprintf("Skipping %s@%s due to %s...\n", packageName, branchOrTagName, err))
	}

	return nil
}
