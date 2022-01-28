# Kernel downloader
Kernel downloader is used to pull kernel packages / sources and build vrouter kernel modules for them.

Kernel downloader uses external commands to unpack, install and compile so usually it is run inside container which provides all the dependencies.

## Configuration
Kernel downloader accepts configuration in format of yaml file passed through `-config` parameter. Example config:

```yaml
artifactoryRepo: cn2-static-dev/cn2/kernels/
distributions:
- name: centos
  parser:
  - kernel-devel-(.+).(el\w+\.x86_64).rpm
  versions:
  - name: 7
    minVersion: 3.10.0-1160
    maxVersion: 3.10.0-1160
    baseURL: https://mirrors.edge.kernel.org/centos/7/os/x86_64/Packages/
    artifactoryCache: true
  - name: 8.4.2105
    minVersion: 4.18.0-305.3.1
    maxVersion: 4.19.0-0.0.0
    baseURL: https://vault.centos.org/8.4.2105/BaseOS/x86_64/os/Packages
    artifactoryCache: true
  requiredVersions:
    - 3.10.0-1160.el7.x86_64
    - 3.10.0-1127.19.1.el7.x86_64
    - 4.18.0-305.3.1.el8.x86_64
    - 4.18.0-305.19.1.el8_4.x86_64
    - 4.18.0-305.25.1.el8_4.x86_64
```

Above configuration defines `centos` distribution for which kernel packages can be discovered by matching package name with pattern defined in `parser` property. This distribution contains 2 versions `7` and `8.4.2105` which corresponds to release model. Every `version` has a defined range of kernel versions (`minVersion` and `maxVersion`) so only packages within this range will be a targets for vrouter modules. The `baseURL` property points to external site from where packages can be downloaded. Every link (`<a href=`) on that site is checked with defined patterns in `parser`.

`requiredVersions` is a list of kernel versions for which vrouter module compilation must succeed, otherwise program will exit with error.

## Artifactory cache
If distribution version has `artifactoryCache` set to `true` instead of pulling kernel sources from `baseURL` the kernel downloader will fetch files from configured artifactory repository

To put new kernel sources in artifactory the kernel downloader can be run with `-artsync` flag. It will, for every distribution version which has `artifactoryCache` set to `true`, download kernel sources and store them in artifactory located at url passed through `artbaseurl` flag in repository defined in configuration key `artifactoryRepo`. Path to sources will be `[artbaseurl]/[artifactoryRepo]/[distribution name]/[version name]/[source file name]`. CN2 pipeline uses artifactory cache located at https://svl-artifactory.juniper.net/artifactory/cn2-static-dev/cn2/kernels/ which is updated by following pipeline: https://svl-jenkins-jcs.juniper.net/job/cn2-sync-kernels/ which runs every 8 hours. Artifactory token which allows upload should be passed by `ARTIFACTORY_TOKEN` env variable.

## CN2 pipeline
Kernel downloader is used in `kernel_build` makefile target. It produces 3 container images:
- `vrouter-kernel-modules` - contains all vrouter modules compiled during kernel downloader run, is later used in `vrouter_kernel_build` target where specific modules are extracted to dedicated images
- `vrouter-kernel-modules-results` - small image which contains just a reports from last run of kernel downloader, it is used by other stages to know which vrouter modules are available
- `kernels-artifactory-sync` - contain kernel downloader configured to update artifactory cache based on latest master configuration. The `ARTIFACTORY_TOKEN` and `RH_OFFLINE_TOKEN` env variable has to be passed. It is used in https://svl-jenkins-jcs.juniper.net/job/cn2-sync-kernels/

Kernel downloader results are cached like other artifacts of cn2 pipeline. This cache will be invalidated and full run happen when:
- vrouter code was changed
- kernel downloader config / code was changed
- information about last modification at `artifactoryRepo` fetched during pipeline run is different then the one stored in cache, which usually mean that new kernel sources were added
