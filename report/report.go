package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"gopkg.in/yaml.v3"
	"ssd-git.juniper.net/contrail/cn2/build/kernel_downloader/distribution"
)

type Result struct {
	Start   time.Time
	End     time.Time
	Kernels []*distribution.Kernel
}

func (r Result) JsonReport() (string, error) {
	data, err := json.MarshalIndent(&r, "", "    ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r Result) YamlReport() (string, error) {
	data, err := yaml.Marshal(&r)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r Result) CsvReport() (string, error) {
	v := strings.Builder{}
	for _, kernel := range r.Kernels {
		v.WriteString(fmt.Sprintf("%s,%t\n", kernel.Name+kernel.LocalVersion, kernel.Compiled))
	}
	return v.String(), nil
}

func (r Result) TableReport() (string, error) {
	t := table.NewWriter()
	t.AppendHeader(table.Row{"Distribution", "DistroVersion", "Kernel", "Success"})
	t.SortBy([]table.SortBy{
		{Name: "Distribution", Mode: table.Asc},
		{Name: "DistroVersion", Mode: table.Dsc},
		{Name: "Kernel", Mode: table.Dsc},
	})
	rowConfigAutoMerge := table.RowConfig{AutoMerge: true}
	for _, kernel := range r.Kernels {
		if kernel.Distro == distribution.MINIKUBE {
			for _, mkVersion := range kernel.MinikubeVersions {
				t.AppendRow(table.Row{kernel.Distro, mkVersion, kernel.Name, kernel.Compiled}, rowConfigAutoMerge)
			}
		} else {
			t.AppendRow(table.Row{kernel.Distro, kernel.DistroVersion, kernel.Name + kernel.LocalVersion, kernel.Compiled}, rowConfigAutoMerge)
		}
	}
	t.SetAutoIndex(true)
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, AutoMerge: true},
		{Number: 2, AutoMerge: true},
		{Number: 3, AutoMerge: true},
	})
	elapsed := r.End.Sub(r.Start)
	t.AppendFooter(table.Row{"Time", elapsed, "Kernel Modules", len(r.Kernels)})

	return t.Render(), nil
}
