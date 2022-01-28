ARG REGISTRY=svl-artifactory.juniper.net/atom-docker/cn2
ARG TAG=distroless
FROM ${REGISTRY}/golang:1.16-buster as build
ENV GOPATH=/go
ENV GOPROXY=https://svl-artifactory.juniper.net/artifactory/api/go/go,direct
RUN mkdir -p /go/src/ssd-git.juniper.net/contrail/cn2/build/kernel_downloader
COPY . /go/src/ssd-git.juniper.net/contrail/cn2/build/kernel_downloader
COPY go.mod /
COPY go.sum /
RUN cd /go/src/ssd-git.juniper.net/contrail/cn2/build/kernel_downloader && go build -o /kernel-downloader main.go

ARG REGISTRY=svl-artifactory.juniper.net/atom-docker/cn2
ARG TAG=distroless
FROM ${REGISTRY}/vrouter-binaries-builder:${TAG} AS binaries

# Download the kernel sources
ADD kernellist.yaml /
ARG ARTIFACTORY_KERNEL_CACHE
COPY --from=build /kernel-downloader .
RUN mkdir /results
RUN --mount=type=cache,id=ccache,target=/root/.ccache \
    echo "$ARTIFACTORY_KERNEL_CACHE" && \
    /kernel-downloader -config /kernellist.yaml -format table \
    -loglevel debug \
    -format csv,/results/kernels.csv \
    -format yaml,/results/kernels.yaml \
    -format json,/results/kernels.json && \
    ccache --show-stats && \
    test -f /results/kernels.csv

