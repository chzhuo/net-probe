package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	labels = []string{
		"src", "dst",
	}
	timeBuckets     = []float64{100, 200, 500, 1000, 2000, 5000, 10000, 20000, 40000, 60000}
	bodyDurationVec = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "namiprobe",
		Name:      "body_time",
		Buckets:   timeBuckets,
	},
		labels)
	headerDurationVec = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "namiprobe",
		Name:      "header_time",
		Buckets:   timeBuckets,
	},
		labels)
	requestCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "namiprobe",
		Name:      "request_count",
	}, []string{
		"src", "dst", "status",
	})
)

var fetchMetricMetux = sync.Mutex{}
var lastFechTime = time.Now()

var timeout int
var localhost string

func httpProbe(ctx context.Context, method string, host string, uri string) {
	url := path.Join(host, uri)
	url = "http://" + url
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		panic(err)
	}
	start := time.Now()
	res, err := http.DefaultClient.Do(req)
	headerTime := time.Now().Sub(start)

	statusCode := 0
	errMessage := ""
	if err != nil {
		errMessage = err.Error()
	} else {
		io.Copy(io.Discard, res.Body)
		statusCode = res.StatusCode
		defer res.Body.Close()
	}
	bodyTime := time.Now().Sub(start)

	headerDu := headerTime / time.Millisecond
	bodyDu := bodyTime / time.Millisecond
	headerDurationVec.WithLabelValues(localhost, host).Observe(float64(headerDu))
	bodyDurationVec.WithLabelValues(localhost, host).Observe(float64(bodyDu))
	requestCount.WithLabelValues(localhost, host, strconv.Itoa(statusCode)).Add(1)
	logrus.Infof(`%s %d %d %d "%s"`, host, statusCode, int(headerDu), int(bodyDu), errMessage)
}

func startMetric(listPort int) {
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", listPort), nil)
		if err != nil {
			panic(err)
		}
	}()
}

func main() {
	var port int
	var hostsStr string
	var interval int
	var method, uri string
	hostname, _ := os.Hostname()
	flag.IntVar(&timeout, "timeout", 60, "request timeout")
	flag.IntVar(&port, "port", 19100, "listen port")
	flag.IntVar(&interval, "interval", 2, "probe interval")
	flag.StringVar(&localhost, "", hostname, "localhost")
	flag.StringVar(&hostsStr, "hosts", "", "hosts ','")
	flag.StringVar(&method, "method", "GET", "method")
	flag.StringVar(&uri, "uri", "/health_check", "uri ','")
	flag.Parse()

	customFormatter := new(logrus.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	customFormatter.FullTimestamp = true
	logrus.StandardLogger().SetFormatter(customFormatter)

	startMetric(port)

	hosts := strings.Split(hostsStr, ",")
	ctx, cancel := context.WithCancel(context.Background())
	for _, host := range hosts {
		time.Sleep(time.Millisecond / 300)
		go func(host string) {
			ticker := time.Tick(time.Duration(interval) * time.Second)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker:
					go httpProbe(ctx, method, host, uri)
				}
			}
		}(host)
	}

	http.DefaultClient.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	logrus.Info("received signal to exit")
	cancel()
}
