package artifactory

import (
	"fmt"
	"strings"

	"github.com/jfrog/jfrog-client-go/artifactory"
	artAuth "github.com/jfrog/jfrog-client-go/artifactory/auth"
	artServices "github.com/jfrog/jfrog-client-go/artifactory/services"
	artUtils "github.com/jfrog/jfrog-client-go/artifactory/services/utils"
	artConfig "github.com/jfrog/jfrog-client-go/config"
	artLog "github.com/jfrog/jfrog-client-go/utils/log"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/logger"
)

type ArtifactoryManger struct {
	manager artifactory.ArtifactoryServicesManager
}

type ArtifactoryKernelCache interface {
	Empty() bool
	InCache(distro, version, fileName string) bool
	InCacheSumCheck(distro, version, fileName, chksum string) bool
	Set(kernels map[string]string)
}
type artifactoryKernelCache struct {
	kernels map[string]string
}

func (a *artifactoryKernelCache) Empty() bool { return a.kernels == nil }
func (a *artifactoryKernelCache) Set(kernels map[string]string) {
	a.kernels = kernels
}
func (a *artifactoryKernelCache) InCache(distro, version, fileName string) bool {
	keyName := fmt.Sprintf("%s_%s_%s", distro, version, fileName)
	if _, ok := a.kernels[keyName]; ok {
		return true
	}
	return false
}

func (a *artifactoryKernelCache) InCacheSumCheck(distro, version, fileName, chksum string) bool {
	keyName := fmt.Sprintf("%s_%s_%s", distro, version, fileName)
	asum := a.kernels[keyName]
	return chksum == asum

}

func NewArtifactoryManger(logger logger.LoggerWithOutput, artifactoryBaseURL, artifactoryToken string) (ArtifactoryManger, error) {
	var mgr ArtifactoryManger
	artLog.SetLogger(logger)

	rtDetails := artAuth.NewArtifactoryDetails()
	rtDetails.SetUrl(artifactoryBaseURL)
	rtDetails.SetApiKey(artifactoryToken)
	serviceConfig, err := artConfig.NewConfigBuilder().
		SetServiceDetails(rtDetails).
		Build()
	if err != nil {
		return mgr, fmt.Errorf("unable create service config: %v", err)
	}
	rtManager, err := artifactory.New(serviceConfig)
	if err != nil {
		return mgr, fmt.Errorf("unable create manger: %v", err)
	}
	mgr = ArtifactoryManger{rtManager}
	return mgr, nil
}

func (a *ArtifactoryManger) UploadFiles(repoPath, localPath string) (int, int, error) {
	params := artServices.NewUploadParams()
	params.Pattern = localPath
	params.Target = repoPath
	/*
		Flat
		If true, files are uploaded to the exact target path specified and their hierarchy in the source file system is ignored.
		If false, files are uploaded to the target path while maintaining their file system hierarchy.
	*/
	params.Flat = false
	params.Recursive = true
	params.IncludeDirs = true
	params.Ant = true
	params.ChecksumsCalcEnabled = true
	return a.manager.UploadFiles(params)
}

func getDistVersionFromPath(path string) (string, string, error) {
	pathEL := strings.Split(path, "/")
	if len(pathEL) < 2 {
		return "", "", fmt.Errorf("something wrong with this path: %s", path)
	}
	return pathEL[len(pathEL)-2], pathEL[len(pathEL)-1], nil
}

func (a *ArtifactoryManger) GetArtifactoryKernels(logger logger.Logger, repo string) (*artifactoryKernelCache, error) {
	fileMap := make(map[string]string)
	artKernels := &artifactoryKernelCache{}
	params := artServices.NewSearchParams()
	params.Recursive = true
	params.Pattern = repo
	reader, err := a.manager.SearchFiles(params)
	if err != nil {
		return artKernels, err
	}
	defer func() {
		if reader != nil {
			err = reader.Close()
		}
	}()
	// Iterate over the results.
	for currentResult := new(artUtils.ResultItem); reader.NextRecord(currentResult) == nil; currentResult = new(artUtils.ResultItem) {
		distro, version, err := getDistVersionFromPath(currentResult.Path)
		if err != nil {
			logger.Errorf("can`t use this result: %s err: %v", currentResult.Path, err)
			continue
		}
		keyName := fmt.Sprintf("%s_%s_%s", distro, version, currentResult.Name)
		fileMap[keyName] = currentResult.Sha256
	}
	if err := reader.GetError(); err != nil {
		return artKernels, err
	}
	// Resets the reader pointer back to the beginning of the output. Make sure not to call this method after the reader had been closed using ```reader.Close()```
	reader.Reset()
	if len(fileMap) > 0 {
		artKernels.Set(fileMap)
	}
	return artKernels, nil
}
