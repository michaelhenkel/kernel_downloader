package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/artifactory"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/distribution"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/report"

	logrus "github.com/sirupsen/logrus"
	logging "ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/logger"
)

type OutputFormats struct {
	formats []map[string]string
}

func (f *OutputFormats) String() string {
	return fmt.Sprint(f.formats)
}

func (f *OutputFormats) Set(value string) error {
	option := strings.Split(strings.TrimSpace(value), ",")
	format := make(map[string]string)
	if len(option) > 1 {
		format[option[0]] = option[1]
	} else {
		format[option[0]] = ""
	}
	f.formats = append(f.formats, format)
	return nil
}

func (f *OutputFormats) Get() []map[string]string {
	return f.formats
}

var (
	kernelDefinitions  string
	artifactoryBaseURL string
	artSync            bool
	logLevel           string
	reportFormats      OutputFormats
)

func init() {
	flag.StringVar(&kernelDefinitions, "config", "./kernellist.yaml", "Defintions of kernel versions for which vrouter module should be built.")
	flag.StringVar(&artifactoryBaseURL, "artbaseurl", "https://svl-artifactory.juniper.net/artifactory/", "Artifactory base url")
	flag.BoolVar(&artSync, "artsync", false, "Upload to artifactory upstream kernel sources. Requires ARTIFACTORY_TOKEN env variable")
	flag.Var(&reportFormats, "format", "format_name[,output file path]. Known formats: table, json, yaml, csv")
	flag.StringVar(&logLevel, "loglevel", "info", "Log level: panic, fatal, error, warn, info, debug, trace")
}

func main() { os.Exit(mainWithReturnCode()) }

func mainWithReturnCode() int {
	flag.Parse()

	logger := logrus.New()
	if err := logger.Level.UnmarshalText([]byte(logLevel)); err != nil {
		log.Fatal(err)
	}
	logger.SetFormatter(&logrus.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})

	start := time.Now()
	artToken := os.Getenv("ARTIFACTORY_TOKEN")
	rhOfflineToken := os.Getenv("RH_OFFLINE_TOKEN")
	var kernelListTotal []*distribution.Kernel
	var existingKernels artifactory.ArtifactoryKernelCache
	var artMgr artifactory.ArtifactoryManger
	retryClient := distribution.GetHttpClientWithRetry(&logging.LeveledLogrus{logger}, 10)

	fileByte, err := os.ReadFile(kernelDefinitions)
	if err != nil {
		logger.Fatal(err)
	}
	distributions := distribution.Distributions{}
	if err := yaml.Unmarshal(fileByte, &distributions); err != nil {
		logger.Fatal(err)
	}

	if artSync {
		if artToken == "" {
			logger.Fatal("ARTIFACTORY_TOKEN env variable not set")
		}
		if distributions.ArtifactoryRepo == "" {
			logger.Fatal("artifactoryRepo not set in config file")
		}
		artMgr, err = artifactory.NewArtifactoryManger(&logging.LogrousWithOutput{logger}, artifactoryBaseURL, artToken)
		if err != nil {
			logger.Fatal(err)
		}
		existingKernels, err = artMgr.GetArtifactoryKernels(logger, distributions.ArtifactoryRepo)
		if err != nil {
			logger.Fatal(err)
		}
	}

	for _, distro := range distributions.Distributions {
		httpClient := retryClient
		if !artSync {
			if err := distro.UseArtifactoryCache(fmt.Sprintf("%s/%s", artifactoryBaseURL, distributions.ArtifactoryRepo)); err != nil {
				logger.Fatal(err)
			}
		}
		if distro.Name == string(distribution.RHEL) && artSync {
			if rhOfflineToken == "" {
				logger.Error("RH_OFFLINE_TOKEN env variable not defined")
				continue
			} else {
				httpClient = distribution.RhPackageClient(&logging.LeveledLogrus{logger}, rhOfflineToken)
			}
		}
		kernelList, err := distro.GetKernelList(httpClient, logger, artSync, existingKernels)
		if err != nil {
			logger.Fatal(err)
		}
		kernelListTotal = append(kernelListTotal, kernelList...)
	}

	if artSync {
		tempDir, err := ioutil.TempDir("", "kernels-")
		logger.Debugf("TEMP DIR: %s", tempDir)
		if err != nil {
			logger.Fatal(err)
		}
		defer os.RemoveAll(tempDir)
		downloadCount := 0
		for _, kernel := range kernelListTotal {
			if kernel.Downloaded {
				logger.Debugf("%s-%s: %s already in artifactory chache, or cache not enabled", kernel.Distro, kernel.DistroVersion, kernel.Name)
				continue
			}
			if err := kernel.Download(retryClient, logger, tempDir); err != nil {
				logger.Error(err)
			}
			downloadCount++
		}
		if downloadCount > 0 {
			totalUploaded, totalFailed, err := artMgr.UploadFiles(filepath.Join(distributions.ArtifactoryRepo, "{1}/"), filepath.Join(tempDir, "(**)/"))
			logger.Infof("Uploaded: %d, Failed %d, Error: %v\n", totalUploaded, totalFailed, err)
		} else {
			logger.Info("Nothing to upload")
		}
		return 0
	}

	for _, kernel := range kernelListTotal {
		if err := kernel.DownloadAndExtract(retryClient, logger); err != nil {
			logger.Error(err)
		}
	}

	for _, kernel := range kernelListTotal {
		baseString := `FROM debian:stretch
RUN apt update && apt install -y rpm2cpio cpio curl
`
		for k, v := range kernel.FileLocation {
			baseString += fmt.Sprintf("ADD %s %s\n", v, k)
		}
		dockerfile := fmt.Sprintf("images/Dockerfile.%s", kernel.Name)
		baseString += fmt.Sprintf("RUN %s\n", kernel.Command)
		baseString += fmt.Sprintf("RUN echo %s > /kernelpath\n", kernel.KernelPath)
		if err := os.WriteFile(dockerfile, []byte(baseString), 0644); err != nil {
			fmt.Println(err)
		}

	}

	os.Exit(0)

	for _, kernel := range kernelListTotal {
		if kernel.Extracted && kernel.Downloaded {
			if err := kernel.Compile(logger); err != nil {
				logger.Error(err)
			}
		}
	}
	result := report.Result{
		Kernels: kernelListTotal,
		Start:   start,
		End:     time.Now(),
	}

	for _, format := range reportFormats.Get() {
		for rType, outFile := range format {
			var err error
			var report string
			switch rType {
			case "table":
				report, err = result.TableReport()
			case "csv":
				report, err = result.CsvReport()
			case "json":
				report, err = result.JsonReport()
			case "yaml":
				report, err = result.YamlReport()
			default:
				err = fmt.Errorf("unknow report format: %s", rType)
			}
			if err != nil {
				logger.Error(err)
				return 2
			}
			if outFile != "" {
				err := ioutil.WriteFile(outFile, []byte(report), 0755)
				if err != nil {
					logger.Error(err)
					return 3
				}
			} else {
				fmt.Println(report)
			}
		}
	}

	var missingRequired []string
	for _, k := range result.Kernels {
		if k.Required && k.Compiled == distribution.FAIL {
			missingRequired = append(missingRequired, (k.Name + k.LocalVersion))
		}
	}
	if len(missingRequired) > 0 {
		logger.Fatalf("List of needed kernels for which vrouter module did not compile: %v", missingRequired)
	}
	return 0
}
