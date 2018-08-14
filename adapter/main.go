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
	"github.com/prometheus/prometheus/documentation/examples/remote_storage/remote_storage_adapter/influxdb"
	"github.com/prometheus/prometheus/prompb"
	"math/rand"
)

// Database config strings.
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

var (
	receivedSamples = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "received_samples_total",
			Help: "Total number of received samples.",
		},
	)
	sentSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sent_samples_total",
			Help: "Total number of processed samples sent to remote storage.",
		},
		[]string{"remote"},
	)
	failedSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failed_samples_total",
			Help: "Total number of processed samples which failed on send to remote storage.",
		},
		[]string{"remote"},
	)
	sentBatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sent_batch_duration_seconds",
			Help:    "Duration of sample batch send calls to the remote storage.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"remote"},
	)
	channelMap = map[string]chan model.Samples{}
)

func init() {
	prometheus.MustRegister(receivedSamples)
	prometheus.MustRegister(sentSamples)
	prometheus.MustRegister(failedSamples)
	prometheus.MustRegister(sentBatchDuration)
}

// Main function.
func main() {
	// Register the prometheus handler for the /metrics path.
	cfg := parseFlags()
	http.Handle(cfg.telemetryPath, prometheus.Handler())

	// Set log levels for prometheus.
	logLevel := promlog.AllowedLevel{}
	logLevel.Set(cfg.logLevel)
	logger := promlog.New(logLevel)

	// Build the influxDB reader and writer client.
	writer, reader := buildClients(logger, cfg)

	// Serve the writers and readers to listen on host:port.
	serve(logger, cfg.listenAddr, writer, reader)
}

// Read the configuration.
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

// Writer interface. Writes samples to influxDB.
// Has the name string attr.
type writer interface {
	Write(sample model.Samples) error
	Name() string
}

// Reader interface. Reads from InfluxDB.
// name string attr.
type reader interface {
	Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error)
	Name() string
}

func buildClients(logger log.Logger, cfg *config) (writer, reader) {
	var influxWriter writer
	var influxReader reader
	if cfg.influxdbURL != "" {
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
		influxWriter = c
		influxReader = c
	}
	level.Info(logger).Log("msg", "Starting up...")
	return influxWriter, influxReader
}

func serve(logger log.Logger, addr string, writer writer, reader reader) error {
	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		logger.Log("Writing to Influxdb.")
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
		receivedSamples.Add(float64(len(samples)))
		write(channelMap, writer, []model.Samples{samples})
		read(logger, writer, channelMap)

/*		var wg sync.WaitGroup
		for _, w := range writers {
			wg.Add(1)
			go func(rw writer) {
				sendSamples(logger, rw, samples)
				wg.Done()
			}(w)
		}
		wg.Wait()*/
	})

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		logger.Log("Reading from Influxdb.")
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

		// todo: wont change in a running servrr. just panic at servere start.
		// TODO: Support reading from more than one reader and merging the results.
		if reader == nil {
			http.Error(w, fmt.Sprintf("expected exactly one reader, found no readers"), http.StatusInternalServerError)
			return
		}

		var resp *prompb.ReadResponse
		resp, err = reader.Read(&req)
		if err != nil {
			level.Warn(logger).Log("msg", "Error executing query", "query", req, "storage", reader.Name(), "err", err)
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

func sendSample(logger log.Logger, w writer, samples model.Samples) {
	begin := time.Now()
	err := w.Write(samples)
	duration := time.Since(begin).Seconds()
	if err != nil {
		level.Warn(logger).Log("msg", "Error sending samples to remote storage", "err", err, "storage", w.Name(), "num_samples", 1)
		failedSamples.WithLabelValues(w.Name()).Add(1)
	}
	sentSamples.WithLabelValues(w.Name()).Add(1)
	sentBatchDuration.WithLabelValues(w.Name()).Observe(duration)
}

func fillUp(messages []model.Samples) chan model.Samples{
	channel := make(chan model.Samples)

	go func(messages []model.Samples) {
		defer close(channel)
		sleepNumber := time.Millisecond * time.Duration(10*rand.Float64())
		for _, v := range messages {
			channel <- v
			time.Sleep(sleepNumber)
		}

	}(messages)

	return channel
}

func write(channelMap map[string]chan model.Samples, w writer, samples []model.Samples) {
	channelMap[w.Name()] = fillUp(samples)
}

func read(logger log.Logger, w writer, channelMap map[string]chan model.Samples) {
	count := 0
	for {
		if len(channelMap) < 1 {
			break
		}

		ch := channelMap[w.Name()]
		select {
		case samples, stillOpen := <-ch:
			if !stillOpen {
				delete(channelMap, w.Name())
			} else {
				sendSample(logger, w, samples)
				count += 1
			}
		default:
		}
	}

	fmt.Println(count)
}
