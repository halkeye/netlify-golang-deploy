package main

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/go-openapi/runtime"
	openapiClient "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	netlify "github.com/netlify/open-api/go/models"
	"github.com/netlify/open-api/go/plumbing"
	"github.com/netlify/open-api/go/plumbing/operations"
	"github.com/pkg/errors"
	"github.com/sethvargo/go-retry"
)

func mustGetSha1(filename string) string {
	f, err := os.Open(filename)
	if err != nil {
		panic(errors.Wrap(err, "Unable to open file to sha it"))
	}
	defer f.Close()

	hash := sha1.New()
	if _, err := io.Copy(hash, f); err != nil {
		panic(errors.Wrap(err, "unable to copy to sha"))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

/*
func debug(i interface{}) {
	jsonStr, _ := json.Marshal(i)
	log.Printf(string(jsonStr))
}
*/

func authInfo(netlifyAccessToken string) runtime.ClientAuthInfoWriter {
	return runtime.ClientAuthInfoWriterFunc(func(r runtime.ClientRequest, _ strfmt.Registry) error {
		err := r.SetHeaderParam("User-Agent", "User-Agent: netlifyGolangDeploy/0.0.0")
		if err != nil {
			return errors.Wrap(err, "Unable to set useragent header")
		}

		err = r.SetHeaderParam("Authorization", "Bearer "+netlifyAccessToken)
		if err != nil {
			return errors.Wrap(err, "Unable to set authorization header")
		}

		return nil
	})
}

type uploadQueueAction func() error

type config struct {
	Token     string
	Site      string
	Directory string
	Branch    string
	Title     string
	QueueSize int
	Draft     bool
}

type shaData struct {
	realfilename string
	uri          string
}

func (cfg *config) findSite(siteName string) (*netlify.Site, error) {
	page := int32(1)
	perPage := int32(25)
	filter := "all"

	for {
		// List sites
		sites, err := netlifyClient().Operations.ListSites(
			operations.NewListSitesParams().WithPage(&page).WithPerPage(&perPage).WithFilter(&filter),
			authInfo(cfg.Token),
		)

		if err != nil {
			return nil, errors.Wrap(err, "Unable to get a list of sites")
		}

		if len(sites.GetPayload()) == 0 {
			return nil, nil
		}

		for _, site := range sites.GetPayload() {
			if site.Name == siteName {
				return site, nil
			}
		}

		page++
		log.Printf("[DEBUG] Site wasn't found, looking for site '%s' with page '%d' and per_page '%d'", siteName, page, perPage)
	}
}

func netlifyClient() *plumbing.Netlify {
	netlifyAPIHost := "api.netlify.com"
	netlifyAPIPath := "/api/v1"

	transport := openapiClient.New(netlifyAPIHost, netlifyAPIPath, plumbing.DefaultSchemes)
	client := plumbing.New(transport, strfmt.Default)

	return client
}

func (cfg *config) getDeploy(deployID string, wantedStatus string) (*netlify.Deploy, error) {
	for {
		deploy, err := netlifyClient().Operations.GetDeploy(
			operations.NewGetDeployParams().WithDeployID(deployID),
			authInfo(cfg.Token),
		)
		if err != nil {
			return nil, errors.Wrap(err, "Unable to check deploy")
		}

		if deploy.GetPayload().State == wantedStatus {
			// site is ready
			return deploy.GetPayload(), nil
		}

		// site is done somehow
		if deploy.GetPayload().State == "ready" {
			return deploy.GetPayload(), nil
		}

		time.Sleep(time.Duration(1) * time.Second)
	}
}

func (cfg *config) wrapUploadJob(deployID string, realFilename string, uri string) func() error {
	auth := authInfo(cfg.Token)

	return func() error {
		f, err := os.Open(realFilename)
		if err != nil {
			return errors.Wrap(err, "Unable to open file")
		}

		body := operations.NewUploadDeployFileParams().WithDeployID(deployID).WithPath(uri).WithFileBody(f)

		// initial 5 second delay - https://github.com/netlify/cli/blob/f563cc794fbcb8f9d716dc36a0f7d792f0cf325a/src/utils/deploy/constants.mjs#L14
		backoff := retry.NewFibonacci(5 * time.Second)

		// Ensure the maximum total retry time is 90s.
		// 90 second max from https://github.com/netlify/cli/blob/f563cc794fbcb8f9d716dc36a0f7d792f0cf325a/src/utils/deploy/constants.mjs#L16
		backoff = retry.WithMaxDuration(90*time.Second, backoff)

		ctx := context.Background()
		err = retry.Do(ctx, backoff, func(ctx context.Context) error {
			_, err = netlifyClient().Operations.UploadDeployFile(body, auth)
			if err != nil && strings.Contains(err.Error(), "GOAWAY") {
				return retry.RetryableError(err)
			}
			return err
		})

		return errors.Wrap(err, "Unable to upload file")
	}
}

func filesInDirectory(dir string) (map[string]string, map[string]*shaData, error) {
	filenameToSha := map[string]string{}
	shaToFilename := map[string]*shaData{}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		key, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		key = "/" + key
		filenameToSha[key] = mustGetSha1(path)
		shaToFilename[mustGetSha1(path)] = &shaData{
			realfilename: path,
			uri:          key,
		}

		return nil
	})

	return filenameToSha, shaToFilename, err
}

func main() {
	app := &cli.App{
		Name:   "deploy",
		Usage:  "deploy a directory to netlify",
		Action: deploy,
		Authors: []*cli.Author{
			{
				Name:  "Gavin Mogan",
				Email: "netlify-deployer@gavinmogan.com",
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "deployDir",
				Aliases: []string{"d"},
				Usage:   "directory to be deployed to netlify",
				EnvVars: []string{"NETLIFY_DIRECTORY"},
				Value:   "./public",
			},
			&cli.StringFlag{
				Name:        "token",
				Aliases:     []string{"t"},
				Usage:       "api token to connect to netlify",
				EnvVars:     []string{"NETLIFY_AUTH_TOKEN"},
				DefaultText: "[censored]",
				Required:    true,
			},
			&cli.StringFlag{
				Name:     "siteName",
				Aliases:  []string{"s"},
				Usage:    "Site name to deploy to",
				EnvVars:  []string{"NETLIFY_SITE"},
				Required: true,
			},
			&cli.StringFlag{
				Name:     "alias",
				Aliases:  []string{"a"},
				Usage:    "Site alias to deploy to",
				EnvVars:  []string{"NETLIFY_ALIAS"},
				Required: false,
			},
			&cli.StringFlag{
				Name:     "title",
				Usage:    "Title to label deploy as in logs",
				EnvVars:  []string{"NETLIFY_TITLE"},
				Required: false,
			},
			&cli.StringFlag{
				Name:     "queueSize",
				Usage:    "Number of parallel upload processes to use",
				EnvVars:  []string{"NETLIFY_QUEUE_SIZE"},
				Value:    "5",
				Required: false,
			},
			&cli.BoolFlag{
				Name:     "draft",
				Usage:    "Should this deployed as a draft?",
				EnvVars:  []string{"NETLIFY_DRAFT"},
				Value:    true, // old code forced it, we'll default it to false in the future
				Required: false,
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func deploy(c *cli.Context) error {
	cfg := config{
		Token:     c.String("token"),
		Site:      c.String("siteName"),
		Directory: c.String("deployDir"),
		Branch:    c.String("alias"),
		Title:     c.String("title"),
		QueueSize: c.Int("queueSize"),
		Draft:     c.Bool("draft"),
	}

	site, err := cfg.findSite(cfg.Site)
	if err != nil {
		return errors.Wrap(err, "Unable to find the site")
	}

	if site == nil {
		return fmt.Errorf("No site found for %s", cfg.Site)
	}

	filenameToSha, shaToFilename, err := filesInDirectory(cfg.Directory)

	if err != nil {
		return errors.Wrap(err, "Unable to walk directory")
	}

	deploy, err := netlifyClient().Operations.CreateSiteDeploy(
		operations.NewCreateSiteDeployParams().WithSiteID(site.ID).WithTitle(&cfg.Title).WithDeploy(&netlify.DeployFiles{
			Async:     true,
			Branch:    cfg.Branch,
			Draft:     cfg.Draft,
			Files:     filenameToSha,
			Functions: nil,
		}),
		authInfo(cfg.Token),
	)
	if err != nil {
		return errors.Wrap(err, "Unable to create deploy")
	}

	if deploy.GetPayload().State == "ready" {
		log.Print("Done deploying site to " + deploy.GetPayload().DeployURL)
	}

	deployID := deploy.GetPayload().ID

	preparedDeploy, err := cfg.getDeploy(deployID, "prepared")
	if err != nil {
		return errors.Wrap(err, "Unable to get deploy")
	}

	jobChan := make(chan uploadQueueAction, cfg.QueueSize)

	var wg sync.WaitGroup
	for i := 0; i < cfg.QueueSize; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for job := range jobChan {
				err := job()
				if err != nil {
					// FIXME - cancel everthing
					panic(err)
				}
			}
		}()
	}

	for _, sha := range preparedDeploy.Required {
		log.Printf("Enqueuing upload of %s", shaToFilename[sha].realfilename)
		jobChan <- cfg.wrapUploadJob(deployID, shaToFilename[sha].realfilename, shaToFilename[sha].uri)
	}

	close(jobChan)

	wg.Wait()

	log.Print("Done uploading. Waiting for site to be ready")

	_, err = cfg.getDeploy(deployID, "ready")

	if err != nil {
		return errors.Wrap(err, "finish deployment")
	}

	log.Printf("Site is deployed - %s", deploy.GetPayload().DeployURL)

	return nil
}
