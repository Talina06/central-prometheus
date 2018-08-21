package database

import (
	"testing"
	"os"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"fmt"
)

func TestConnect(t *testing.T) {
	os.Setenv("DB_HOST", "localhost")
	os.Setenv("DB_PORT", "5433")
	client := NewClient()

	t.Run("Should successfully connect to the postgres database", func(t *testing.T) {
		if err := client.db.Ping(); err != nil {
			t.Fatalf("Could not connect to the database. %v", err)
		}
	})

	t.Run("Should setup the database successfully.", func(t *testing.T) {
		if err := client.setup(); err != nil {
			t.Fatalf("Could not setup the database extensions. %v", err)
		}
	})

	t.Run("Should write samples to the database.", func(t *testing.T) {
		samples := []*model.Sample{
			{
				Metric: model.Metric{
					model.MetricNameLabel: "test_metric",
					"label1":              "1",
				},
				Value:     123.1,
				Timestamp: 1234567,
			},
			{
				Metric: model.Metric{
					model.MetricNameLabel: "test_metric",
					"label1":              "1",
				},
				Value:     123.2,
				Timestamp: 1234568,
			},
			{
				Metric: model.Metric{
					model.MetricNameLabel: "test_metric",
				},
				Value:     123.2,
				Timestamp: 1234569,
			},
			{
				Metric: model.Metric{
					model.MetricNameLabel: "test_metric_2",
					"label1":              "1",
				},
				Value:     123.4,
				Timestamp: 1234570,
			},
		}

		if err := client.Write(samples); err != nil {
			t.Fatalf("could not write %v", err)
		}

		var cnt int
		err := client.db.QueryRow("SELECT count(*) FROM metrics").Scan(&cnt)
		if err != nil {
			t.Fatal(err)
		}

		if cnt == 0 {
			t.Fatal("Write did not happen")
		}

	})

	t.Run("Should read samples from DB", func(t *testing.T) {
		r := &prompb.ReadRequest{}
		resp, err := client.Read(r)
		if err != nil {
			t.Fatalf("error while reading from DB %v", err)
		}

		fmt.Println(resp.Results)
	})
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
