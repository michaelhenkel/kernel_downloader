package distribution

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/artifactory"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/logger"
)

var (
	config = oauth2.Config{
		ClientID: "rhsm-api",
		Scopes:   []string{"refresh_token"},
		Endpoint: oauth2.Endpoint{
			TokenURL: "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/token"}}
)

const (
	RH_API_THREADS = 3
)

type RepoContent struct {
	Pagination Pagination  `json:"pagination"`
	Packages   []RhPackage `json:"body"`
}
type Pagination struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Count  int `json:"count"`
}
type RhPackage struct {
	Arch         string   `json:"arch"`
	Checksum     string   `json:"checksum"`
	ContentSets  []string `json:"contentSets"`
	Epoch        string   `json:"epoch"`
	Name         string   `json:"name"`
	Release      string   `json:"release"`
	Version      string   `json:"version"`
	Href         string   `json:"href"`
	DownloadHref string   `json:"downloadHref"`
}
type DownloadInfo struct {
	File FileInfo `json:"body"`
}
type FileInfo struct {
	Expiration time.Time `json:"expiration"`
	Filename   string    `json:"filename"`
	Href       string    `json:"href"`
}

func RhPackageClient(logger logger.LeveledLogger, rhOfflineToken string) *http.Client {
	restoredToken := &oauth2.Token{
		RefreshToken: rhOfflineToken,
	}
	// https://github.com/hashicorp/go-retryablehttp/pull/128#issuecomment-796527518
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 5
	retryClient.Logger = logger
	retryClient.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	stdClient := retryClient.StandardClient()
	stdClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	parent := context.Background()
	/*
		oauth2 client will use RoundTripper from retryablehttp
		https://github.com/golang/oauth2/issues/324
		if v := client.Transport.(*oauth2.Transport).Base.(*retryablehttp.RoundTripper).Client; v != retryClient {
			fmt.Printf("expected %v, got %v", retryClient, v)
		}
		Set reasonable timeout https://github.com/golang/oauth2/issues/368
	*/
	ctx := context.WithValue(parent, oauth2.HTTPClient, stdClient)
	tokenSource := config.TokenSource(ctx, restoredToken)
	oa2Client := oauth2.NewClient(ctx, tokenSource)
	client := &http.Client{
		Transport: oa2Client.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client
}

func getRhelPackages(client *http.Client, repo, pattern string) ([]RhPackage, error) {
	ctx := context.Background()
	var mutex sync.RWMutex
	var rhPackages []RhPackage
	r, _ := regexp.Compile(pattern)
	g, _ := errgroup.WithContext(ctx)

	for i := 0; i < RH_API_THREADS; i++ {
		segmentNum := i * 100
		g.Go(func() error {
			offset := segmentNum

			for {
				var kernels []RhPackage
				resp, err := getEntries(client, repo, offset)
				if err != nil {
					return err
				}
				for _, pkg := range resp.Packages {
					if r.MatchString(pkg.Name) {
						kernels = append(kernels, pkg)
					}
				}
				mutex.Lock()
				rhPackages = append(rhPackages, kernels...)
				mutex.Unlock()
				if resp.Pagination.Count < resp.Pagination.Limit {
					break
				}
				offset = offset + RH_API_THREADS*100
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return rhPackages, fmt.Errorf("at least one request failed, results can be incomplite: %v", err)
	}
	return rhPackages, nil
}

func getEndpoint(repo string, offset int) string {
	return fmt.Sprintf("https://api.access.redhat.com/management/v1/packages/cset/%s/arch/x86_64?limit=100&offset=%d", repo, offset)
}
func getEntries(client *http.Client, repo string, offset int) (RepoContent, error) {
	var response RepoContent
	res, err := client.Get(getEndpoint(repo, offset))
	if err != nil {
		return response, err
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		log.Fatal(readErr)
		return response, err
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return response, err
	}
	return response, nil
}

func parseRedHatPackages(client *http.Client, packages []RhPackage, version Version, parsers []string, artifactoryCache artifactory.ArtifactoryKernelCache) (map[string][]string, error) {
	kernelMap := make(map[string][]string)
	for _, rhp := range packages {
		fileName := rhp.fileName()
		for _, parser := range parsers {
			valid, versionMatch, err := validateVersion(
				fileName, parser, version.MinVersion, version.MaxVersion)
			if err != nil {
				return nil, err
			}
			if valid {
				var downloadUrl string
				if !artifactoryCache.Empty() && artifactoryCache.InCacheSumCheck(string(RHEL), version.Name, fileName, rhp.Checksum) {
					// prevent additional request when package already in artifactory
					downloadUrl = rhp.fileName()
				} else {
					downloadUrl, err = rhp.getAuthDownloadUrl(client)
					if err != nil {
						return nil, err
					}
				}
				if len(versionMatch) == 3 {
					kernelMap[versionMatch[1]+"."+versionMatch[2]] = append(kernelMap[versionMatch[1]+"."+versionMatch[2]], downloadUrl)
				} else {
					kernelMap[versionMatch[1]] = append(kernelMap[versionMatch[1]], downloadUrl)
				}
			}
		}

	}

	return kernelMap, nil
}

func (p RhPackage) fileName() string {
	return fmt.Sprintf("%s-%s-%s.%s.rpm", p.Name, p.Version, p.Release, p.Arch)
}

func (p RhPackage) getAuthDownloadUrl(client *http.Client) (string, error) {
	var downloadInfo DownloadInfo
	res, err := client.Get(p.DownloadHref)
	if err != nil {
		return "", err
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		log.Fatal(readErr)
		return "", err
	}
	if err := json.Unmarshal(body, &downloadInfo); err != nil {
		return "", err
	}
	return downloadInfo.File.Href, nil
}
