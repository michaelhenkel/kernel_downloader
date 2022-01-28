package distribution

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/google/go-github/v39/github"
	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/net/html"

	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/artifactory"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/logger"
)

type Distro string
type FileType string
type Status bool

const (
	UBUNTU   Distro   = "ubuntu"
	CENTOS   Distro   = "centos"
	RHEL     Distro   = "rhel"
	MINIKUBE Distro   = "minikube"
	DEB      FileType = "deb"
	RPM      FileType = "rpm"
	TGZ      FileType = "tgz"
	FAIL     Status   = false
	SUCCESS  Status   = true
)

type Distributions struct {
	Distributions   []Distribution `yaml:"distributions"`
	ArtifactoryRepo string         `yaml:"artifactoryRepo"`
}

type Kernel struct {
	Name             string
	Files            []string
	Distro           Distro
	KernelPath       string
	Compiled         Status
	Errormsg         string
	Downloaded       Status
	Extracted        Status
	DistroVersion    string
	MinikubeVersions []string
	LocalVersion     string
	CustomConfig     map[string]string
	Required         bool
	Command          string
	FileLocation     map[string]string
}

type Distribution struct {
	Name             string    `yaml:"name"`
	Versions         []Version `yaml:"versions"`
	Parser           []string  `yaml:"parser"`
	RequiredVersions []string  `yaml:"requiredVersions"`
}

type Version struct {
	Name               string         `yaml:"name"`
	MinVersion         string         `yaml:"minVersion"`
	MaxVersion         string         `yaml:"maxVersion"`
	ExtraVersions      []string       `yaml:"extraVersions"`
	BaseURL            string         `yaml:"baseURL"`
	KernelURL          string         `yaml:"kernelURL"`
	DefconfigURL       string         `yaml:"defconfigURL"`
	KernelDefconfigURL string         `yaml:"kernelDeconfigURL"`
	ArtifactoryCache   bool           `yaml:"artifactoryCache"`
	RhRepository       string         `yaml:"rhRepository"`
	CustomConfigs      []CustomConfig `yaml:"customConfigs"`
}

type CustomConfig struct {
	KernelName         string            `yaml:"kernelName"`
	LocalVersionSuffix string            `yaml:"localVersionSuffix"`
	Properties         map[string]string `yaml:"properties"`
}

var gccMap = map[string]string{
	"5": "7",
	"4": "7",
	"3": "4.9",
}

var minikubeMap = make(map[string][]string)

func GetHttpClientWithRetry(logger logger.LeveledLogger, retryNum int) *http.Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = retryNum
	retryClient.Logger = logger
	return retryClient.StandardClient()
}

func downloadFile(client *http.Client, logger logger.Logger, filepath string, url string) error {
	if _, err := os.Stat(filepath); os.IsNotExist(err) {
		logger.Debugf("Download url: %s, dest file: %s", url, filepath)
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		out, err := os.Create(filepath)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, resp.Body); err != nil {
			return err
		}
	} else {
		return err
	}
	return nil
}

func (k *Kernel) Compile(logger logger.Logger) error {
	var destKernelName string
	if k.LocalVersion != "" {
		destKernelName = k.Name + k.LocalVersion
	} else {
		destKernelName = k.Name
	}
	switch k.Distro {
	case MINIKUBE:
		logger.Infof("compiling kernel %s for minikube", destKernelName)
		if err := os.Chdir(k.KernelPath); err != nil {
			return err
		}
		// Copy fresh conifg
		srcFile, err := os.Open("../linux_defconfig")
		if err != nil {
			return err
		}
		defer srcFile.Close()
		destFile, err := os.Create(k.KernelPath + "/.config")
		if err != nil {
			return err
		}
		defer destFile.Close()
		if _, err := io.Copy(destFile, srcFile); err != nil {
			return err
		}

		if k.CustomConfig != nil {
			if err := prepareCustomConfig(logger, ".config", k.CustomConfig); err != nil {
				return err
			}
		}
		makeOldConfig := []string{"make", "olddefconfig"}
		if err := runner(logger, makeOldConfig); err != nil {
			return err
		}
		make := []string{"make", "-j", strconv.Itoa(runtime.NumCPU()), "prepare", "headers_install", "scripts"}
		if err := runner(logger, make); err != nil {
			return err
		}
	}
	kverList := strings.Split(k.Name, ".")
	updateGCC := []string{"update-alternatives", "--set", "gcc", fmt.Sprintf("/usr/bin/gcc-%s", gccMap[kverList[0]])}
	if err := runner(logger, updateGCC); err != nil {
		return err
	}
	if err := os.Remove("/tf-dev-env/vrouter/Module.symvers"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Chdir("/tf-dev-env"); err != nil {
		return err
	}

	logger.Infof("compiling vrouter kernel module for kernel %s", destKernelName)
	scons := []string{"scons", fmt.Sprintf("--kernel-dir=%s", k.KernelPath), "--c++=c++11", "--opt=production", fmt.Sprintf("-j%d", runtime.NumCPU()), "vrouter/vrouter.ko"}
	if err := runner(logger, scons); err != nil {
		k.Compiled = FAIL
		k.Errormsg = fmt.Sprintf("%s", err)
		logger.Errorf("failed to compile vrouter kernel module for kernel: %s", k.Name)
		return err
	} else {
		k.Compiled = SUCCESS
	}
	if err := os.MkdirAll(fmt.Sprintf("/kernelmodules/%s", destKernelName), 0755); err != nil && !os.IsExist(err) {
		return err
	}
	srcFile, err := os.Open("vrouter/vrouter.ko")
	if err != nil {
		return err
	}
	defer srcFile.Close()
	destFile, err := os.Create(fmt.Sprintf("/kernelmodules/%s/vrouter.ko", destKernelName))
	if err != nil {
		return err
	}
	defer destFile.Close()
	if _, err := io.Copy(destFile, srcFile); err != nil {
		return err
	}
	return nil
}

func runner(logger logger.Logger, cmdList []string) error {
	logger.Debugf("runnning: %v", cmdList)
	cmd := exec.Command(cmdList[0], cmdList[1:]...)
	cmdOut, err := cmd.CombinedOutput()
	// log limit 1MiB reached
	// logger.Debugf("output: %s", string(cmdOut))
	if err != nil {
		return fmt.Errorf("err %s %s", err, string(cmdOut))
	}
	return nil
}

func destFileName(fileURL string) (string, error) {
	u, err := url.Parse(fileURL)
	if err != nil {
		return "", err
	}
	return filepath.Base(u.Path), nil
}

func (k *Kernel) Download(client *http.Client, logger logger.Logger, baseDir string) error {
	kernelDir := fmt.Sprintf("%s/%s/%s", baseDir, k.Distro, k.DistroVersion)
	if err := os.MkdirAll(kernelDir, 0755); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	for _, kernelFile := range k.Files {
		fileName, err := destFileName(kernelFile)
		if err != nil {
			k.Downloaded = FAIL
			return err
		}
		fileLocation := fmt.Sprintf("%s/%s", kernelDir, fileName)
		if err := downloadFile(client, logger, fileLocation, kernelFile); err != nil {
			k.Downloaded = FAIL
			return err
		}
		k.Downloaded = SUCCESS
	}
	return nil
}

func (k *Kernel) DownloadAndExtract(client *http.Client, logger logger.Logger) error {
	baseKernelDir := "/tmp/kernel"
	if err := os.Mkdir(baseKernelDir, 0755); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	kernelDir := fmt.Sprintf("%s/%s", baseKernelDir, k.Name)
	if err := os.Mkdir(kernelDir, 0755); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	for _, kernelFile := range k.Files {
		fileName, err := destFileName(kernelFile)
		if err != nil {
			k.Downloaded = FAIL
			return err
		}
		fileLocation := fmt.Sprintf("%s/%s", kernelDir, fileName)
		if k.FileLocation == nil {
			k.FileLocation = make(map[string]string)
		}
		k.FileLocation[fileLocation] = kernelFile
		/*
			if err := downloadFile(client, logger, fileLocation, kernelFile); err != nil {
				k.Downloaded = FAIL
				return err
			}
		*/
		k.Downloaded = SUCCESS
		//mtype, err := mimetype.DetectFile(fileLocation)
		fileExtension := filepath.Ext(fileLocation)
		if err != nil {
			return err
		}
		switch fileExtension {
		case ".gz":
			k.Command = fmt.Sprintf("tar zxvf -C %s %s", kernelFile, fileLocation)
			/*
				if err := extractTGZ(logger, kernelDir, fileLocation); err != nil {
					k.Extracted = FAIL
					return err
				}
			*/
			k.Extracted = SUCCESS
		case ".rpm":
			r, err := regexp.Compile(`kernel-devel-(.+).(el\w+).x86_64.rpm`)
			if err != nil {
				return err
			}
			version := r.FindStringSubmatch(fileLocation)
			if len(version) > 1 {
				k.KernelPath = fmt.Sprintf("%s/usr/src/kernels/%s.%s.x86_64", kernelDir, version[1], version[2])
			}
			/*
				if err := os.Chdir(kernelDir); err != nil {
					return err
				}
				if err := os.RemoveAll(kernelDir + "/usr"); err != nil && !os.IsNotExist(err) {
					return err
				}
			*/
			//rpm2Cpio := []string{"sh", "-c", fmt.Sprintf("rpm2cpio %s | cpio -idmv", fileLocation)}
			/*
				logger.Infof("extracting %s", fileLocation)
				if err := runner(logger, rpm2Cpio); err != nil {
					k.Extracted = FAIL
					return err
				}
			*/
			k.Command = fmt.Sprintf("rpm2cpio %s | cpio -idmv", fileLocation)
			k.Extracted = SUCCESS
		}
	}
	switch k.Distro {
	case MINIKUBE:
		if k.Downloaded && k.Extracted {
			k.KernelPath = fmt.Sprintf("%s/linux-%s", kernelDir, k.Name)
		}
	case UBUNTU:
		if k.Downloaded {
			installHeaders := []string{"dpkg", "-i", fmt.Sprintf("%s/%s", kernelDir, filepath.Base(k.Files[0])), fmt.Sprintf("%s/%s", kernelDir, filepath.Base(k.Files[1]))}
			/*
				logger.Infof("extracting %s and %s", filepath.Base(k.Files[0]), filepath.Base(k.Files[1]))
				if err := runner(logger, installHeaders); err != nil {
					k.Extracted = FAIL
					return err
				}
			*/
			k.Extracted = SUCCESS
			k.KernelPath = fmt.Sprintf("/usr/src/linux-headers-%s-generic", k.Name)
			k.Command = strings.Join(installHeaders, " ")
		}
	}
	return nil
}

func (d *Distribution) UseArtifactoryCache(artifactoryRepoUrl string) error {
	base, err := url.Parse(artifactoryRepoUrl)
	if err != nil {
		return err
	}
	for i := range d.Versions {
		if d.Versions[i].ArtifactoryCache {
			path, err := url.Parse(fmt.Sprintf("%s/%s", d.Name, d.Versions[i].Name))
			if err != nil {
				return err
			}
			url := base.ResolveReference(path).String()
			if d.Name == string(MINIKUBE) {
				d.Versions[i].KernelURL = url
			} else {
				d.Versions[i].BaseURL = url
			}
		}
	}
	return nil
}

func hrefList(client *http.Client, baseURL string) ([]string, error) {
	response, err := getHttpStringRespone(client, baseURL)
	if err != nil {
		return nil, err
	}
	fileList, err := getFileList(response)
	if err != nil {
		return nil, err
	}

	return fileList, nil
}

func minikubeList(client *http.Client, baseURL string) ([]string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	var fileList []string
	if u.Host == "github.com" {
		p := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(p) < 2 {
			return nil, fmt.Errorf("unable to find owner and repository in provided url: %s", baseURL)
		}
		fileList, err = getMinikubeTags(client, p[0], p[1])
		if err != nil {
			return nil, err
		}
	} else {
		fileList, err = hrefList(client, baseURL)
		if err != nil {
			return nil, err
		}
	}
	return fileList, nil

}

func (d *Distribution) GetKernelList(client *http.Client, logger logger.Logger, upstream bool, cachedKernels artifactory.ArtifactoryKernelCache) ([]*Kernel, error) {
	var kernelList []*Kernel
	for _, version := range d.Versions {
		var downloadFileList map[string][]string
		if !upstream && d.Name != string(MINIKUBE) && version.ArtifactoryCache {
			// Fetch from artifactory
			fileList, err := hrefList(client, version.BaseURL)
			if err != nil {
				return nil, err
			}
			downloadFileList, err = d.parse(fileList, version)
			if err != nil {
				logger.Errorf("%v", err)
				return nil, err
			}
		} else {
			switch d.Name {
			case string(RHEL):
				rhPackages, err := getRhelPackages(client, version.RhRepository, "kernel-devel")
				if err != nil && len(rhPackages) < 1 {
					return kernelList, err
				}
				if err != nil {
					// Multiple request are done, but only some of them contain information about kernel packages
					logger.Errorf("RedHat package fetch error: %v", err)
				}
				downloadFileList, err = parseRedHatPackages(client, rhPackages, version, d.Parser, cachedKernels)
				if err != nil {
					return kernelList, err
				}
			case string(UBUNTU), string(CENTOS):
				fileList, err := hrefList(client, version.BaseURL)
				if err != nil {
					return nil, err
				}
				downloadFileList, err = d.parse(fileList, version)
				if err != nil {
					return nil, err
				}
			case string(MINIKUBE):
				fileList, err := minikubeList(client, version.BaseURL)
				if err != nil {
					return nil, err
				}
				downloadFileList, err = d.parse(fileList, version)
				if err != nil {
					return nil, err
				}
				downloadFileList, err = getMinikubeKernelFile(downloadFileList, version.KernelURL, version.DefconfigURL, version.KernelDefconfigURL)
				if err != nil {
					return nil, err
				}
			}
		}
		for k, v := range downloadFileList {
			// build with default config
			var downloaded bool
			if upstream {
				if version.ArtifactoryCache && checkIfKernelInArtifactory(d.Name, version.Name, v, cachedKernels) {
					downloaded = true
				} else if !version.ArtifactoryCache {
					// skip download if cache not enabled for version
					downloaded = true
				}
			}
			kernel := &Kernel{
				Name:          k,
				Files:         v,
				Distro:        Distro(d.Name),
				DistroVersion: version.Name,
				Downloaded:    Status(downloaded),
			}
			if mkVersions, ok := minikubeMap[k]; ok {
				kernel.MinikubeVersions = mkVersions
			}
			// Ubuntu reports kernel version with -generic suffix
			if d.Name == string(UBUNTU) {
				kernel.LocalVersion = "-generic"
			}
			kernelList = append(kernelList, kernel)

			if version.CustomConfigs != nil {
				for _, cc := range version.CustomConfigs {
					if cc.KernelName == k {
						cc.Properties["CONFIG_LOCALVERSION"] = cc.LocalVersionSuffix
						// build with custom config
						kernel := &Kernel{
							Name:          k,
							Files:         v,
							Distro:        Distro(d.Name),
							DistroVersion: version.Name,
							LocalVersion:  cc.LocalVersionSuffix,
							CustomConfig:  cc.Properties,
							Downloaded:    Status(downloaded),
						}
						if mkVersions, ok := minikubeMap[k]; ok {
							kernel.MinikubeVersions = mkVersions
						}
						kernelList = append(kernelList, kernel)
					}
				}
			}
		}
	}
	if d.RequiredVersions != nil {
		for _, rv := range d.RequiredVersions {
			found := false
			for _, k := range kernelList {
				kernelName := k.Name + k.LocalVersion
				if rv == kernelName {
					k.Required = true
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("kernel %s is defined as required, but not present in the any defined versions for %s distribution", rv, d.Name)
			}
		}
	}
	return kernelList, nil
}

func checkIfKernelInArtifactory(distro, version string, kernelFiles []string, artifactoryKernels artifactory.ArtifactoryKernelCache) bool {
	if artifactoryKernels == nil || artifactoryKernels.Empty() {
		return false
	}
	/*
		if distro == string(MINIKUBE) && len(kernelFiles) > 1 {
			// linux_defconfig is a second file
			kernelFiles = kernelFiles[:1]
		}
	*/
	for _, f := range kernelFiles {
		fname, err := destFileName(f)
		if err != nil {
			continue
		}
		if !artifactoryKernels.InCache(distro, version, fname) {
			return false
		}
	}
	return true
}

func getMinikubeKernelFile(downloadFileList map[string][]string, kernelURL, defconfigURL, kernelDefconfigURL string) (map[string][]string, error) {
	var newDownloadFileList = make(map[string][]string)
	var fileMap = make(map[string]struct{})
	for version := range downloadFileList {
		minikubeDefconfURL := fmt.Sprintf(defconfigURL, version)
		response, err := http.Get(minikubeDefconfURL)
		if err != nil {
			return nil, err
		}
		responseByte, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		r, err := regexp.Compile(`BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE="(.*)"`)
		if err != nil {
			return nil, err
		}
		versionMatch := r.FindStringSubmatch(string(responseByte))
		if len(versionMatch) > 1 {
			kernelFileURL := fmt.Sprintf("%s/linux-%s.tar.gz", kernelURL, versionMatch[1])

			minikubeMap[versionMatch[1]] = append(minikubeMap[versionMatch[1]], version)
			if _, ok := fileMap[filepath.Base(kernelFileURL)]; !ok {
				newDownloadFileList[versionMatch[1]] = append(newDownloadFileList[versionMatch[1]], kernelFileURL)
				fileMap[filepath.Base(kernelFileURL)] = struct{}{}
			}
			kernelDefconfigString := fmt.Sprintf(kernelDefconfigURL, version)
			if _, ok := fileMap[versionMatch[1]+filepath.Base(kernelDefconfigString)]; !ok {
				newDownloadFileList[versionMatch[1]] = append(newDownloadFileList[versionMatch[1]], kernelDefconfigString)
				fileMap[versionMatch[1]+filepath.Base(kernelDefconfigString)] = struct{}{}
			}
		}

	}
	return newDownloadFileList, nil
}

func (d *Distribution) parse(fileList []string, version Version) (map[string][]string, error) {
	var kernelMap = make(map[string][]string)
	for _, file := range fileList {
		for _, parser := range d.Parser {
			valid, versionMatch, err := validateVersion(file, parser, version.MinVersion, version.MaxVersion)
			if err != nil {
				return nil, err
			}
			if valid {
				if len(versionMatch) == 3 {
					kernelMap[versionMatch[1]+"."+versionMatch[2]] = append(kernelMap[versionMatch[1]+"."+versionMatch[2]], version.BaseURL+"/"+versionMatch[0])
				} else {
					kernelMap[versionMatch[1]] = append(kernelMap[versionMatch[1]], version.BaseURL+"/"+versionMatch[0])
				}
			}
		}
	}
	return kernelMap, nil
}

func validateVersion(file, parser, minVerStr, maxVerStr string) (bool, []string, error) {
	r, err := regexp.Compile(parser)
	if err != nil {
		return false, nil, err
	}
	versionMatch := r.FindStringSubmatch(file)
	if len(versionMatch) < 1 {
		return false, versionMatch, nil
	}
	minVer, err := semver.NewVersion(minVerStr)
	if err != nil {
		return false, versionMatch, fmt.Errorf("wrong min ver %s %s", minVerStr, err)
	}
	maxVer, err := semver.NewVersion(maxVerStr)
	if err != nil {
		return false, versionMatch, fmt.Errorf("wrong max ver %s %s", maxVerStr, err)
	}
	currentVer, err := semver.NewVersion(versionMatch[1])
	if err != nil {
		if semver.ErrInvalidSemVer == err {
			return false, versionMatch, nil
		}
		return false, versionMatch, fmt.Errorf("wrong current ver %s %s", versionMatch[1], err)
	}
	valid := true
	if currentVer.LessThan(minVer) || currentVer.GreaterThan(maxVer) {
		valid = false
	}
	return valid, versionMatch, nil
}

func getMinikubeTags(client *http.Client, githubOwner, repo string) ([]string, error) {
	gclient := github.NewClient(client)
	opt := &github.ListOptions{PerPage: 99}
	var allTags []*github.RepositoryTag
	for {
		tags, resp, err := gclient.Repositories.ListTags(context.Background(), githubOwner, repo, opt)
		if err != nil {
			return nil, err
		}
		allTags = append(allTags, tags...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	var tags []string
	for _, tag := range allTags {
		tags = append(tags, *tag.Name)
	}
	return tags, nil
}

func getHttpStringRespone(client *http.Client, url string) (string, error) {
	response, err := client.Get(url)
	if err != nil {
		return "", err
	}
	responseByte, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return string(responseByte), nil
}
func getFileList(content string) ([]string, error) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	var fileList []string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					fileList = append(fileList, a.Val)
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return fileList, nil
}

func extractTGZ(logger logger.Logger, dst, filename string) error {
	logger.Debugf("extracting %s", filename)
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		case header == nil:
			continue
		}
		target := filepath.Join(dst, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				return err
			}
			f.Close()
		}
	}
}

func prepareCustomConfig(logger logger.Logger, configFilePath string, configuration map[string]string) error {
	input, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		return err
	}
	defaultConfig := make(map[string]string)
	lines := strings.Split(string(input), "\n")
	r, err := regexp.Compile(`^#\s+(.*)\s+is not set`)
	if err != nil {
		return err
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case len(line) == 0:
			continue
		case strings.HasPrefix(line, "#"):
			match := r.FindStringSubmatch(line)
			if len(match) > 1 {
				defaultConfig[match[1]] = "n"
			}
			continue
		}
		splits := strings.Split(line, "=")
		if len(splits) < 2 {
			logger.Errorf("unable to parse kernel config line: %s", line)
			continue
		}
		defaultConfig[splits[0]] = splits[1]
	}

	for k, v := range configuration {
		defaultConfig[k] = v
	}

	f, err := os.Create(configFilePath)

	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	for key, value := range defaultConfig {
		w.WriteString(key + "=" + escapeStringValue(value) + "\n")
	}

	return w.Flush()
}

func escapeStringValue(value string) string {
	switch value {
	case "y", "n", "m", "Y", "N", "M":
		return value
	}
	if strings.HasPrefix(value, "-") {
		if _, err := strconv.ParseInt(value, 0, 64); err == nil {
			return value
		}
	} else {
		if _, err := strconv.ParseUint(value, 0, 64); err == nil {
			return value
		}
	}

	if len(value) > 0 && value[0] == '"' && value[len(value)-1] == '"' {
		return value
	}
	return "\"" + value + "\""
}
