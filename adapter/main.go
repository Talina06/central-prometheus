// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The main package for the Prometheus server executable.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	influx "github.com/influxdata/influxdb/client/v2"

	"github.com/prometheus/common/promlog"
	"github.com/prometheus/prometheus/prompb"
	"prom-alertmanager/adapter/client"
	"github.com/meson10/highbrow"
	"os/signal"
)

type config struct {
	influxdbURL             string
	influxdbRetentionPolicy string
	influxdbUsername        string
	influxdbDatabase        string
	influxdbPassword        string
	remoteTimeout           time.Duration
	listenAddr              string
	telemetryPath           string
	logLevel                string
}

const RateLimit = 50

var (
	mChannel = make(chan *model.Samples)
)

func main() {
	cfg := parseFlags()
	http.Handle(cfg.telemetryPath, prometheus.Handler())

	logLevel := promlog.AllowedLevel{}
	logLevel.Set(cfg.logLevel)
	logger := promlog.New(logLevel)

	client := buildClients(logger, cfg)
	serve(logger, cfg.listenAddr, client)
}

func parseFlags() *config {
	cfg := &config{
		influxdbPassword: os.Getenv("INFLUXDB_PW"),
	}

	flag.StringVar(&cfg.influxdbURL, "influxdb-url", "",
		"The URL of the remote InfluxDB server to send samples to. None, if empty.",
	)
	flag.StringVar(&cfg.influxdbRetentionPolicy, "influxdb.retention-policy", "autogen",
		"The InfluxDB retention policy to use.",
	)
	flag.StringVar(&cfg.influxdbUsername, "influxdb.username", "",
		"The username to use when sending samples to InfluxDB. The corresponding password must be provided via the INFLUXDB_PW environment variable.",
	)
	flag.StringVar(&cfg.influxdbDatabase, "influxdb.database", "prometheus",
		"The name of the database to use for storing samples in InfluxDB.",
	)
	flag.DurationVar(&cfg.remoteTimeout, "send-timeout", 30*time.Second,
		"The timeout to use when sending samples to the remote storage.",
	)
	flag.StringVar(&cfg.listenAddr, "web.listen-address", ":9201", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.telemetryPath, "web.telemetry-path", "/metrics", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.logLevel, "log.level", "debug", "Only log messages with the given severity or above. One of: [debug, info, warn, error]")

	flag.Parse()

	return cfg
}

func buildClients(logger log.Logger, cfg *config) (*influxdb.Client) {
	if cfg.influxdbURL == "" {
		return nil
	}

	url, err := url.Parse(cfg.influxdbURL)
	if err != nil {
		level.Error(logger).Log("msg", "Failed to parse InfluxDB URL", "url", cfg.influxdbURL, "err", err)
		os.Exit(1)
	}

	conf := influx.HTTPConfig{
		Addr:     url.String(),
		Username: cfg.influxdbUsername,
		Password: cfg.influxdbPassword,
		Timeout:  cfg.remoteTimeout,
	}
	c := influxdb.NewClient(
		log.With(logger, "storage", "InfluxDB"),
		conf,
		cfg.influxdbDatabase,
		cfg.influxdbRetentionPolicy,
	)
	prometheus.MustRegister(c)

	return c
}

func serve(logger log.Logger, addr string, client *influxdb.Client) error {
	go readFromChannel(makeLimiter(), client, mChannel)

	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			level.Error(logger).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			level.Error(logger).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.WriteRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			level.Error(logger).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		samples := protoToSamples(&req)
		mChannel <- &samples
	})

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			level.Error(logger).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			level.Error(logger).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			level.Error(logger).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// TODO: Support reading from more than one reader and merging the results.
		if client == nil {
			http.Error(w, fmt.Sprintf("expected exactly one reader, found %d readers", 0), http.StatusInternalServerError)
			return
		}

		var resp *prompb.ReadResponse
		resp, err = client.Read(&req)
		if err != nil {
			level.Warn(logger).Log("msg", "Error executing query", "query", req, "storage", client.Name(), "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "snappy")

		compressed = snappy.Encode(nil, data)
		if _, err := w.Write(compressed); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	return http.ListenAndServe(addr, nil)
}

func protoToSamples(req *prompb.WriteRequest) model.Samples {
	var samples model.Samples
	for _, ts := range req.Timeseries {
		metric := make(model.Metric, len(ts.Labels))
		for _, l := range ts.Labels {
			metric[model.LabelName(l.Name)] = model.LabelValue(l.Value)
		}

		for _, s := range ts.Samples {
			samples = append(samples, &model.Sample{
				Metric:    metric,
				Value:     model.SampleValue(s.Value),
				Timestamp: model.Time(s.Timestamp),
			})
		}
	}
	return samples
}

func readFromChannel(l *highbrow.RateLimiter, writer *influxdb.Client, mChannel chan *model.Samples) {
	go handleInterrupts()

	t := l.Start()
	for {
		<-t
		y := <-mChannel
		go writer.Write(*y)
	}

	l.Stop()
}

func handleInterrupts() {
	signalChan := make(chan os.Signal, 10)
	signal.Notify(signalChan, os.Interrupt, os.Kill)

	select {
	case <-signalChan:
		time.Sleep(2 * time.Second)
	}

	os.Exit(0)
}

func makeLimiter() *highbrow.RateLimiter {
	return highbrow.NewLimiter(RateLimit)
}
