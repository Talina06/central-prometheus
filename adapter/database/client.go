package database

import (
"fmt"

_ "github.com/lib/pq"
	"time"
	"database/sql"
	"strings"
	"github.com/prometheus/common/model"
	"sort"
	"github.com/prometheus/prometheus/prompb"
	"encoding/json"
	"os"
	"reflect"
)

const(
	user                      = "postgres"
	password                  = "password"
	database                  = "postgres"
	schema                    = ""
	sslMode                   = "disable"
	table                     = "metrics"
	copyTable                 = "metrics_copy"
	maxOpenConns              = 50
	maxIdleConns              = 10
	pgPrometheusNormalize     = true
	pgPrometheusLogSamples    = true
	pgPrometheusChunkInterval = time.Hour*12
	useTimescaleDb            = true
	dbConnectRetries          = 10
)

// Client sends Prometheus samples to PostgreSQL
type Client struct {
	db  *sql.DB
}

// while reading the labels from postgres, convert them to prometheus format.
// using this struct.
type sampleLabels struct {
	JSON        []byte
	Map         map[string]string
	OrderedKeys []string
}

func Connect(host, port string) (*sql.DB, error) {
	psqlInfo := fmt.Sprintf("host=%v port=%v user=%v "+
		"password=%v dbname=%v sslmode=disable",
		host, port, user, password, database)

	db, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		return nil, err
	}

	db.SetMaxIdleConns(maxIdleConns)
	db.SetMaxOpenConns(maxOpenConns)

	return db, nil
}

func NewClient() *Client {
	host := os.Getenv("DB_HOST")
	port := os.Getenv("DB_PORT")

	db, err := Connect(host, port)
	if err != nil {
		panic(err)
	}

	return &Client{db: db}
}

// create extension if not exists.
// uses the timeseries prometheus extension for postgres.
func (c *Client) setup() error {
	db := c.db

	tx, err := db.Begin()

	if err != nil {
		panic(err)
	}

	defer tx.Rollback()

	_, err = tx.Exec("CREATE EXTENSION IF NOT EXISTS pg_prometheus")

	if err != nil {
		return err
	}

	if useTimescaleDb {
		_, err = tx.Exec("CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE")
	}
	if err != nil {
		return err
	}

	var rows *sql.Rows
	rows, err = tx.Query("SELECT create_prometheus_table($1, normalized_tables => $2, chunk_time_interval => $3,  use_timescaledb=> $4)",
		table, pgPrometheusNormalize, pgPrometheusChunkInterval.String(), useTimescaleDb)

	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	rows.Close()

	err = tx.Commit()

	if err != nil {
		return err
	}

	return nil
}

// Write implements the Writer interface and writes metric samples to the database
func (c *Client) Write(samples model.Samples) error {
	db := c.db
	tx, err := db.Begin()

	if err != nil {
		return err
	}

	defer tx.Rollback()

	stmt, err := tx.Prepare(fmt.Sprintf("COPY \"%s\" FROM STDIN", copyTable))

	if err != nil {
		return err
	}

	for _, sample := range samples {
		milliseconds := sample.Timestamp.UnixNano() / 1000000
		line := fmt.Sprintf("%v %v %v", metricString(sample.Metric), sample.Value, milliseconds)

		if pgPrometheusLogSamples {
			fmt.Println(line)
		}

		_, err = stmt.Exec(line)
		if err != nil {
			return err
		}
	}

	_, err = stmt.Exec()
	if err != nil {
		return err
	}

	err = stmt.Close()

	if err != nil {
		return err
	}

	err = tx.Commit()

	if err != nil {
		return err
	}

	return nil
}

// Read implements the Reader interface and reads metrics samples from the database
func (c *Client) Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error) {
	labelsToSeries := map[string]*prompb.TimeSeries{}

	for _, q := range req.Queries {
		command, err := c.buildCommand(q)

		if err != nil {
			return nil, err
		}

		rows, err := c.db.Query(command)

		if err != nil {
			return nil, err
		}

		defer rows.Close()

		for rows.Next() {
			var (
				value  float64
				name   string
				labels sampleLabels
				time   time.Time
			)

			err := rows.Scan(&time, &name, &value, &labels)

			if err != nil {
				return nil, err
			}

			key := labels.key(name)
			ts, ok := labelsToSeries[key]

			if !ok {
				labelPairs := make([]*prompb.Label, 0, labels.len()+1)
				labelPairs = append(labelPairs, &prompb.Label{
					Name:  model.MetricNameLabel,
					Value: name,
				})

				for _, k := range labels.OrderedKeys {
					labelPairs = append(labelPairs, &prompb.Label{
						Name:  k,
						Value: labels.Map[k],
					})
				}

				ts = &prompb.TimeSeries{
					Labels:  labelPairs,
					Samples: make([]*prompb.Sample, 0, 100),
				}
				labelsToSeries[key] = ts
			}

			ts.Samples = append(ts.Samples, &prompb.Sample{
				Timestamp: time.UnixNano() / 1000000,
				Value:     value,
			})
		}

		err = rows.Err()

		if err != nil {
			return nil, err
		}
	}

	resp := prompb.ReadResponse{
		Results: []*prompb.QueryResult{
			{
				Timeseries: make([]*prompb.TimeSeries, 0, len(labelsToSeries)),
			},
		},
	}

	for _, ts := range labelsToSeries {
		resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, ts)
		fmt.Println(resp.Results)
	}

	return &resp, nil
}

func (c *Client) buildQuery(q *prompb.Query) (string, error) {
	matchers := make([]string, 0, len(q.Matchers))
	labelEqualPredicates := make(map[string]string)

	for _, m := range q.Matchers {
		escapedValue := escapeValue(m.Value)

		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					matchers = append(matchers, fmt.Sprintf("(name IS NULL OR name = '')"))
				} else {
					matchers = append(matchers, fmt.Sprintf("name = '%s'", escapedValue))
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("name != '%s'", escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("name ~ '%s'", anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("name !~ '%s'", anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown metric name match type %v", m.Type)
			}
		} else {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					// From the PromQL docs: "Label matchers that match
					// empty label values also select all time series that
					// do not have the specific label set at all."
					matchers = append(matchers, fmt.Sprintf("((labels ? '%s') = false OR (labels->>'%s' = ''))",
						m.Name, m.Name))
				} else {
					labelEqualPredicates[m.Name] = m.Value
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' != '%s'", m.Name, escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' ~ '%s'", m.Name, anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' !~ '%s'", m.Name, anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown match type %v", m.Type)
			}
		}
	}
	equalsPredicate := ""

	if len(labelEqualPredicates) > 0 {
		labelsJSON, err := json.Marshal(labelEqualPredicates)

		if err != nil {
			return "", err
		}
		equalsPredicate = fmt.Sprintf(" AND labels @> '%s'", labelsJSON)
	}

	matchers = append(matchers, fmt.Sprintf("EXTRACT (EPOCH FROM time) >= '%v'", q.StartTimestampMs/1000))
	matchers = append(matchers, fmt.Sprintf("EXTRACT( EPOCH FROM time) <= '%v'", q.EndTimestampMs/1000 ))

	query := fmt.Sprintf("SELECT time, name, value, labels FROM %s WHERE %s %s",
		table, strings.Join(matchers, " AND "), equalsPredicate)

	fmt.Println(query)

	return query, nil
}


func (c *Client) buildCommand(q *prompb.Query) (string, error) {
	return c.buildQuery(q)
}

func (c *Client) Ping() error {
	return c.db.Ping()
}

func metricString(m model.Metric) string {
	metricName, hasName := m[model.MetricNameLabel]
	numLabels := len(m) - 1
	if !hasName {
		numLabels = len(m)
	}
	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		if label != model.MetricNameLabel {
			labelStrings = append(labelStrings, fmt.Sprintf("%s=%q", label, value))
		}
	}

	switch numLabels {
	case 0:
		if hasName {
			return string(metricName)
		}
		return "{}"
	default:
		sort.Strings(labelStrings)
		return fmt.Sprintf("%s{%s}", metricName, strings.Join(labelStrings, ","))
	}
}


func escapeValue(str string) string {
	return strings.Replace(str, `'`, `\'`, -1)
}

// anchorValue adds anchors to values in regexps since PromQL docs
// states that "Regex-matches are fully anchored."
func anchorValue(str string) string {
	l := len(str)

	if l == 0 || (str[0] == '^' && str[l-1] == '$') {
		return str
	}

	if str[0] == '^' {
		return fmt.Sprintf("%s$", str)
	}

	if str[l-1] == '$' {
		return fmt.Sprintf("^%s", str)
	}

	return fmt.Sprintf("^%s$", str)
}
func createOrderedKeys(m *map[string]string) []string {
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (l *sampleLabels) Scan(value interface{}) error {
	if value == nil {
		l = &sampleLabels{}
		return nil
	}

	switch t := value.(type) {
	case []uint8:
		m := make(map[string]string)
		err := json.Unmarshal(t, &m)

		if err != nil {
			return err
		}

		*l = sampleLabels{
			JSON:        t,
			Map:         m,
			OrderedKeys: createOrderedKeys(&m),
		}
		return nil
	}
	return fmt.Errorf("Invalid labels value %s", reflect.TypeOf(value))
}

func (l sampleLabels) String() string {
	return string(l.JSON)
}

func (l *sampleLabels) key(extra string) string {
	// 0xff cannot cannot occur in valid UTF-8 sequences, so use it
	// as a separator here.
	separator := "\xff"
	pairs := make([]string, 0, len(l.Map)+1)
	pairs = append(pairs, extra+separator)

	for _, k := range l.OrderedKeys {
		pairs = append(pairs, k+separator+l.Map[k])
	}
	return strings.Join(pairs, separator)
}

func (l *sampleLabels) len() int {
	return len(l.OrderedKeys)
}
