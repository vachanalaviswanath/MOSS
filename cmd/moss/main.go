package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-github/v47/github"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

func check_gitleaks_conf(gitleaks_path string) error {
	_, err := os.ReadFile(gitleaks_path)
	if err != nil {
		log.Fatal().Err(err).Str("path", gitleaks_path).Msg("failed to read gitleaks toml")
		return err
	}
	return nil
}

func scan_repo(repo *github.Repository, pat, orgname, gl_conf_path string, results chan GitleaksRepoResult) {
	// build a result object
	result := GitleaksRepoResult{
		Repository: *repo.Name,
		URL:        *repo.URL,
		IsPrivate:  *repo.Private,
		Org:        orgname,
	}
	// make temp dir
	dir, err := os.MkdirTemp(os.TempDir(), "moss_")
	if err != nil {
		log.Error().Err(err).Str("repo", *repo.Name).Msg("failed to create temp dir to scan repo")
		result.Err = err
		results <- result
		return
	}
	log.Debug().Str("repo", *repo.Name).Str("dir", dir).Msg("tempdir set")
	//defer os.RemoveAll(dir)
	// clone into it
	cloneUrl := *repo.CloneURL
	cloneUrl = strings.Replace(cloneUrl, "https://", fmt.Sprintf("https://%s@", pat), 1)
	cloneargs := []string{"clone", cloneUrl, dir}
	cmd := exec.Command("git", cloneargs...)
	if err := cmd.Run(); err != nil {
		log.Error().Err(err).Str("repo", *repo.Name).Msg("failed to clone repo")
		result.Err = err
		results <- result
		return
	}
	// run gitleaks
	outputpath := fmt.Sprintf("%s/__gitleaks.json", dir)
	outputarg := fmt.Sprintf("-r=%s", outputpath)
	confpath := fmt.Sprintf("-c=%s", gl_conf_path)
	// not exactly sure why gitleaks doesn't detect that
	// it IS a git repo, but we can still detect secrets
	gitleaks_args := []string{"detect", "-v", "--no-git", "-f=json", "--exit-code=0", outputarg, confpath, dir}
	// TEMP
	var outb, errb bytes.Buffer
	gl_cmd := exec.Command("gitleaks", gitleaks_args...)
	gl_cmd.Stdout = &outb
	gl_cmd.Stderr = &errb
	//fmt.Println(strings.Join(gl_cmd.Args, " "))
	if err := gl_cmd.Run(); err != nil {
		log.Error().Err(err).Str("repo", *repo.Name).Msg("error running gitleaks on the repo")
		result.Err = err
		results <- result
		return
	}

	// code useful for debugging, but not for leaving compiled
	// fmt.Println(outb.String())
	// fmt.Println(errb.String())
	// log.Debug().Str("stdout", outb.String()).Str("stderr", errb.String()).Msg("output from gitleaks")

	// load the result into a GitleaksResult
	resultfile, err := os.ReadFile(outputpath)
	if err != nil {
		log.Error().Err(err).Str("repo", *repo.Name).Msg("error opening results file")
		result.Err = err
		results <- result
		return
	}
	jsonResults := make([]GitleaksResult, 0)
	err = json.Unmarshal(resultfile, &jsonResults)
	if err != nil {
		log.Error().Err(err).Str("repo", *repo.Name).Msg("error unmarshaling gitleaks results")
		result.Err = err
		results <- result
		return
	}
	//success: return
	result.Results = jsonResults
	result.Err = nil
	results <- result
}

func get_org_repos(orgname, pat string, daysago int) ([]*github.Repository, error) {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: pat},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	//
	time_ago := time.Now().AddDate(0, 0, (-1 * daysago))
	org_repos := make([]*github.Repository, 0)
	page := 0
	for {
		opt := &github.RepositoryListByOrgOptions{Type: "all", Sort: "pushed", Direction: "desc", ListOptions: github.ListOptions{Page: page}}
		repos, _, err := client.Repositories.ListByOrg(context.Background(), orgname, opt)
		if err != nil {
			log.Error().Err(err).Str("org", orgname).Msg("Error getting repositories from Github")
			return nil, err
		}
		saw_older := false
		for _, repo := range repos {
			if *repo.Archived {
				continue
			}
			if repo.PushedAt.Time.Before(time_ago) {
				saw_older = true
				break
			}
			org_repos = append(org_repos, repo)
		}
		if saw_older {
			break
		}
		page = page + 1
	}
	return org_repos, nil
}

func main() {
	// setup logging
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Info().Msg("logging setup")
	if os.Getenv("MOSS_DEBUG") != "" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
		log.Debug().Msg("loglevel debug")
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
	// load the config file
	foo := os.Environ()
	_ = foo
	confdir := os.Getenv("MOSS_CONFDIR")
	if confdir == "" {
		confdir = "./configs/conf.yml"
	}
	var conf Conf
	conf.getConfig(confdir)
	// check the gitleaks.toml file exists and isn't empty
	gitleaks_toml_path := os.Getenv("MOSS_GITLEAKSCONF")
	if gitleaks_toml_path == "" {
		log.Debug().Msg("gitleaks toml path was empty, using default")
		gitleaks_toml_path = "./configs/gitleaks.toml"
	}
	check_gitleaks_conf(gitleaks_toml_path)
	// check the PAT exists for each org
	pats := make(map[string]string, 0)
	for _, org := range conf.GithubConfig.OrgsToScan {
		patenv := fmt.Sprintf("PAT_%s", org)
		pat := os.Getenv(patenv)
		if pat == "" {
			log.Error().Str("org", org).Msg("PAT for org doesn't exist. Skipping it")
			continue
		}
		pats[org] = pat
	}
	// foreach org, get the repos according to days_to_scan
	all_repos := make([]*github.Repository, 0)
	for org, pat := range pats {
		repos, err := get_org_repos(org, pat, conf.GithubConfig.DaysToScan)
		if err != nil {
			log.Error().Err(err).Str("org", org).Msg("Failed to get repos from org. Continuing")
			continue
		}
		all_repos = append(all_repos, repos...)
	}
	// create the channel and kick off the scans
	results := make(chan GitleaksRepoResult)
	for _, repo := range all_repos {
		reponame := repo.GetFullName()
		orgname := strings.Split(reponame, "/")[0]
		pat := pats[orgname]
		go scan_repo(repo, pat, orgname, gitleaks_toml_path, results)
	}
	// collect the results
	collected := 0
	final_results := make([]GitleaksRepoResult, 0)
	for {
		repoResult := <-results
		final_results = append(final_results, repoResult)
		collected = collected + 1
		if collected >= len(all_repos) {
			break
		}
	}
	// format and output the results nicely
	if conf.Output.Format == "json" {
		output := json_output(final_results, conf.GithubConfig.OrgsToScan)
		// todo: make this part of the conf
		os.WriteFile("./output.json", []byte(output), 0644)
		fmt.Println(output)
	}
	// for _, rr := range final_results {
	// 	if rr.Err != nil {
	// 		fmt.Printf("repo %s finished with an error\n", rr.Repository)
	// 	} else {
	// 		fmt.Printf("repo %s finished with no errors\n", rr.Repository)
	// 	}
	// }
}
