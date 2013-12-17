package main

import (
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/influxdb/influxdb-go"
	"io/ioutil"
	"math/rand"
	"os"
	"runtime"
	"time"
)

type benchmarkConfig struct {
	OutputAfterCount   int                `toml:"output_after_count"`
	LogFile            string             `toml:"log_file"`
	StatsServer        statsServer        `toml:"stats_server"`
	Servers            []server           `toml:"servers"`
	ClusterCredentials clusterCredentials `toml:"cluster_credentials"`
	LoadSettings       loadSettings       `toml:"load_settings"`
	LoadDefinitions    []loadDefinition   `toml:"load_definitions"`
	Log                *os.File
}

type statsServer struct {
	ConnectionString string `toml:"connection_string"`
	User             string `toml:"user"`
	Password         string `toml:"password"`
	Database         string `toml:"database"`
}

type clusterCredentials struct {
	Database string `toml:"database"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type server struct {
	ConnectionString string `toml:"connection_string"`
}

type loadSettings struct {
	ConcurrentConnections int `toml:"concurrent_connections"`
	RunPerLoadDefinition  int `toml:"runs_per_load_definition"`
}

type loadDefinition struct {
	Name                   string         `toml:"name"`
	ReportSamplingInterval int            `toml:"report_sampling_interval"`
	Percentiles            []float64      `toml:"percentiles"`
	PercentileTimeInterval string         `toml:"percentile_time_interval"`
	BaseSeriesName         string         `toml:"base_series_name"`
	SeriesCount            int            `toml:"series_count"`
	WriteSettings          writeSettings  `toml:"write_settings"`
	IntColumns             []intColumn    `toml:"int_columns"`
	StringColumns          []stringColumn `toml:"string_columns"`
	FloatColumns           []floatColumn  `toml:"float_columns"`
	BoolColumns            []boolColumn   `toml:"bool_columns"`
	Queries                []query        `toml:"queries"`
	ReportSampling         int            `toml:"report_sampling"`
}

type writeSettings struct {
	BatchSeriesSize   int    `toml:"batch_series_size"`
	BatchPointsSize   int    `toml:"batch_points_size"`
	DelayBetweenPosts string `toml:"delay_between_posts"`
}

type query struct {
	Name         string `toml:"name"`
	FullQuery    string `toml:"full_query"`
	QueryStart   string `toml:"query_start"`
	QueryEnd     string `toml:"query_end"`
	PerformEvery string `toml:"perform_every"`
}

type intColumn struct {
	Name     string `toml:"name"`
	MinValue int    `toml:"min_value"`
	MaxValue int    `toml:"max_value"`
}

type floatColumn struct {
	Name     string  `toml:"name"`
	MinValue float64 `toml:"min_value"`
	MaxValue float64 `toml:"max_value"`
}

type boolColumn struct {
	Name string `toml:"name"`
}

type stringColumn struct {
	Name         string   `toml:"name"`
	Values       []string `toml:"values"`
	RandomLength int      `toml:"random_length"`
}

func main() {
	configFile := flag.String("config", "benchmark_config.sample.toml", "Config file")

	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	data, err := ioutil.ReadFile(*configFile)
	if err != nil {
		panic(err)
	}
	var conf benchmarkConfig
	if _, err := toml.Decode(string(data), &conf); err != nil {
		panic(err)
	}
	logFile, err := os.OpenFile(conf.LogFile, os.O_RDWR|os.O_CREATE, 0660)
	if err != nil {
		panic(fmt.Sprintf("Error opening log file \"%s\": %s", conf.LogFile, err))
	}
	conf.Log = logFile
	defer logFile.Close()
	fmt.Println("Logging benchmark results to ", conf.LogFile)
	logFile.WriteString("Starting benchmark run...\n")

	harness := NewBenchmarkHarness(&conf)

	startTime := time.Now()
	harness.Run()
	elapsed := time.Now().Sub(startTime)

	fmt.Printf("Finished in %.3f seconds\n", elapsed.Seconds())
}

type BenchmarkHarness struct {
	Config          *benchmarkConfig
	writes          chan *LoadWrite
	requestPending  chan bool
	requestComplete chan bool
	done            chan bool
	success         chan *successResult
	failure         chan *failureResult
}

type successResult struct {
	write        *LoadWrite
	microseconds int64
}

type failureResult struct {
	write        *LoadWrite
	err          error
	microseconds int64
}

type LoadWrite struct {
	LoadDefinition *loadDefinition
	Series         []*influxdb.Series
}

const MAX_SUCCESS_REPORTS_TO_QUEUE = 1000

func NewBenchmarkHarness(conf *benchmarkConfig) *BenchmarkHarness {
	harness := &BenchmarkHarness{
		Config:          conf,
		requestPending:  make(chan bool),
		requestComplete: make(chan bool),
		done:            make(chan bool),
		success:         make(chan *successResult, MAX_SUCCESS_REPORTS_TO_QUEUE),
		failure:         make(chan *failureResult, 1000)}
	go harness.trackRunningRequests()
	harness.startPostWorkers()
	go harness.reportResults()
	return harness
}

func (self *BenchmarkHarness) Run() {
	for _, loadDef := range self.Config.LoadDefinitions {
		go func() {
			self.runLoadDefinition(&loadDef)
		}()
	}
	self.waitForCompletion()
}

func (self *BenchmarkHarness) startPostWorkers() {
	self.writes = make(chan *LoadWrite)
	for i := 0; i < self.Config.LoadSettings.ConcurrentConnections; i++ {
		for _, s := range self.Config.Servers {
			go self.handleWrites(&s)
		}
	}
}

func (self *BenchmarkHarness) reportClient() *influxdb.Client {
	clientConfig := &influxdb.ClientConfig{
		Host:     self.Config.StatsServer.ConnectionString,
		Database: self.Config.StatsServer.Database,
		Username: self.Config.StatsServer.User,
		Password: self.Config.StatsServer.Password}
	client, _ := influxdb.NewClient(clientConfig)
	return client
}

func (self *BenchmarkHarness) reportResults() {
	client := self.reportClient()

	successColumns := []string{"response_time", "point_count", "series_count"}
	failureColumns := []string{"response_time", "err"}

	startTime := time.Now()
	lastReport := time.Now()
	totalPointCount := 0
	lastReportPointCount := 0
	for {
		select {
		case res := <-self.success:
			pointCount := 0
			seriesCount := len(res.write.Series)
			for _, s := range res.write.Series {
				pointCount += len(s.Points)
			}
			totalPointCount += pointCount
			postedSinceLastReport := totalPointCount - lastReportPointCount
			if postedSinceLastReport > self.Config.OutputAfterCount {
				now := time.Now()
				totalPerSecond := float64(totalPointCount) / now.Sub(startTime).Seconds()
				runPerSecond := float64(postedSinceLastReport) / now.Sub(lastReport).Seconds()
				fmt.Printf("This Interval: %d points. %.0f per second. Run Total: %d points. %.0f per second.\n",
					postedSinceLastReport,
					runPerSecond,
					totalPointCount,
					totalPerSecond)
				lastReport = now
				lastReportPointCount = totalPointCount
			}

			s := &influxdb.Series{
				Name:    res.write.LoadDefinition.Name + ".ok",
				Columns: successColumns,
				Points:  [][]interface{}{{res.microseconds / 1000, pointCount, seriesCount}}}
			client.WriteSeries([]*influxdb.Series{s})

			self.requestComplete <- true
		case res := <-self.failure:
			s := &influxdb.Series{
				Name:    res.write.LoadDefinition.Name + ".ok",
				Columns: failureColumns,
				Points:  [][]interface{}{{res.microseconds / 1000, res.err}}}
			client.WriteSeries([]*influxdb.Series{s})
			self.requestComplete <- true
		}
	}
}

func (self *BenchmarkHarness) waitForCompletion() {
	<-self.done
}

func (self *BenchmarkHarness) trackRunningRequests() {
	count := 0
	for {
		select {
		case <-self.requestPending:
			count += 1
		case <-self.requestComplete:
			count -= 1
			if count == 0 {
				self.done <- true
				return
			}
		}
	}
}

func (self *BenchmarkHarness) runLoadDefinition(loadDef *loadDefinition) {
	seriesNames := make([]string, loadDef.SeriesCount, loadDef.SeriesCount)
	for i := 0; i < loadDef.SeriesCount; i++ {
		seriesNames[i] = fmt.Sprintf("%s_%d", loadDef.BaseSeriesName, i)
	}
	columnCount := len(loadDef.IntColumns) + len(loadDef.BoolColumns) + len(loadDef.FloatColumns) + len(loadDef.StringColumns)
	columns := make([]string, 0, columnCount)
	for _, col := range loadDef.IntColumns {
		columns = append(columns, col.Name)
	}
	for _, col := range loadDef.BoolColumns {
		columns = append(columns, col.Name)
	}
	for _, col := range loadDef.FloatColumns {
		columns = append(columns, col.Name)
	}
	for _, col := range loadDef.StringColumns {
		columns = append(columns, col.Name)
	}

	for _, q := range loadDef.Queries {
		go self.runQuery(loadDef, seriesNames, &q)
	}

	requestCount := self.Config.LoadSettings.RunPerLoadDefinition

	if requestCount != 0 {
		for i := 0; i < requestCount; i++ {
			self.runLoad(seriesNames, columns, loadDef)
		}
		return
	} else {
		// run forever
		for {
			self.runLoad(seriesNames, columns, loadDef)
		}
	}
}

func (self *BenchmarkHarness) runLoad(seriesNames []string, columns []string, loadDef *loadDefinition) {
	columnCount := len(columns)
	sleepTime, shouldSleep := time.ParseDuration(loadDef.WriteSettings.DelayBetweenPosts)

	pointsPosted := 0
	for j := 0; j < len(seriesNames); j += loadDef.WriteSettings.BatchSeriesSize {
		names := seriesNames[j : j+loadDef.WriteSettings.BatchSeriesSize]
		seriesToPost := make([]*influxdb.Series, len(names), len(names))
		for ind, name := range names {
			s := &influxdb.Series{Name: name, Columns: columns, Points: make([][]interface{}, loadDef.WriteSettings.BatchPointsSize, loadDef.WriteSettings.BatchPointsSize)}
			for pointCount := 0; pointCount < loadDef.WriteSettings.BatchPointsSize; pointCount++ {
				pointsPosted++
				point := make([]interface{}, 0, columnCount)
				for _, col := range loadDef.IntColumns {
					point = append(point, rand.Intn(col.MaxValue))
				}
				for n := 0; n < len(loadDef.BoolColumns); n++ {
					point = append(point, rand.Intn(2) == 0)
				}
				for n := 0; n < len(loadDef.FloatColumns); n++ {
					point = append(point, rand.Float64())
				}
				for _, col := range loadDef.StringColumns {
					point = append(point, col.Values[rand.Intn(len(col.Values))])
				}

				s.Points[pointCount] = point
			}
			seriesToPost[ind] = s
		}
		self.writes <- &LoadWrite{LoadDefinition: loadDef, Series: seriesToPost}
	}
	if shouldSleep == nil {
		time.Sleep(sleepTime)
	}
}

func (self *BenchmarkHarness) runQuery(loadDef *loadDefinition, seriesNames []string, q *query) {
	sleepTime, err := time.ParseDuration(q.PerformEvery)
	if err != nil {
		panic("Queries must have a perform_every value. Couldn't parse " + q.PerformEvery)
	}
	for {
		if q.FullQuery != "" {
			go self.queryAndReport(loadDef, q, q.FullQuery)
		} else {
			for _, name := range seriesNames {
				go self.queryAndReport(loadDef, q, q.QueryStart+" "+name+" "+q.QueryEnd)
			}
		}
		time.Sleep(sleepTime)
	}
}

func (self *BenchmarkHarness) queryAndReport(loadDef *loadDefinition, q *query, queryString string) {
}

func (self *BenchmarkHarness) handleWrites(s *server) {
	clientConfig := &influxdb.ClientConfig{
		Host:     s.ConnectionString,
		Database: self.Config.ClusterCredentials.Database,
		Username: self.Config.ClusterCredentials.User,
		Password: self.Config.ClusterCredentials.Password}
	client, err := influxdb.NewClient(clientConfig)
	if err != nil {
		panic(fmt.Sprintf("Error connecting to server \"%s\": %s", s.ConnectionString, err))
	}
	for {
		write := <-self.writes

		self.requestPending <- true
		startTime := time.Now()
		err := client.WriteSeries(write.Series)
		microsecondsTaken := time.Now().Sub(startTime).Nanoseconds() / 1000

		if err != nil {
			self.reportFailure(&failureResult{write: write, err: err, microseconds: microsecondsTaken})
		} else {
			self.reportSuccess(&successResult{write: write, microseconds: microsecondsTaken})
		}
		self.requestComplete <- true
	}
}

func (self *BenchmarkHarness) reportSuccess(success *successResult) {
	if len(self.success) == MAX_SUCCESS_REPORTS_TO_QUEUE {
		fmt.Println("Success reporting queue backed up. Dropping report.")
		return
	}
	self.success <- success
	self.requestPending <- true
}

func (self *BenchmarkHarness) reportFailure(failure *failureResult) {
	self.failure <- failure
	self.requestPending <- true
}