package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v55/github"
	"golang.org/x/oauth2"
)

type ModJSON struct {
	Version string `json:"version"`
}

var verbose bool

func debugf(format string, args ...any) {
	if verbose {
		fmt.Printf(format+"\n", args...)
	}
}

func main() {
	owner := flag.String("owner", "", "GitHub repo owner (required)")
	repo := flag.String("repo", "", "GitHub repo name (required)")
	branch := flag.String("branch", "main", "Branch name to look for workflow runs")
	workflowFile := flag.String("workflow", "multi-platform.yml", "Workflow filename")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose debug output")
	flag.Parse()

	if *owner == "" || *repo == "" {
		flag.Usage()
		os.Exit(1)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN environment variable must be set")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	debugf("Listing workflow runs for workflow file %q on branch %q", *workflowFile, *branch)
	runs, _, err := client.Actions.ListWorkflowRunsByFileName(ctx, *owner, *repo, *workflowFile, &github.ListWorkflowRunsOptions{
		Status: "completed",
		Branch: *branch,
	})
	if err != nil {
		log.Fatalf("Error listing workflow runs: %v", err)
	}
	if len(runs.WorkflowRuns) == 0 {
		log.Fatalf("No completed workflow runs found for workflow '%s' on branch '%s'", *workflowFile, *branch)
	}

	debugf("Found %d completed workflow runs", len(runs.WorkflowRuns))

	latestRun := runs.WorkflowRuns[0]
	debugf("Latest run ID: %d, Head SHA: %s, Created at: %v", latestRun.GetID(), latestRun.GetHeadSHA(), latestRun.GetCreatedAt())

	debugf("Listing artifacts for repo %s/%s", *owner, *repo)
	arts, _, err := client.Actions.ListArtifacts(ctx, *owner, *repo, &github.ListOptions{})
	if err != nil {
		log.Fatalf("Error listing artifacts: %v", err)
	}
	debugf("Found %d artifacts total", len(arts.Artifacts))

	var artifact *github.Artifact
	for _, a := range arts.Artifacts {
		debugf("Artifact: ID=%d, Name=%q, WorkflowRunID=%d", a.GetID(), a.GetName(), *a.GetWorkflowRun().ID)
		if a.GetName() == "Build Output" && *a.GetWorkflowRun().ID == latestRun.GetID() {
			artifact = a
			break
		}
	}
	if artifact == nil {
		log.Fatalf("Artifact 'Build Output' not found for latest run")
	}
	debugf("Selected artifact ID: %d", artifact.GetID())

	debugf("Getting artifact download URL")
	artifactURL, _, err := client.Actions.DownloadArtifact(ctx, *owner, *repo, artifact.GetID(), true)
	if err != nil {
		log.Fatalf("Error getting artifact download URL: %v", err)
	}
	debugf("Downloading artifact from: %s", artifactURL.String())

	tmpZipFile, err := os.CreateTemp("", "artifact-*.zip")
	if err != nil {
		log.Fatalf("Error creating temp file for artifact download: %v", err)
	}
	defer func() {
		tmpZipFile.Close()
		os.Remove(tmpZipFile.Name())
	}()

	debugf("Downloading artifact to temp file: %s", tmpZipFile.Name())

	resp, err := http.Get(artifactURL.String())
	if err != nil {
		log.Fatalf("Error downloading artifact: %v", err)
	}
	defer resp.Body.Close()

	written, err := io.Copy(tmpZipFile, resp.Body)
	if err != nil {
		log.Fatalf("Error writing artifact to temp file: %v", err)
	}
	debugf("Downloaded %d bytes to %s", written, tmpZipFile.Name())

	zipData, err := os.ReadFile(tmpZipFile.Name())
	if err != nil {
		log.Fatalf("Error reading downloaded artifact zip from temp file: %v", err)
	}

	geodeData, geodeFilename, err := extractGeodeFileFromZip(zipData)
	if err != nil {
		log.Fatalf("Error extracting .geode file: %v", err)
	}
	fmt.Printf("Found .geode file: %s\n", geodeFilename)

	debugf("Listing contents of artifact zip:")
	if verbose {
		if err := debugListZipContents(zipData); err != nil {
			debugf("Failed to list artifact zip contents: %v", err)
		}
	}

	debugf("Listing contents of .geode zip:")
	if verbose {
		if err := debugListZipContents(geodeData); err != nil {
			debugf("Failed to list .geode zip contents: %v", err)
		}
	}

	version, err := parseVersionFromGeode(geodeData)
	if err != nil {
		log.Fatalf("Error parsing mod.json: %v", err)
	}
	fmt.Printf("Parsed version: %s\n", version)

	tagName := fmt.Sprintf(version)

	debugf("Getting branch ref 'refs/heads/%s'", *branch)
	ref, _, err := client.Git.GetRef(ctx, *owner, *repo, "refs/heads/"+*branch)
	if err != nil {
		log.Fatalf("Error getting branch ref: %v", err)
	}
	commitSHA := ref.GetObject().GetSHA()
	debugf("Latest commit SHA on branch %s: %s", *branch, commitSHA)

	debugf("Creating git tag object %s", tagName)
	tagMessage := fmt.Sprintf("Tag for version %s", version)
	tag := &github.Tag{
		Tag:     github.String(tagName),
		Message: github.String(tagMessage),
		Object: &github.GitObject{
			Type: github.String("commit"),
			SHA:  github.String(commitSHA),
		},
		Tagger: &github.CommitAuthor{
			Name:  github.String("GitHub Actions Bot"),
			Email: github.String("actions@github.com"),
		},
	}

	createdTag, _, err := client.Git.CreateTag(ctx, *owner, *repo, tag)
	if err != nil {
		log.Fatalf("Error creating git tag object: %v", err)
	}
	debugf("Created tag object SHA: %s", createdTag.GetSHA())

	refTag := &github.Reference{
		Ref: github.String("refs/tags/" + tagName),
		Object: &github.GitObject{
			SHA: createdTag.SHA,
		},
	}

	_, _, err = client.Git.CreateRef(ctx, *owner, *repo, refTag)
	if err != nil {
		log.Fatalf("Error creating tag ref: %v", err)
	}
	fmt.Printf("Created tag %s\n", tagName)

	debugf("Creating release for tag %s", tagName)
	release := &github.RepositoryRelease{
		TagName: github.String(tagName),
		Name:    github.String(fmt.Sprintf("Release %s", tagName)),
	}
	createdRelease, _, err := client.Repositories.CreateRelease(ctx, *owner, *repo, release)
	if err != nil {
		log.Fatalf("Error creating release: %v", err)
	}
	debugf("Created release ID: %d", createdRelease.GetID())

	tmpfile, err := os.CreateTemp("", "mod-*.geode")
	if err != nil {
		log.Fatalf("Error creating temp file for upload: %v", err)
	}
	defer func() {
		tmpfile.Close()
		os.Remove(tmpfile.Name())
	}()

	_, err = tmpfile.Write(geodeData)
	if err != nil {
		log.Fatalf("Error writing .geode to temp file: %v", err)
	}
	debugf("Wrote .geode data to temp file %s", tmpfile.Name())

	uploadOpts := &github.UploadOptions{
		Name: geodeFilename,
	}

	f, err := os.Open(tmpfile.Name())
	if err != nil {
		log.Fatalf("Error opening temp file for upload: %v", err)
	}
	defer f.Close()

	debugf("Uploading release asset %s", geodeFilename)
	_, _, err = client.Repositories.UploadReleaseAsset(ctx, *owner, *repo, createdRelease.GetID(), uploadOpts, f)
	if err != nil {
		log.Fatalf("Error uploading release asset: %v", err)
	}

	fmt.Println("Release created and asset uploaded successfully")
}

func extractGeodeFileFromZip(zipData []byte) ([]byte, string, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, "", fmt.Errorf("failed to open zip reader: %w", err)
	}

	for _, f := range r.File {
		if strings.HasSuffix(f.Name, ".geode") {
			rc, err := f.Open()
			if err != nil {
				return nil, "", fmt.Errorf("failed to open .geode file inside zip: %w", err)
			}
			defer rc.Close()

			data, err := io.ReadAll(rc)
			if err != nil {
				return nil, "", fmt.Errorf("failed to read .geode file inside zip: %w", err)
			}

			debugf("Extracted .geode file from zip: %s (%d bytes)", f.Name, len(data))

			return data, filepath.Base(f.Name), nil
		}
	}

	return nil, "", fmt.Errorf(".geode file not found in zip")
}

func parseVersionFromGeode(geodeData []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(geodeData), int64(len(geodeData)))
	if err != nil {
		return "", fmt.Errorf("failed to open .geode as zip: %w", err)
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}

		if strings.HasSuffix(f.Name, "mod.json") {
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("failed to open mod.json inside .geode: %w", err)
			}
			defer rc.Close()

			debugf("Found mod.json inside .geode at path: %s", f.Name)

			var mod ModJSON
			if err := json.NewDecoder(rc).Decode(&mod); err != nil {
				return "", fmt.Errorf("failed to decode mod.json: %w", err)
			}

			if mod.Version == "" {
				return "", fmt.Errorf("version key not found in mod.json")
			}

			return mod.Version, nil
		}
	}

	return "", fmt.Errorf("mod.json not found inside .geode file")
}

func debugListZipContents(zipData []byte) error {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}

	for _, f := range r.File {
		debugf("  %s", f.Name)
	}
	return nil
}
