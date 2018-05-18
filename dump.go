package main

import (
	"errors"
	"flag"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bzed/go-whisper"
	"github.com/marpaia/graphite-golang"
)

type rateLimiter struct {
	pointsPerSecond int64
	currentPoints   int64
	full            chan bool
	lock            *sync.Mutex
	enabled         bool
}

func newRateLimiter(pointsPerSecond int64) *rateLimiter {
	rl := new(rateLimiter)
	rl.pointsPerSecond = pointsPerSecond
	rl.currentPoints = 0
	rl.full = make(chan bool)
	rl.lock = new(sync.Mutex)
	if pointsPerSecond == 0 {
		rl.enabled = false
	} else {
		rl.enabled = true
		go func() {
			for {
				time.Sleep(1 * time.Second)
				select {
				case <-rl.full:
				default:
				}
			}
		}()
		return rl
	}
	return rl
}

func (rl *rateLimiter) limit(n int64) {
	if !rl.enabled {
		return
	}
	rl.lock.Lock()
	defer rl.lock.Unlock()

	rl.currentPoints += n
	if rl.currentPoints >= rl.pointsPerSecond {
		rl.full <- true
		rl.currentPoints = 0
	}
}

func convertFilename(filename string, baseDirectory string) (string, error) {
	absFilename, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	absBaseDirectory, err := filepath.Abs(baseDirectory)
	if err != nil {
		return "", err
	}
	err = nil
	if strings.HasPrefix(absFilename, absBaseDirectory) {
		metric := strings.Replace(
			strings.TrimPrefix(
				strings.TrimSuffix(
					strings.TrimPrefix(
						absFilename,
						absBaseDirectory),
					".wsp"),
				"/"),
			"/",
			".",
			-1)
		return metric, err
	}
	err = errors.New("Path for whisper file does not live in BasePath")
	return "", err
}

func sendWhisperData(
	filename string,
	baseDirectory string,
	graphiteConn *graphite.Graphite,
	rateLimiter *rateLimiter,
) error {
	metricName, err := convertFilename(filename, baseDirectory)
	if err != nil {
		return err
	}

	whisperData, err := whisper.Open(filename)
	if err != nil {
		return err
	}

	archiveDataPoints, err := whisperData.DumpArchives()
	if err != nil {
		return err
	}
	metrics := make([]graphite.Metric, 0, 1000)
	for _, dataPoint := range archiveDataPoints {
		interval, value := dataPoint.Point()

		if math.IsNaN(value) || interval == 0 {
			continue
		}

		v := strconv.FormatFloat(value, 'f', 20, 64)
		metrics = append(metrics, graphite.NewMetric(metricName, v, int64(interval)))

	}
	rateLimiter.limit(int64(len(metrics)))
	err = graphiteConn.SendMetrics(metrics)
	if err != nil {
		return err
	}
	err = nil
	return err
}

func findWhisperFiles(ch chan string, quit chan int, directory string) {
	visit := func(path string, info os.FileInfo, err error) error {
		if (info != nil) && !info.IsDir() {
			if strings.HasSuffix(path, ".wsp") {
				ch <- path
			}
		}
		return nil
	}
	err := filepath.Walk(directory, visit)
	if err != nil {
		log.Fatal(err)
	}
	close(quit)
}

func worker(ch chan string,
	quit chan int,
	wg *sync.WaitGroup,
	baseDirectory string,
	graphiteHost string,
	graphitePort int,
	graphiteProtocol string,
	rateLimiter *rateLimiter) {

	defer wg.Done()

	graphiteConn, err := graphite.GraphiteFactory(graphiteProtocol, graphiteHost, graphitePort, "")
	if err != nil {
		return
	}

	for {
		select {
		case path := <-ch:
			{

				err := sendWhisperData(path, baseDirectory, graphiteConn, rateLimiter)
				if err != nil {
					log.Println("Failed: " + path)
					log.Println(err)
				} else {
					log.Println("OK: " + path)
				}
			}
		case <-quit:
			return
		}

	}
}

func main() {

	baseDirectory := flag.String(
		"basedirectory",
		"/var/lib/graphite/whisper",
		"Base directory where whisper files are located. Used to retrieve the metric name from the filename.")
	directory := flag.String(
		"directory",
		"/var/lib/graphite/whisper/collectd",
		"Directory containing the whisper files you want to send to graphite again")
	graphiteHost := flag.String(
		"host",
		"127.0.0.1",
		"Hostname/IP of the graphite server")
	graphitePort := flag.Int(
		"port",
		2003,
		"graphite Port")
	graphiteProtocol := flag.String(
		"protocol",
		"tcp",
		"Protocol to use to transfer graphite data (tcp/udp/nop)")
	workers := flag.Int(
		"workers",
		5,
		"Workers to run in parallel")

	pointsPerSecond := flag.Int64(
		"pps",
		0,
		"Number of maximum points per second to send (0 means rate limiter is disabled)")
	flag.Parse()

	if !(*graphiteProtocol == "tcp" ||
		*graphiteProtocol == "udp" ||
		*graphiteProtocol == "nop") {
		log.Fatalln("Graphite protocol " + *graphiteProtocol + " not supported, use tcp/udp/nop.")
	}
	ch := make(chan string)
	quit := make(chan int)
	var wg sync.WaitGroup

	rl := newRateLimiter(*pointsPerSecond)
	wg.Add(*workers)
	for i := 0; i < *workers; i++ {
		go worker(ch, quit, &wg, *baseDirectory, *graphiteHost, *graphitePort, *graphiteProtocol, rl)
	}
	go findWhisperFiles(ch, quit, *directory)
	wg.Wait()
}
