/*
Copyright 2021 Loggie Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package filewatcher

import (
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus"
	"loggie.io/loggie/pkg/core/api"
	"loggie.io/loggie/pkg/eventbus"
	"loggie.io/loggie/pkg/eventbus/export/logger"
	promeExporter "loggie.io/loggie/pkg/eventbus/export/prometheus"
	"loggie.io/loggie/pkg/util"
	"strings"
	"time"
)

func init() {
	eventbus.Registry(makeListener(), eventbus.WithTopics([]string{eventbus.FileWatcherTopic}))
}

func makeListener() *Listener {
	l := &Listener{
		done:   make(chan struct{}),
		data:   make(map[string]data),
		config: &Config{},
	}
	return l
}

type Config struct {
	Period            time.Duration `yaml:"period" default:"5m"`
	UnFinishedTimeout time.Duration `yaml:"checkUnFinishedTimeout" default:"24h"`
}

type Listener struct {
	config *Config
	done   chan struct{}

	data map[string]data // key=pipelineName+sourceName
}

type data struct {
	PipelineName string `json:"pipeline"`
	SourceName   string `json:"source"`

	FileInfo []*fileInfo `json:"info,omitempty"` // key=fileName

	TotalFileCount  int `json:"total"`
	InactiveFdCount int `json:"inactive"`
}

type fileInfo struct {
	FileName       string    `json:"name"`
	FileSize       int64     `json:"size"` // It will not be brought when reporting. It is obtained by directly using OS. Stat (filename). Size() on the consumer side
	AckOffset      int64     `json:"ackOffset"`
	LastModifyTime time.Time `json:"modify"`
	IgnoreOlder    bool      `json:"ignoreOlder"`
}

func (l *Listener) Name() string {
	return "filewatcher"
}

func (l *Listener) Init(ctx api.Context) {
}

func (l *Listener) Start() {
	go l.export()
}

func (l *Listener) Stop() {
	close(l.done)
}

func (l *Listener) Config() interface{} {
	return l.config
}

func (l *Listener) Subscribe(event eventbus.Event) {

	e := event.Data.(eventbus.WatchMetricData)
	var buf strings.Builder
	buf.WriteString(e.PipelineName)
	buf.WriteString("-")
	buf.WriteString(e.SourceName)
	key := buf.String()

	m := data{
		PipelineName: e.PipelineName,
		SourceName:   e.SourceName,
	}

	var files []*fileInfo
	for _, fi := range e.FileInfos {
		f := &fileInfo{
			FileName:       fi.FileName,
			FileSize:       fi.Size,
			AckOffset:      fi.Offset,
			LastModifyTime: fi.LastModifyTime,
			IgnoreOlder:    fi.IsIgnoreOlder,
		}
		files = append(files, f)
	}
	m.FileInfo = files
	m.TotalFileCount = e.TotalFileCount
	m.InactiveFdCount = e.InactiveFdCount

	l.data[key] = m
}

func (l *Listener) exportPrometheus() {
	m := promeExporter.ExportedMetrics{}
	const FileNameKey = "filename"
	const FileStatusKey = "status"
	for _, d := range l.data {
		m1 := promeExporter.ExportedMetrics{
			{
				Desc: prometheus.NewDesc(
					prometheus.BuildFQName(promeExporter.Loggie, eventbus.FileWatcherTopic, "total_file_count"),
					"file count total",
					nil, prometheus.Labels{promeExporter.PipelineNameKey: d.PipelineName, promeExporter.SourceNameKey: d.SourceName},
				),
				Eval:    float64(d.TotalFileCount),
				ValType: prometheus.GaugeValue,
			},
			{
				Desc: prometheus.NewDesc(
					prometheus.BuildFQName(promeExporter.Loggie, eventbus.FileWatcherTopic, "inactive_file_count"),
					"inactive file count",
					nil, prometheus.Labels{promeExporter.PipelineNameKey: d.PipelineName, promeExporter.SourceNameKey: d.SourceName},
				),
				Eval:    float64(d.InactiveFdCount),
				ValType: prometheus.GaugeValue,
			},
		}
		for _, info := range d.FileInfo {
			status := "pending"
			if time.Since(info.LastModifyTime) > l.config.UnFinishedTimeout && util.Abs(info.FileSize-info.AckOffset) >= 1 {
				status = "unfinished"
			}
			if info.IgnoreOlder {
				status = "ignored"
			}

			m2 := promeExporter.ExportedMetrics{
				{
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName(promeExporter.Loggie, eventbus.FileWatcherTopic, "file_size"),
						"file size",
						nil, prometheus.Labels{promeExporter.PipelineNameKey: d.PipelineName, promeExporter.SourceNameKey: d.SourceName,
							FileNameKey: info.FileName, FileStatusKey: status},
					),
					Eval:    float64(info.FileSize),
					ValType: prometheus.GaugeValue,
				},
				{
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName(promeExporter.Loggie, eventbus.FileWatcherTopic, "file_ack_offset"),
						"file ack offset",
						nil, prometheus.Labels{promeExporter.PipelineNameKey: d.PipelineName, promeExporter.SourceNameKey: d.SourceName,
							FileNameKey: info.FileName, FileStatusKey: status},
					),
					Eval:    float64(info.AckOffset),
					ValType: prometheus.GaugeValue,
				},
				{
					Desc: prometheus.NewDesc(
						prometheus.BuildFQName(promeExporter.Loggie, eventbus.FileWatcherTopic, "file_last_modify"),
						"file last modify timestamp",
						nil, prometheus.Labels{promeExporter.PipelineNameKey: d.PipelineName, promeExporter.SourceNameKey: d.SourceName,
							FileNameKey: info.FileName, FileStatusKey: status},
					),
					Eval:    float64(info.LastModifyTime.UnixNano() / 1e6),
					ValType: prometheus.GaugeValue,
				},
			}

			m1 = append(m1, m2...)
		}

		m = append(m, m1...)
	}
	promeExporter.Export(eventbus.FileWatcherTopic, m)
}

func (l *Listener) clean() {
	for k := range l.data {
		delete(l.data, k)
	}
}

func (l *Listener) export() {
	tick := time.Tick(l.config.Period)
	for {
		select {
		case <-l.done:
			return
		case <-tick:
			l.exportPrometheus()

			m, _ := json.Marshal(l.data)
			logger.Export(eventbus.FileWatcherTopic, m)

			l.clean()
		}
	}
}