package main

import (
	"crypto/sha1"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	env "github.com/caarlos0/env/v6"
	"github.com/go-openapi/runtime"
	openapiClient "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"github.com/netlify/open-api/go/models"
	netlify "github.com/netlify/open-api/go/models"
	"github.com/netlify/open-api/go/plumbing"
	"github.com/netlify/open-api/go/plumbing/operations"

	"github.com/pkg/errors"
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
	Token     string `env:"NETLIFY_AUTH_TOKEN"`
	Site      string `env:"NETLIFY_SITE"`
	Directory string `env:"NETLIFY_DIRECTORY" envDefault:"./public" envExpand:"true"`
}

type shaData struct {
	realfilename string
	uri          string
}

func (cfg *config) findSite(siteName string) (*netlify.Site, error) {
	page := int32(1)
	perPage := int32(25)

	for {
		// List sites
		sites, err := netlifyClient().Operations.ListSites(
			operations.NewListSitesParams().WithPage(&page).WithPerPage(&perPage),
			authInfo(cfg.Token),
		)

		if err != nil {
			return nil, errors.Wrap(err, "Unable to get a list of sites")
		}

		for _, site := range sites.GetPayload() {
			if site.Name == siteName {
				return site, nil
			}
		}

		page++
	}
}

func netlifyClient() *plumbing.Netlify {
	netlifyAPIHost := "api.netlify.com"
	netlifyAPIPath := "/api/v1"

	httpClient := &http.Client{}

	transport := openapiClient.NewWithClient(netlifyAPIHost, netlifyAPIPath, plumbing.DefaultSchemes, httpClient)
	client := plumbing.New(transport, strfmt.Default)

	return client
}

func (cfg *config) getDeploy(deployID string, wantedStatus string) (*models.Deploy, error) {
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

		log.Printf("deployID: %s, state: %s", deployID, deploy.GetPayload().State)
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

		_, err = netlifyClient().Operations.UploadDeployFile(body, auth)

		return errors.Wrap(err, "Unable to upload file")
	}
}

func main() {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		panic(errors.Wrap(err, "Unable to parse env"))
	}

	if len(cfg.Token) == 0 {
		panic(errors.New("Auth token is required"))
	}

	site, err := cfg.findSite(cfg.Site)
	if err != nil {
		panic(errors.Wrap(err, "Unable to find the site"))
	}

	title := "Preview Deploy"
	filenameToSha := map[string]string{}
	shaToFilename := map[string]*shaData{}
	err = filepath.Walk(cfg.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		key, err := filepath.Rel(cfg.Directory, path)
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
	if err != nil {
		panic(errors.Wrap(err, "Unable to walk directory"))
	}

	deploy, err := netlifyClient().Operations.CreateSiteDeploy(
		operations.NewCreateSiteDeployParams().WithSiteID(site.ID).WithTitle(&title).WithDeploy(&netlify.DeployFiles{
			Async:     true,
			Branch:    "preview-site-name",
			Draft:     true,
			Files:     filenameToSha,
			Functions: nil,
		}),
		authInfo(cfg.Token),
	)
	if err != nil {
		panic(errors.Wrap(err, "Unable to create deploy"))
	}

	if deploy.GetPayload().State == "ready" {
		log.Print("Done deploying site to " + deploy.GetPayload().DeployURL)
	}

	deployID := deploy.GetPayload().ID

	preparedDeploy, err := cfg.getDeploy(deployID, "prepared")
	if err != nil {
		panic(errors.Wrap(err, "Unable to get deploy"))
	}

	queueSize := 5
	jobChan := make(chan uploadQueueAction, queueSize)

	var wg sync.WaitGroup
	for i := 0; i < queueSize; i++ {
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

	log.Print("Done uploading")

	_, err = cfg.getDeploy(deployID, "ready")

	if err != nil {
		panic(errors.Wrap(err, "finish deployment"))
	}

	log.Printf("Done deploying site to %s", deploy.GetPayload().DeployURL)
}
