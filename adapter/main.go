package main

import (
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/prompb"
	"gopkg.in/alecthomas/kingpin.v2"
	"github.com/meson10/highbrow"
	"os/signal"
	"fmt"

	"prom-alertmanager/adapter/database"
)

const (
	Version = "0.1.0"
	RateLimit = 40
)

var (
	timeout = kingpin.Flag("timeout", "Timeout for writing to remote storage").Default("10000").String()
	address = kingpin.Flag("address", "Listen to address").Default(":9201").String()
	config = kingpin.Flag("config-path", "Config path of prometheus.").String()
	logLevel = kingpin.Flag("log-level", "Level of logging to enable.").Default("debug").String()
	readOnly = kingpin.Flag("readOnly", "If the app is to only read from the remote storage").
		Default("false").Bool()
	mChannel = make(chan *model.Samples)
)

func main() {
	kingpin.Version(Version)
	kingpin.Parse()

	client := buildClient()
	if err := client.Ping(); err != nil {
		panic(err)
	}

	serve(*address , client)
}

func buildClient() (*database.Client) {
	pgClient := database.NewClient()
	return pgClient
}

func serve(addr string, client *database.Client) error {
	go readFromChannel(makeLimiter(), client, mChannel)

	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.WriteRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		samples := protoToSamples(&req)
		mChannel <- &samples
	})

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
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

func readFromChannel(l *highbrow.RateLimiter, writer *database.Client, mChannel chan *model.Samples) {
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

